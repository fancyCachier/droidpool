// droidpool 是 agent 侧 CLI：claim 一台独占设备、跑完验证、release 归还。
//
// 典型用法（接在 worktree 开工流程里）：
//
//	droidpool claim                       # 拿设备，写 .droidpool，打印 adb 地址
//	flutter run -d $(droidpool addr)
//	droidpool status                      # 看租约与人工接管标志
//	droidpool release                     # 归还
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const stateFile = ".droidpool"

type client struct {
	base  string
	token string
}

func (c *client) do(method, path string, body any, out any) (int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequest(method, c.base+path, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, out)
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error, Message string
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return resp.StatusCode, fmt.Errorf("%s: %s", e.Error, msg)
	}
	return resp.StatusCode, nil
}

// gitInfo 从当前目录推导 worktree 名、分支与 HEAD。
func gitInfo() (worktree, branch, head string) {
	run := func(args ...string) string {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	top := run("rev-parse", "--show-toplevel")
	if top != "" {
		worktree = filepath.Base(top)
	}
	return worktree, run("rev-parse", "--abbrev-ref", "HEAD"), run("rev-parse", "--short", "HEAD")
}

type leaseState struct {
	LeaseID  string `json:"lease_id"`
	DeviceID string `json:"device_id"`
	ADBAddr  string `json:"adb_addr"`
}

func saveState(s leaseState) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(stateFile, b, 0o600)
}

func loadState() (leaseState, error) {
	var s leaseState
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return s, fmt.Errorf("没有本地租约记录（先跑 droidpool claim）")
	}
	return s, json.Unmarshal(b, &s)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	base := envOr("DROIDPOOL_URL", "http://192.168.14.32:8600")
	token := os.Getenv("DROIDPOOL_TOKEN")
	if token == "" {
		fatal("未设置 DROIDPOOL_TOKEN")
	}
	c := &client{base: strings.TrimRight(base, "/"), token: token}

	switch os.Args[1] {
	case "claim":
		cmdClaim(c)
	case "addr":
		s, err := loadState()
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(s.ADBAddr)
	case "status":
		cmdStatus(c)
	case "release":
		cmdRelease(c)
	case "devices":
		cmdDevices(c)
	case "-h", "--help", "help":
		usage()
	default:
		fatal("未知子命令 %q（跑 droidpool help 看用法）", os.Args[1])
	}
}

func cmdClaim(c *client) {
	wt, branch, head := gitInfo()
	if wt == "" {
		fatal("当前目录不是 git 仓库，无法推导 worktree 名")
	}
	host, _ := os.Hostname()
	req := map[string]any{
		"owner": os.Getenv("USER") + "@" + host, "host": host,
		"worktree": wt, "branch": branch, "head_sha": head,
	}
	var resp struct {
		leaseState
		ExpiresAt time.Time `json:"expires_at"`
		Reused    bool      `json:"reused"`
	}
	code, err := c.do("POST", "/api/leases", req, &resp)
	if err != nil {
		// 池满与换页是可预期的拒绝，给出可操作的提示而不是堆栈
		switch code {
		case http.StatusConflict:
			fatal("池中无空闲设备，稍后再试（或看设备墙谁占着）")
		case http.StatusServiceUnavailable:
			fatal("节点正在换页，暂不接受新租约：%v", err)
		}
		fatal("claim 失败: %v", err)
	}
	if err := saveState(resp.leaseState); err != nil {
		fatal("写 %s 失败: %v", stateFile, err)
	}
	verb := "已分配"
	if resp.Reused {
		verb = "复用既有租约"
	}
	fmt.Printf("%s 设备 %s\n  adb: %s\n  租约: %s（到期 %s）\n  用法: flutter run -d $(droidpool addr)\n",
		verb, resp.DeviceID, resp.ADBAddr, resp.LeaseID, resp.ExpiresAt.Local().Format("15:04:05"))
	// 顺手 adb connect，省掉 agent 一步
	if out, err := exec.Command("adb", "connect", resp.ADBAddr).CombinedOutput(); err == nil {
		fmt.Printf("  %s", out)
	}
}

func cmdStatus(c *client) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	var leases []struct {
		ID            string    `json:"id"`
		DeviceID      string    `json:"device_id"`
		Owner         string    `json:"owner"`
		Worktree      string    `json:"worktree"`
		ExpiresAt     time.Time `json:"expires_at"`
		HumanTakeover bool      `json:"human_takeover"`
		HumanNote     string    `json:"human_note"`
	}
	if _, err := c.do("GET", "/api/leases", nil, &leases); err != nil {
		fatal("查租约失败: %v", err)
	}
	for _, l := range leases {
		if l.ID != s.LeaseID {
			continue
		}
		fmt.Printf("设备 %s（%s）\n租约 %s 到期 %s\n",
			l.DeviceID, s.ADBAddr, l.ID, l.ExpiresAt.Local().Format("15:04:05"))
		if l.HumanTakeover {
			// agent 看到这个应停手，等操作人员交还
			fmt.Printf("⚠️  人工接管中：%s\n", l.HumanNote)
			os.Exit(10)
		}
		return
	}
	fatal("租约 %s 已不存在（可能已过期被回收），请重新 claim", s.LeaseID)
}

func cmdRelease(c *client) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	if _, err := c.do("DELETE", "/api/leases/"+s.LeaseID, nil, nil); err != nil {
		fmt.Fprintf(os.Stderr, "归还接口报错（仍清理本地记录）: %v\n", err)
	}
	_ = os.Remove(stateFile)
	fmt.Printf("已归还设备 %s\n", s.DeviceID)
}

func cmdDevices(c *client) {
	var ds []struct {
		ID      string `json:"id"`
		ADBAddr string `json:"adb_addr"`
		State   string `json:"state"`
	}
	if _, err := c.do("GET", "/api/devices", nil, &ds); err != nil {
		fatal("查设备失败: %v", err)
	}
	for _, d := range ds {
		fmt.Printf("%-12s %-22s %s\n", d.ID, d.ADBAddr, d.State)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func usage() {
	fmt.Print(`droidpool —— 从设备池取一台独占 Android 设备

  claim     取一台设备（幂等：同一 worktree 重复调用复用同一台）
  addr      打印 adb 地址，供 flutter run -d $(droidpool addr)
  status    查看租约；人工接管中时以退出码 10 结束
  release   归还设备
  devices   列出池中所有设备

环境变量:
  DROIDPOOL_URL     控制面地址（默认 http://192.168.14.32:8600）
  DROIDPOOL_TOKEN   鉴权 token（必填）
`)
}
