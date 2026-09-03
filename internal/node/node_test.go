package node

import (
	"context"
	"errors"
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
	// free -m 第2行「总量 可用」、第3行 swap 已用、温度毫度、1 分钟负载
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
	guard := 256
	// swap 为 0 是健康态（Phase 1 的 N≤10）
	if (&Health{SwapUsedMiB: 0}).UnderPressure(guard) {
		t.Error("swap=0 不应判为有压力")
	}
	if (&Health{SwapUsedMiB: 256}).UnderPressure(guard) {
		t.Error("swap 等于阈值不应判为有压力")
	}
	// N=12 实测 swap 590 MiB，此时失败率已到 5.6%，必须拒绝新 claim
	if !(&Health{SwapUsedMiB: 590}).UnderPressure(guard) {
		t.Error("swap 超阈值应判为有压力")
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
