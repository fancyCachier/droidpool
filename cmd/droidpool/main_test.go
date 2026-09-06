package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionKeySanitized(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"abc-123_x.y": "abc-123_x.y",
		"a b/c\\d":    "a-b-c-d", // 要进文件名，路径分隔符与空白都不能留
		"会话":          "--",
	}
	for in, want := range cases {
		t.Setenv("DROIDPOOL_SESSION", in)
		if got := sessionKey(); got != want {
			t.Errorf("sessionKey(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestStateFileFollowsSession(t *testing.T) {
	t.Setenv("DROIDPOOL_SESSION", "")
	if got := stateFile(); got != ".droidpool" {
		t.Errorf("无会话键时状态文件应为 .droidpool，得到 %q", got)
	}
	t.Setenv("DROIDPOOL_SESSION", "s1")
	if got := stateFile(); got != ".droidpool.s1" {
		t.Errorf("有会话键时状态文件应带后缀，得到 %q", got)
	}
}

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
