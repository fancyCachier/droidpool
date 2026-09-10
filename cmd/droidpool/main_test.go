package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimWorktree(t *testing.T) {
	if got := claimWorktree("big-boss", ""); got != "big-boss" {
		t.Errorf("无会话键时 worktree 应原样发送，得到 %q", got)
	}
	if got := claimWorktree("big-boss", "s1"); got != "big-boss@s1" {
		t.Errorf("有会话键时应并入幂等键，得到 %q", got)
	}
}

// fakeClaimServer 起一个假控制面：记录收到的 worktree，按 reused 回一条固定租约。
func fakeClaimServer(t *testing.T, reused bool) (srv *httptest.Server, gotWorktree *string) {
	t.Helper()
	var wt string
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Worktree string `json:"worktree"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		wt = req.Worktree
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"lease_id": "L1", "device_id": "n-1", "adb_addr": "127.0.0.1:1",
			"expires_at": "2026-01-01T00:00:00Z", "reused": reused,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &wt
}

// claimEnv 把测试放进一个空 git 仓库，用假 adb 挡住 claim 后的 adb connect，
// 并把 stdout 丢掉、stderr 收进文件供断言。
func claimEnv(t *testing.T) (repo string, stderr func() string) {
	t.Helper()
	repo = t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init 不可用: %v\n%s", err, out)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "adb"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(repo)

	errPath := filepath.Join(t.TempDir(), "stderr")
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, errFile
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		errFile.Close()
		devNull.Close()
	})
	return repo, func() string {
		b, _ := os.ReadFile(errPath)
		return string(b)
	}
}

func TestClaimSendsSessionKeyAndIsolatesState(t *testing.T) {
	repo, _ := claimEnv(t)
	t.Setenv("DROIDPOOL_SESSION", "s1")
	srv, gotWorktree := fakeClaimServer(t, false)

	cmdClaim(&client{base: srv.URL, token: "t"})

	if want := filepath.Base(repo) + "@s1"; *gotWorktree != want {
		t.Errorf("发给控制面的 worktree = %q，期望 %q", *gotWorktree, want)
	}
	if _, err := os.Stat(".droidpool.s1"); err != nil {
		t.Errorf("状态文件应写到 .droidpool.s1: %v", err)
	}
	if _, err := os.Stat(".droidpool"); err == nil {
		t.Error("有会话键时不应再写无后缀的 .droidpool")
	}
}

func TestClaimWarnsWhenReusedLeaseIsNotOurs(t *testing.T) {
	_, stderr := claimEnv(t)
	t.Setenv("DROIDPOOL_SESSION", "")
	srv, _ := fakeClaimServer(t, true)

	cmdClaim(&client{base: srv.URL, token: "t"})

	if !strings.Contains(stderr(), "另一个会话") {
		t.Errorf("复用了别人的租约应有提示，stderr = %q", stderr())
	}
}

func TestClaimStaysQuietWhenReusingOwnLease(t *testing.T) {
	_, stderr := claimEnv(t)
	t.Setenv("DROIDPOOL_SESSION", "")
	if err := saveState(leaseState{LeaseID: "L1", DeviceID: "n-1", ADBAddr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	srv, _ := fakeClaimServer(t, true)

	cmdClaim(&client{base: srv.URL, token: "t"})

	if strings.Contains(stderr(), "另一个会话") {
		t.Errorf("复用自己的租约不该提示，stderr = %q", stderr())
	}
}

// agent 常在 worktree 顶层 claim、再 cd 进子目录干活；子目录里再 claim 是复用自己的租约，不该提示。
func TestClaimStaysQuietFromSubdirOfOwnWorktree(t *testing.T) {
	repo, stderr := claimEnv(t)
	t.Setenv("DROIDPOOL_SESSION", "")
	if err := saveState(leaseState{LeaseID: "L1", DeviceID: "n-1", ADBAddr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "cashier-app")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	srv, _ := fakeClaimServer(t, true)

	cmdClaim(&client{base: srv.URL, token: "t"})

	if strings.Contains(stderr(), "另一个会话") {
		t.Errorf("在自己 worktree 的子目录里复用不该提示，stderr = %q", stderr())
	}
}

// TestBatterySetsLevelBeforePresent 锁住一条安全不变式：level 必须先于 present。
//
// 反过来的话（present=1 而 level 还是 0），Android 判定电量耗尽发起关机，
// 容器约 20 s 后退出——2026-09-06 实测把一台在线设备干下线过。关机是异步的，
// 期间后面的命令还照常返回，所以一旦顺序写反，测试之外几乎不可能靠现场归因。
func TestBatterySetsLevelBeforePresent(t *testing.T) {
	log := batteryFakeADB(t)
	cmdBattery([]string{"--level", "67", "--status", "discharging"})

	got := batteryCalls(t, log)
	lvl, pres := indexOfCall(got, "set level 67"), indexOfCall(got, "set present 1")
	if lvl < 0 || pres < 0 {
		t.Fatalf("没发出 set level / set present：%v", got)
	}
	if lvl > pres {
		t.Errorf("set level 必须早于 set present，实际顺序：%v", got)
	}
	if i := indexOfCall(got, "unplug"); i < 0 {
		t.Errorf("discharging 应当 unplug，实际：%v", got)
	}
}

func TestBatteryChargingPlugsAC(t *testing.T) {
	log := batteryFakeADB(t)
	cmdBattery([]string{"--level", "50", "--status", "charging"})

	got := batteryCalls(t, log)
	if indexOfCall(got, "set ac 1") < 0 {
		t.Errorf("charging 应当插上 ac，实际：%v", got)
	}
	if indexOfCall(got, "unplug") >= 0 {
		t.Errorf("charging 不该 unplug，实际：%v", got)
	}
}

// batteryFakeADB 用一个把参数记进文件的假 adb 挡住真设备，返回日志路径。
func batteryFakeADB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "adb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DROIDPOOL_SESSION", "")
	t.Chdir(t.TempDir())
	if err := saveState(leaseState{ADBAddr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	return log
}

func batteryCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("假 adb 没被调用: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// indexOfCall 找出第一条含 want 的调用下标，没有则 -1。
func indexOfCall(calls []string, want string) int {
	for i, c := range calls {
		if strings.Contains(c, want) {
			return i
		}
	}
	return -1
}

// 记录固定在 worktree 顶层：从子目录 claim 也写到顶层，子目录里执行别的命令读得到顶层的记录。
// 原先落在当前目录——skill 的流程是顶层 claim 后 cd cashier-app 再 run，run 就报「先跑 droidpool claim」。
func TestStateAnchoredAtWorktreeTop(t *testing.T) {
	repo, _ := claimEnv(t)
	t.Setenv("DROIDPOOL_SESSION", "")
	sub := filepath.Join(repo, "cashier-app")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	srv, _ := fakeClaimServer(t, false)

	cmdClaim(&client{base: srv.URL, token: "t"})

	if _, err := os.Stat(filepath.Join(repo, ".droidpool")); err != nil {
		t.Errorf("从子目录 claim，记录应写到 worktree 顶层: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, ".droidpool")); err == nil {
		t.Error("子目录里不该再写一份记录")
	}
	if s, err := loadState(); err != nil || s.LeaseID != "L1" {
		t.Errorf("子目录里应读得到顶层的记录：%+v %v", s, err)
	}
}

func TestReleaseOutcome(t *testing.T) {
	cases := []struct {
		name            string
		code            int
		err             error
		clear, ok       bool
		msgContains     string
		msgMustNotClaim bool // 失败时不许出现「已归还」
	}{
		{"成功", 200, nil, true, true, "已归还设备 n-1", false},
		{"租约已不在", 404, errors.New("not_found"), true, true, "租约已不在", false},
		{"服务端出错", 500, errors.New("internal: boom"), false, false, "本地记录保留", true},
		{"连不上", 0, errors.New("dial tcp: refused"), false, false, "稍后重试", true},
	}
	for _, c := range cases {
		msg, clear, ok := releaseOutcome(c.code, c.err, "n-1")
		if clear != c.clear || ok != c.ok || !strings.Contains(msg, c.msgContains) {
			t.Errorf("%s：msg=%q clear=%v ok=%v", c.name, msg, clear, ok)
		}
		if c.msgMustNotClaim && strings.Contains(msg, "已归还") {
			t.Errorf("%s：失败时不能谎报已归还：%q", c.name, msg)
		}
	}
}

// fakeReleaseServer DELETE 一律回 code。
func fakeReleaseServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/leases/L1" {
			t.Errorf("请求打错了地方：%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":"x","message":"y"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReleaseClearsRecordOnSuccessAndGone(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNotFound} {
		repo, _ := claimEnv(t)
		t.Setenv("DROIDPOOL_SESSION", "")
		if err := saveState(leaseState{LeaseID: "L1", DeviceID: "n-1"}); err != nil {
			t.Fatal(err)
		}
		cmdRelease(&client{base: fakeReleaseServer(t, code).URL, token: "t"})
		if _, err := os.Stat(filepath.Join(repo, ".droidpool")); err == nil {
			t.Errorf("HTTP %d 后本地记录应清掉", code)
		}
	}
}

// 失败分支会 os.Exit(1)：在子进程里跑 cmdRelease，断言退出码与记录还在。
func TestReleaseFailureKeepsRecordAndExitsNonZero(t *testing.T) {
	if os.Getenv("DP_RELEASE_CHILD") == "1" {
		t.Chdir(os.Getenv("DP_REPO"))
		cmdRelease(&client{base: os.Getenv("DP_URL"), token: "t"})
		return
	}
	repo, _ := claimEnv(t)
	t.Setenv("DROIDPOOL_SESSION", "")
	if err := saveState(leaseState{LeaseID: "L1", DeviceID: "n-1"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestReleaseFailureKeepsRecordAndExitsNonZero$")
	cmd.Env = append(os.Environ(), "DP_RELEASE_CHILD=1", "DP_REPO="+repo, "DP_URL="+fakeReleaseServer(t, http.StatusInternalServerError).URL)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("归还失败应以退出码 1 结束，得到 %v\n%s", err, out)
	}
	if strings.Contains(string(out), "已归还") {
		t.Errorf("失败时不能打印「已归还」：%s", out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".droidpool")); err != nil {
		t.Errorf("归还失败应保留本地记录以便重试: %v", err)
	}
}
