package node

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner 记录被调用的命令，并按序返回预置结果。
// 不覆写被测方法本身——只替换最外层的进程执行，保证业务逻辑真的被跑到。
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	// replies 按「命令包含某子串」匹配，返回 (输出, 错误)
	replies []reply
}

type reply struct {
	match string
	out   string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	full := append([]string{name}, args...)
	f.calls = append(f.calls, full)
	joined := strings.Join(full, " ")
	for _, r := range f.replies {
		if strings.Contains(joined, r.match) {
			return r.out, r.err
		}
	}
	return "", nil
}

func (f *fakeRunner) lastMatching(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if strings.Contains(strings.Join(f.calls[i], " "), sub) {
			return f.calls[i]
		}
	}
	return nil
}

func testNode(r Runner) *Node {
	return &Node{
		Name: "3588-a", DockerHost: "ssh://sa@192.168.14.54", ADBHost: "192.168.14.54",
		Image: "redroid/redroid:14.0.0_64only-latest", DataRoot: "/data/droidpool",
		BootArgs: "androidboot.use_memfd=true androidboot.redroid_width=1366 androidboot.redroid_dpi=160",
		Runner:   r,
	}
}

func TestContainerName(t *testing.T) {
	if got := ContainerName("d1"); got != "droidpool-d1" {
		t.Errorf("ContainerName = %q", got)
	}
}

func TestCreateWithOverlay(t *testing.T) {
	f := &fakeRunner{}
	n := testNode(f)
	if err := n.Create(context.Background(), "d1", 5561, "/data/droidpool/base"); err != nil {
		t.Fatal(err)
	}
	run := f.lastMatching("run -d")
	if run == nil {
		t.Fatal("没有发出 docker run")
	}
	joined := strings.Join(run, " ")
	for _, want := range []string{
		"--privileged",                       // binder 需要
		"--name droidpool-d1",                // 容器名可追溯到设备
		"-p 5561:5555",                       // adb 端口映射
		"/data/droidpool/base:/data-base",    // 共享只读基底
		"/data/droidpool/diff/d1:/data-diff", // 实例私有增量
		"androidboot.use_memfd=true",         // .54 无 ashmem，漏了起不来
		"androidboot.redroid_width=1366",     // 实机 profile
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker run 缺少 %q\n实际: %s", want, joined)
		}
	}
	// 用 overlay 时不应再挂独立 /data，否则两套挂载打架
	if strings.Contains(joined, ":/data ") || strings.HasSuffix(joined, ":/data") {
		t.Errorf("overlay 模式不应挂独立 /data: %s", joined)
	}
	// 起容器前要先清掉同名残留
	if f.lastMatching("rm -f") == nil {
		t.Error("创建前应先 docker rm -f 清理同名容器")
	}
}

func TestCreateWithoutOverlay(t *testing.T) {
	f := &fakeRunner{}
	n := testNode(f)
	if err := n.Create(context.Background(), "d2", 5562, ""); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.lastMatching("run -d"), " ")
	if !strings.Contains(joined, "/data/droidpool/data/d2:/data") {
		t.Errorf("非 overlay 模式应挂独立 /data: %s", joined)
	}
	if strings.Contains(joined, "data-base") {
		t.Errorf("非 overlay 模式不应出现 /data-base: %s", joined)
	}
}

func TestWaitBootSucceeds(t *testing.T) {
	f := &fakeRunner{replies: []reply{{match: "getprop", out: "1\n"}}}
	n := testNode(f)
	if err := n.WaitBoot(context.Background(), "d1", 5*time.Second); err != nil {
		t.Fatalf("boot_completed=1 时应立即返回: %v", err)
	}
}

