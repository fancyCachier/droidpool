package node

import (
	"archive/tar"
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/fancyCachier/droidpool/internal/pool"
)

func acme() *pool.Identity {
	id := pool.Identity{Model: "X1", Brand: "ACME"}.Normalized()
	return &id
}

const sampleProps = `# begin build properties
ro.product.system.brand=redroid
ro.product.system.device=redroid_arm64_only
ro.product.system.manufacturer=redroid
ro.product.system.model=redroid14_arm64_only
ro.product.system.name=redroid_arm64_only
ro.system.product.cpu.abilist=arm64-v8a
ro.product.model_for_attestation=
ro.product.cpu.abi=arm64-v8a
ro.build.id=UP1A.231005.007.A1
`

// propTar 造一份 `docker cp … -` 那样的 tar 流，里面一个 build.prop。
func propTar(t *testing.T, content string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "build.prop", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// withProps 给假 runner 装上「docker cp 返回 build.prop」的应答。
func withProps(t *testing.T, f *fakeRunner) *fakeRunner {
	t.Helper()
	f.replies = append(f.replies, reply{match: "cp droidpool-props-", out: propTar(t, sampleProps)})
	return f
}

// 身份要落到四份 build.prop 上：只改 system 与 vendor 不生效，
// ro.product.model 按 product → odm → vendor → system_ext → system 取第一个
// 非空值，product 分区那份会赢（2026-09-09 实测）。
func TestCreateWithIdentityOverridesAllFourPropFiles(t *testing.T) {
	f := withProps(t, &fakeRunner{})
	n := testNode(f)
	if err := n.Create(context.Background(), "d1", 5561, "", acme()); err != nil {
		t.Fatal(err)
	}
	// 原文件从镜像里取：create 不启动，cp 成 tar 流，用完删掉
	if c := f.lastMatching("create --name droidpool-props-d1"); c == nil || !strings.Contains(strings.Join(c, " "), n.Image) {
		t.Errorf("应从当前镜像 create 一个临时容器取 build.prop: %v", c)
	}
	for _, p := range []string{"/system/build.prop", "/vendor/build.prop", "/system/product/etc/build.prop", "/system/system_ext/etc/build.prop"} {
		if f.lastMatching("cp droidpool-props-d1:"+p+" -") == nil {
			t.Errorf("没有以 tar 流拷出 %s", p)
		}
	}
	if f.lastMatching("rm -f droidpool-props-d1") == nil {
		t.Error("临时容器用完应删掉")
	}
	// 写回走一次性 busybox 容器（不能让 docker 客户端直接写宿主路径，见 identityArgs 注释）
	w := f.lastMatching("cat > /out/d1/")
	if w == nil {
		t.Fatal("没有经 busybox 写属性文件")
	}
	wj := strings.Join(w, " ")
	if !strings.Contains(wj, "-v /data/droidpool/props:/out busybox:stable") {
		t.Errorf("应挂 props 根目录进 busybox 写: %s", wj)
	}
	script := w[len(w)-1]
	for _, want := range []string{
		"set -e\nmkdir -p /out/d1\n",
		// 结束符之后必须换行再接下一段，接 && 是语法错误（线上踩过）
		"DROIDPOOLEOF\ncat > /out/d1/vendor.prop",
		"cat > /out/d1/system.prop <<'DROIDPOOLEOF'",
		"cat > /out/d1/vendor.prop <<'DROIDPOOLEOF'",
		"cat > /out/d1/product.prop <<'DROIDPOOLEOF'",
		"cat > /out/d1/system_ext.prop <<'DROIDPOOLEOF'",
		"ro.product.system.model=X1\n",
		"ro.product.system.brand=ACME\n",
		"ro.product.system.manufacturer=ACME\n",
		"ro.product.system.device=x1\n",
		"ro.product.system.name=x1\n",
		"ro.build.id=UP1A.231005.007.A1\n", // 其余行原样
	} {
		if !strings.Contains(script, want) {
			t.Errorf("写回脚本缺少 %q\n实际: %s", want, script)
		}
	}
	if strings.Contains(script, "redroid14_arm64_only") {
		t.Errorf("旧型号不该残留: %s", script)
	}
	run := strings.Join(f.lastMatching("run -d"), " ")
	for _, want := range []string{
		"-v /data/droidpool/props/d1/system.prop:/system/build.prop:ro",
		"-v /data/droidpool/props/d1/vendor.prop:/vendor/build.prop:ro",
		"-v /data/droidpool/props/d1/product.prop:/system/product/etc/build.prop:ro",
		"-v /data/droidpool/props/d1/system_ext.prop:/system/system_ext/etc/build.prop:ro",
		"androidboot.serialno=DPD1", // 没给序列号就按设备 id 派生
	} {
		if !strings.Contains(run, want) {
			t.Errorf("docker run 缺少 %q\n实际: %s", want, run)
		}
	}
	// 启动参数要在镜像名之后，挂载要在之前
	if strings.Index(run, "androidboot.serialno") < strings.Index(run, n.Image) ||
		strings.Index(run, "props/d1/system.prop") > strings.Index(run, n.Image) {
		t.Errorf("参数顺序不对: %s", run)
	}
}

func TestRewriteProps(t *testing.T) {
	got := rewriteProps(strings.ReplaceAll(sampleProps, "system", "vendor"), *acme())
	for _, want := range []string{
		"ro.product.vendor.model=X1\n", "ro.product.vendor.brand=ACME\n",
		"ro.product.vendor.manufacturer=ACME\n", "ro.product.vendor.device=x1\n",
		"ro.product.vendor.name=x1\n",
		// 形似但不是目标的键不能动
		"ro.vendor.product.cpu.abilist=arm64-v8a\n", "ro.product.model_for_attestation=\n",
		"ro.product.cpu.abi=arm64-v8a\n", "# begin build properties\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q\n实际:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Errorf("应恰好以一个换行结尾: %q", got)
	}
}

func TestFirstFileInTar(t *testing.T) {
	if got, err := firstFileInTar(propTar(t, "a=b\n")); err != nil || got != "a=b\n" {
		t.Errorf("firstFileInTar = %q, %v", got, err)
	}
	if _, err := firstFileInTar(""); err == nil {
		t.Error("空流应报错，而不是拿空内容去覆盖 build.prop")
	}
}

func TestCreateWithExplicitSerial(t *testing.T) {
	f := withProps(t, &fakeRunner{})
	id := acme()
	id.Serial = "X10001"
	if err := testNode(f).Create(context.Background(), "d1", 5561, "", id); err != nil {
		t.Fatal(err)
	}
	if run := strings.Join(f.lastMatching("run -d"), " "); !strings.Contains(run, "androidboot.serialno=X10001") {
		t.Errorf("应用显式序列号: %s", run)
	}
}

// 没配身份时必须和从前一模一样：不多一次 docker 调用，不多一个挂载。
func TestCreateWithoutIdentityTouchesNoProps(t *testing.T) {
	f := &fakeRunner{}
	if err := testNode(f).Create(context.Background(), "d1", 5561, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "props") || strings.Contains(j, "serialno") || strings.Contains(j, "build.prop") {
			t.Errorf("没配身份不该碰属性: %s", j)
		}
	}
}

