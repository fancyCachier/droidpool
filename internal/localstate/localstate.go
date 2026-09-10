// Package localstate 管 agent 侧本地租约记录的位置与文件名，CLI 与 MCP 共用。
//
// 记录固定放在 worktree 顶层，与发给控制面的幂等键同一口径（键里的 worktree 名也取自 git 顶层）。
// 原先落在执行命令时的当前目录：在顶层 claim、进子目录 run 就读不到记录；在子目录 claim 的记录，
// 收尾时在顶层 release 也还不掉，随 worktree 一起被删，设备白占到空闲回收。
package localstate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SessionKey 返回 DROIDPOOL_SESSION（已做文件名安全化），未设置时为空。
//
// 幂等键原本只有 (host, worktree)，假设「同一主机同一目录再来一次 = 同一个 agent 在重试」。
// dsh 这类多会话宿主在同一台机器、同一个检出里跑好几个 agent，这个假设不成立：
// 后来者全都「复用既有租约」挤到同一台设备上（2026-09-04 线上 20 条租约 19 条在 1 号机）。
// 会话键把每个会话分开，本地记录也随之分开；dsh 插件会自动注入，人手工用时可以不设。
func SessionKey() string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, os.Getenv("DROIDPOOL_SESSION"))
}

// FileName 记录的文件名：.droidpool，带会话键时为 .droidpool.<会话键>。
func FileName() string {
	if s := SessionKey(); s != "" {
		return ".droidpool." + s
	}
	return ".droidpool"
}

// Path dir 所在 worktree 顶层下的记录路径；不在 git 仓库里就放在 dir 下。
func Path(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return filepath.Join(top, FileName())
		}
	}
	return filepath.Join(dir, FileName())
}