func TestWaitBootTimesOut(t *testing.T) {
	// 一直返回 0：模拟起不来的容器
	f := &fakeRunner{replies: []reply{{match: "getprop", out: "0\n"}}}
	n := testNode(f)
	start := time.Now()
	err := n.WaitBoot(context.Background(), "d1", 100*time.Millisecond)
	if err == nil {
		t.Fatal("超时应报错")
	}
	if !strings.Contains(err.Error(), "未启动完成") {
		t.Errorf("错误信息应说明未启动完成，得到 %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("超时后应尽快返回，不应一直轮询")
	}
}

func TestWaitBootRespectsContext(t *testing.T) {
	f := &fakeRunner{replies: []reply{{match: "getprop", out: "0\n"}}}
	n := testNode(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.WaitBoot(ctx, "d1", time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 取消时应返回 context.Canceled，得到 %v", err)
	}
}

func TestRunningFiltersOwnContainers(t *testing.T) {
	f := &fakeRunner{replies: []reply{{
		match: "ps --format",
		// 节点上可能跑着别人的容器，不能误伤
		out: "droidpool-d1\nsome-other-app\ndroidpool-d2\n\n",
	}}}
	n := testNode(f)
	got, err := n.Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "droidpool-d1" || got[1] != "droidpool-d2" {
		t.Errorf("只应返回 droidpool- 前缀的容器，得到 %v", got)
	}
}

func TestParseHealth(t *testing.T) {
	// 第1行「总量 可用」（自算，见 Health 的注释）、第2行 swap 已用、温度毫度、1 分钟负载
	out := "15843 4623\n1015\n60100\n21.51\n"
	h, err := parseHealth(out)
	if err != nil {
		t.Fatal(err)
	}
	if h.MemTotalMiB != 15843 || h.MemAvailMiB != 4623 {
		t.Errorf("内存解析错: %+v", h)
	}
	if h.SwapUsedMiB != 1015 {
		t.Errorf("swap 解析错: %d", h.SwapUsedMiB)
	}
	if h.TempC != 60.1 {
		t.Errorf("温度应从毫度换算为 60.1，得到 %v", h.TempC)
	}
	if h.Load1 != 21.51 {
		t.Errorf("负载解析错: %v", h.Load1)
	}
}

func TestParseHealthRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "只有一行", "1 2\n3\n"} {
		if _, err := parseHealth(in); err == nil {
			t.Errorf("输入 %q 应报错而不是返回垃圾数据", in)
		}
	}
}

func TestUnderPressure(t *testing.T) {
	const minAvail = 2048
	// 余量充足：放得下再一台
	if (&Health{MemAvailMiB: 10926}).UnderPressure(minAvail) {
		t.Error("可用内存充足不应判为有压力")
	}
	if (&Health{MemAvailMiB: minAvail}).UnderPressure(minAvail) {
		t.Error("恰好等于阈值不应判为有压力")
	}
	// 装不下一台（每台实测约 1 GB）时必须拒绝新 claim
	if !(&Health{MemAvailMiB: 900}).UnderPressure(minAvail) {
		t.Error("可用内存低于阈值应判为有压力")
	}
	// swap 高但内存充足 ≠ 有压力：这正是压测残留会造成的误判
	if (&Health{MemAvailMiB: 10926, SwapUsedMiB: 1023}).UnderPressure(minAvail) {
		t.Error("swap 残留不应在内存充足时误判为有压力")
	}
	// 闸门关闭
	if (&Health{MemAvailMiB: 10}).UnderPressure(0) {
		t.Error("阈值为 0 表示不启用，不应判为有压力")
	}
}

func TestCreatePropagatesError(t *testing.T) {
	boom := errors.New("ssh 不通")
	f := &fakeRunner{replies: []reply{{match: "run -d", err: boom}}}
	n := testNode(f)
	if err := n.Create(context.Background(), "d1", 5561, ""); !errors.Is(err, boom) {
		t.Errorf("docker run 失败应向上传递，得到 %v", err)
	}
}

func TestWipeDataUsesPrivilegedContainer(t *testing.T) {
	f := &fakeRunner{}
	n := testNode(f)
	if err := n.WipeData(context.Background(), "d1", "/data/droidpool/base"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.lastMatching("run --rm"), " ")
	// overlay 模式清 diff 目录
	if !strings.Contains(joined, "/data/droidpool/diff/d1:/wipe") {
		t.Errorf("overlay 模式应清 diff 目录，实际: %s", joined)
	}
	// 必须删干净包括隐藏文件，否则残留状态会漏给下一个 agent
	if !strings.Contains(joined, "/wipe/.[!.]*") {
		t.Errorf("清空命令应覆盖隐藏文件，实际: %s", joined)
	}
	// 不能删挂载点本身，否则下次挂载会失败
	if strings.Contains(joined, "rm -rf /wipe ") {
		t.Errorf("不应删除挂载点本身，实际: %s", joined)
	}
}

func TestWipeDataNonOverlayPath(t *testing.T) {
	f := &fakeRunner{}
	n := testNode(f)
	if err := n.WipeData(context.Background(), "d2", ""); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.lastMatching("run --rm"), " ")
	if !strings.Contains(joined, "/data/droidpool/data/d2:/wipe") {
		t.Errorf("非 overlay 模式应清 data 目录，实际: %s", joined)
	}
}

func TestWipeDataPropagatesError(t *testing.T) {
	boom := errors.New("docker 不通")
	f := &fakeRunner{replies: []reply{{match: "run --rm", err: boom}}}
	n := testNode(f)
	if err := n.WipeData(context.Background(), "d1", ""); !errors.Is(err, boom) {
		t.Errorf("清空失败应向上传递，得到 %v", err)
	}
}