// 值会原样进 build.prop 与启动参数，非法字符必须在进容器之前挡住。
func TestCreateRejectsInvalidIdentity(t *testing.T) {
	f := &fakeRunner{}
	err := testNode(f).Create(context.Background(), "d1", 5561, "", &pool.Identity{Model: "a/b", Brand: "x", Manufacturer: "x", Device: "d", Name: "n"})
	if err == nil {
		t.Fatal("非法身份应报错")
	}
	if f.lastMatching("run -d") != nil {
		t.Error("被拒时不该起容器")
	}
}

func TestDefaultSerial(t *testing.T) {
	if got := DefaultSerial("3588-a-1"); got != "DP3588A1" {
		t.Errorf("DefaultSerial = %q", got)
	}
}

// docker exec 默认是 root，而 AppOps 按 uid 判 MOCK_LOCATION，root 会被拒
// （实测 "android from uid 0 not allowed to perform MOCK_LOCATION"），必须以
// shell 用户跑。gps 与 network 都要设，fused 是从这两个里取的。
func TestSetLocationRunsAsShellAndSetsBothProviders(t *testing.T) {
	f := &fakeRunner{}
	if err := testNode(f).SetLocation(context.Background(), "d1", "23.1291,113.2644"); err != nil {
		t.Fatal(err)
	}
	c := f.lastMatching("exec")
	j := strings.Join(c, " ")
	if !strings.HasPrefix(j, "docker exec -u 2000 droidpool-d1 sh -c ") {
		t.Errorf("应以 uid 2000 在设备容器里执行: %s", j)
	}
	script := c[len(c)-1]
	for _, want := range []string{
		"appops set com.android.shell android:mock_location allow",
		"for p in gps network",
		"add-test-provider $p",
		"set-test-provider-enabled $p true",
		"set-test-provider-location $p --location 23.1291,113.2644",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("脚本缺少 %q\n实际: %s", want, script)
		}
	}
}

