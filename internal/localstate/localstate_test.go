package localstate

import (
	"os"
	"os/exec"
	"path/filepath"
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
		if got := SessionKey(); got != want {
			t.Errorf("SessionKey(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 收尾脚本从文件名反推会话键再传回 DROIDPOOL_SESSION：安全化必须幂等，否则还不到同一份记录
func TestSessionKeyIdempotent(t *testing.T) {
	t.Setenv("DROIDPOOL_SESSION", "a b/会话.x")
	once := SessionKey()
	t.Setenv("DROIDPOOL_SESSION", once)
	if twice := SessionKey(); twice != once {
		t.Errorf("再安全化一次应不变：%q → %q", once, twice)
	}
}

func TestFileNameFollowsSession(t *testing.T) {
	t.Setenv("DROIDPOOL_SESSION", "")
	if got := FileName(); got != ".droidpool" {
		t.Errorf("无会话键时应为 .droidpool，得到 %q", got)
	}
	t.Setenv("DROIDPOOL_SESSION", "s1")
	if got := FileName(); got != ".droidpool.s1" {
		t.Errorf("有会话键时应带后缀，得到 %q", got)
	}
}

func TestPathAnchorsAtWorktreeTop(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init 不可用: %v\n%s", err, out)
	}
	top, _ := filepath.EvalSymlinks(repo) // macOS 临时目录经过符号链接，git 报的是真实路径
	sub := filepath.Join(repo, "cashier-app", "lib")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DROIDPOOL_SESSION", "s1")
	for _, dir := range []string{repo, sub} {
		if got := Path(dir); got != filepath.Join(top, ".droidpool.s1") {
			t.Errorf("Path(%s) = %s；从子目录也要落到 worktree 顶层（与幂等键同一口径）", dir, got)
		}
	}
}

func TestPathOutsideGitStaysInDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir)) // 临时目录恰好在某个 git 仓库里时，别让 git 往上找到它
	t.Setenv("DROIDPOOL_SESSION", "")
	if got := Path(dir); got != filepath.Join(dir, ".droidpool") {
		t.Errorf("不在 git 仓库里应放在目录本身，得到 %s", got)
	}
}