// 基线测试留下的 redroid-N 占着池端口，整个池一台都起不来——对账必须把它们清掉。
func TestReconcileRemovesPortHogsAndOrphans(t *testing.T) {
	f := &fakeRunner{replies: []reply{{
		match: "ps -a --format",
		out: "redroid-1\t0.0.0.0:5561->5555/tcp\n" + // 非 droidpool 但占池端口 → 清
			"droidpool-3588-a-2\t0.0.0.0:5562->5555/tcp\n" + // 在 keep 里 → 留
			"droidpool-3588-a-9\t0.0.0.0:5569->5555/tcp\n" + // droidpool 前缀但不在 keep → 孤儿，清
			"droidpool-golden\t\n" + // golden 构建容器 → 留
			"some-other-app\t0.0.0.0:8080->80/tcp\n", // 无关且不占池端口 → 留
	}}}
	n := testNode(f)
	keep := map[string]bool{"droidpool-3588-a-2": true}
	removed, err := n.Reconcile(context.Background(), keep, []int{5561, 5562, 5563})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range removed {
		got[r] = true
	}
	if !got["redroid-1"] {
		t.Error("占池端口的 redroid-1 应被清掉")
	}
	if !got["droidpool-3588-a-9"] {
		t.Error("不在 keep 里的 droidpool 孤儿应被清掉")
	}
	if got["droidpool-3588-a-2"] {
		t.Error("在 keep 里的设备不应被清")
	}
	if got["droidpool-golden"] {
		t.Error("golden 构建容器不应被清")
	}
	if got["some-other-app"] {
		t.Error("不占池端口的无关容器不应被误伤")
	}
}

func TestMakeGoldenSkipsWhenBaseExists(t *testing.T) {
	// 判据是「基底存在**且**是当前镜像造的」——只看目录在不在不够，
	// 换镜像后旧基底会被拿去跑新镜像，见 TestMakeGoldenRebuildsWhenImageChanged。
	n := testNode(nil)
	f := &fakeRunner{replies: []reply{{match: ".droidpool-image", out: n.Image + "\n"}}}
	n.Runner = f
	if err := n.MakeGolden(context.Background(), "/data/droidpool/base", 5576); err != nil {
		t.Fatal(err)
	}
	// 幂等：已有 base 时不应起任何容器
	if f.lastMatching("run -d") != nil {
		t.Error("base 已存在时不应重新构建")
	}
}

func TestMakeGoldenStripsOverlayFlagAndAppliesSettings(t *testing.T) {
	f := &fakeRunner{replies: []reply{
		{match: ".droidpool-image", out: "\n"}, // 空 = 还没造过
		{match: "getprop", out: "1\n"},
	}}
	n := testNode(f)
	n.BootArgs = "androidboot.use_memfd=true androidboot.use_redroid_overlayfs=1 androidboot.redroid_width=1366"
	if err := n.MakeGolden(context.Background(), "/data/droidpool/base", 5576); err != nil {
		t.Fatal(err)
	}
	run := strings.Join(f.lastMatching("run -d"), " ")
	// 造 base 时 /data 是普通挂载，带 overlay 参数会让容器去找不存在的 /data-base
	if strings.Contains(run, "use_redroid_overlayfs") {
		t.Errorf("构建 golden 时不应带 overlay 参数: %s", run)
	}
	if !strings.Contains(run, "/data/droidpool/base:/data") {
		t.Errorf("应以普通 -v 挂到 /data: %s", run)
	}
	if !strings.Contains(run, "use_memfd=true") {
		t.Errorf("其它启动参数应保留: %s", run)
	}
	// 系统设置必须落下：关动画是截图与驱动稳定的前提
	// screen_off_timeout 是真正管用的那条常亮设置：容器没有充电器，svc power stayon
	// 只在已插电时生效，光靠它设备还是会息屏变成黑屏
	for _, want := range []string{"window_animation_scale 0", "screen_off_timeout 2147483647",
		"stayon true", "persist.sys.locale zh-CN", "package_verifier_enable 0"} {
		if f.lastMatching(want) == nil {
			t.Errorf("golden 应执行设置 %q", want)
		}
	}
	// 结束要 stop 而非 kill，否则设置可能没落盘
	if f.lastMatching("stop") == nil {
		t.Error("应 docker stop 让 /data 落盘")
	}
}

func TestMakeGoldenFailsIfBootTimesOut(t *testing.T) {
	f := &fakeRunner{replies: []reply{
		{match: "test -d", out: "no\n"},
		{match: "getprop", out: "0\n"},
	}}
	n := testNode(f)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := n.MakeGolden(ctx, "/data/droidpool/base", 5576); err == nil {
		t.Error("容器起不来时应报错，不能留下半成品 base")
	}
}