func TestSetLocationEmptyRemovesProviders(t *testing.T) {
	f := &fakeRunner{}
	if err := testNode(f).SetLocation(context.Background(), "d1", ""); err != nil {
		t.Fatal(err)
	}
	c := f.lastMatching("exec")
	script := c[len(c)-1]
	if !strings.Contains(script, "remove-test-provider") || strings.Contains(script, "add-test-provider") {
		t.Errorf("撤销应只删 provider: %s", script)
	}
}

func TestSetLocationRejectsBadCoordinates(t *testing.T) {
	f := &fakeRunner{}
	if err := testNode(f).SetLocation(context.Background(), "d1", "91,0"); err == nil {
		t.Fatal("非法坐标应报错")
	}
	if len(f.calls) != 0 {
		t.Error("被拒时不该碰设备")
	}
}

// golden 要按节点默认身份起：基底里记着开机时的 fingerprint，设备用另一个
// fingerprint 起来会被当成 OTA 升级，每次复位都重跑一遍。换了身份要重造。
func TestMakeGoldenUsesDefaultIdentityAndStampsIt(t *testing.T) {
	f := withProps(t, &fakeRunner{replies: []reply{
		{match: ".droidpool-image", out: "\n"},
		{match: "getprop", out: "1\n"},
	}})
	n := testNode(f)
	n.DefaultIdentity = acme()
	if err := n.MakeGolden(context.Background(), "/data/droidpool/base", 5576); err != nil {
		t.Fatal(err)
	}
	run := strings.Join(f.lastMatching("run -d"), " ")
	if !strings.Contains(run, "props/golden/product.prop:/system/product/etc/build.prop:ro") ||
		!strings.Contains(run, "androidboot.serialno=DPGOLDEN") {
		t.Errorf("golden 应带默认身份起: %s", run)
	}
	stamp := strings.Join(f.lastMatching(".droidpool-image"), " ")
	if !strings.Contains(stamp, "X1") {
		t.Errorf("标记里应记下身份，换身份才知道要重造: %s", stamp)
	}
}

func TestMakeGoldenRebuildsWhenIdentityChanged(t *testing.T) {
	n := testNode(nil)
	// 旧标记只有镜像名（没配身份时造的）
	f := withProps(t, &fakeRunner{replies: []reply{
		{match: ".droidpool-image", out: n.Image + "\n"},
		{match: "getprop", out: "1\n"},
	}})
	n.Runner = f
	n.DefaultIdentity = acme()
	if err := n.MakeGolden(context.Background(), "/data/droidpool/base", 5576); err != nil {
		t.Fatal(err)
	}
	if f.lastMatching("run -d") == nil {
		t.Error("默认身份变了应重造 golden")
	}
}

// 写回脚本是拼出来的 sh，语法错只有跑到节点上才会炸；这里用本机 sh -n 先把关。
// 不是 adb / docker，只是本地 shell 的语法检查，不碰任何外部服务。
func TestPropsScriptParsesAsShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("本机没有 sh")
	}
	f := withProps(t, &fakeRunner{})
	if err := testNode(f).Create(context.Background(), "d1", 5561, "", acme()); err != nil {
		t.Fatal(err)
	}
	w := f.lastMatching("cat > /out/d1/")
	script := w[len(w)-1]
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("写回脚本过不了 sh -n: %v\n%s\n脚本:\n%s", err, out, script)
	}
}