func TestRunningSet(t *testing.T) {
	f := &fakeRunner{replies: []reply{{match: "ps --format", out: "droidpool-a\nother\ndroidpool-b\n"}}}
	set, err := testNode(f).RunningSet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !set["droidpool-a"] || !set["droidpool-b"] || set["other"] {
		t.Errorf("集合内容不对: %v", set)
	}
}

// 采集内存用的 awk 表达式，直接喂真实形状的 /proc/meminfo 验算。
// 内核的 MemAvailable 在这台节点上会报出比 MemFree 还小的值，所以我们自己算；
// 算错的话池子要么误拒 claim，要么在真没内存时还往上塞。
func TestMemAvailFormula(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("没有 awk")
	}
	const meminfo = `MemTotal:       16334848 kB
MemFree:         6038528 kB
MemAvailable:    2494464 kB
Buffers:          204800 kB
Cached:          3317760 kB
SReclaimable:     288768 kB
Shmem:            348160 kB
Mapped:           967680 kB
`
	// 用 node.go 里那一份，读 stdin 而不是 /proc/meminfo
	script := strings.Replace(memAvailAwk, " /proc/meminfo", "", 1)
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = strings.NewReader(meminfo)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("跑 awk 失败: %v", err)
	}
	var total, avail int
	if _, err := fmt.Sscanf(string(out), "%d %d", &total, &avail); err != nil {
		t.Fatalf("输出不符: %q", out)
	}
	if total != 15952 {
		t.Errorf("总量 = %d，期望 15952", total)
	}
	// 6038528+3317760+288768-348160-967680 = 8329216 kB = 8134 MiB
	if avail != 8134 {
		t.Errorf("可用 = %d，期望 8134", avail)
	}
	// 关键：必须远高于内核那个 2436，否则等于没改
	if avail <= 2436 {
		t.Errorf("自算结果 %d 不该低于内核的 MemAvailable(2436)——那说明公式又退回保守了", avail)
	}
}

// 页缓存被压光时（cache 全没了），结果不能低于 MemFree——空闲内存一定是可用的。
func TestMemAvailNeverBelowFree(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("没有 awk")
	}
	const meminfo = `MemTotal:       16334848 kB
MemFree:         6038528 kB
Cached:            10240 kB
SReclaimable:       1024 kB
Shmem:            348160 kB
Mapped:           967680 kB
`
	// 用 node.go 里那一份，读 stdin 而不是 /proc/meminfo
	script := strings.Replace(memAvailAwk, " /proc/meminfo", "", 1)
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = strings.NewReader(meminfo)
	out, _ := cmd.Output()
	var total, avail int
	fmt.Sscanf(string(out), "%d %d", &total, &avail)
	if avail != 5897 {
		t.Errorf("可用 = %d，期望回落到 MemFree 的 5897", avail)
	}
}

// 换了镜像就必须重造 golden 基底。不重造的话，8 台设备会拿着旧镜像的 /data
// 去跑新镜像——同版本大概率能跑起来，但 fingerprint 变了，属于「跑起来了
// 但不对」，最难查。
func TestMakeGoldenRebuildsWhenImageChanged(t *testing.T) {
	f := &fakeRunner{replies: []reply{
		{match: ".droidpool-image", out: "redroid/redroid:14.0.0_64only-latest\n"},
		{match: "getprop", out: "1\n"},
	}}
	n := testNode(f)
	n.Image = "droidpool/redroid:14-custom"
	_ = n.MakeGolden(context.Background(), "/data/droidpool/base", 5599)

	if f.lastMatching("rm -rf /wipe") == nil {
		t.Error("镜像变了却没清空旧基底")
	}
	if f.lastMatching("--name droidpool-golden") == nil {
		t.Error("镜像变了却没重造 golden")
	}
}

// 同一个镜像就别重造——每次启动都重来一遍要一分多钟。
func TestMakeGoldenSkipsWhenImageSame(t *testing.T) {
	f := &fakeRunner{replies: []reply{
		{match: ".droidpool-image", out: "redroid/redroid:14.0.0_64only-latest\n"},
	}}
	n := testNode(f)
	n.Image = "redroid/redroid:14.0.0_64only-latest"
	if err := n.MakeGolden(context.Background(), "/data/droidpool/base", 5599); err != nil {
		t.Fatal(err)
	}
	if f.lastMatching("--name droidpool-golden") != nil {
		t.Error("同一个镜像不该重造")
	}
	if f.lastMatching("rm -rf /wipe") != nil {
		t.Error("同一个镜像不该清空基底")
	}
}
