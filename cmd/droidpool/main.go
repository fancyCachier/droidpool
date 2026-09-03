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
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	// help 不该要 token —— 想看用法的人往往正是还没配好环境的人
	switch os.Args[1] {
	case "-h", "--help", "help":
		usage()
		return
	}
	// 不给内网默认值：这是开源项目，偷偷回退到某个 IP 只会让外部用户连到别人的内网
	base := os.Getenv("DROIDPOOL_URL")
	if base == "" {
		fatal("未设置 DROIDPOOL_URL（控制面地址，如 http://droidpool.example:8600）")
	}
	token := os.Getenv("DROIDPOOL_TOKEN")
	if token == "" {
		fatal("未设置 DROIDPOOL_TOKEN（在控制面的 /opt/droidpool/env 里）")
	}
	c := &client{base: strings.TrimRight(base, "/"), token: token}

	switch os.Args[1] {
	case "claim":
		cmdClaim(c)
	case "addr":
		go touchIfLeased(c) // 顺手证明自己还活着
		s, err := loadState()
		if err != nil {
			fatal("%v", err)
		}
		fmt.Println(s.ADBAddr)
	case "status":
		touchIfLeased(c)
		cmdStatus(c)
	case "release":
		cmdRelease(c)
	case "seed-edge":
		touchIfLeased(c)
		cmdSeedEdge(os.Args[2:])
	case "run":
		touchIfLeased(c)
		cmdRun(os.Args[2:])
	case "watch":
		cmdWatch(c)
	case "heartbeat":
		touchIfLeased(c)
	case "devices":
		touchIfLeased(c)
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

// heartbeat 告诉 watchdog「这个 agent 还活着」。失败只提示不中断——
// 心跳失败最坏是设备被提前回收，不该让 agent 的正常操作也失败。
func (c *client) heartbeat(leaseID string) {
	if _, err := c.do("POST", "/api/leases/"+leaseID+"/heartbeat", nil, nil); err != nil {
		fmt.Fprintf(os.Stderr, "心跳失败（设备可能被提前回收）: %v\n", err)
	}
}

// touchIfLeased 每条命令顺手发一次心跳，让「agent 在干活」这件事被看见。
func touchIfLeased(c *client) {
	if s, err := loadState(); err == nil && s.LeaseID != "" {
		c.heartbeat(s.LeaseID)
	}
}

func cmdWatch(c *client) {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	every := 60 * time.Second
	if v := os.Getenv("DROIDPOOL_HEARTBEAT_SEC"); v != "" {
		if n, e := time.ParseDuration(v + "s"); e == nil && n > 0 {
			every = n
		}
	}
	fmt.Printf("持续心跳中（每 %s 一次，Ctrl-C 停止）：租约 %s 设备 %s\n", every, s.LeaseID, s.DeviceID)
	for {
		c.heartbeat(s.LeaseID)
		time.Sleep(every)
	}
}

// adbDev 对本租约设备跑 adb，等价于 `adb -s $(droidpool addr) ...`。
func adbDev(args ...string) *exec.Cmd {
	s, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	return exec.Command("adb", append([]string{"-s", s.ADBAddr}, args...)...)
}

// edgeCertPin 取 Edge 证书的 DER SHA-256，与 cashier-app 的 TOFU pin 格式一致。
func edgeCertPin(host string, port int) (string, error) {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp",
		fmt.Sprintf("%s:%d", host, port), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // 自签证书，取指纹本就不该校验
	if err != nil {
		return "", err
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("Edge 没有返回证书")
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

const cashierPkg = "cn.daboshi.cashier_app.dev"

// cmdSeedEdge 给已装的 cashier-app 写入 Edge 端点与证书 pin，免走引导页。
//
// 写的是 shared_prefs/FlutterSharedPreferences.xml 的两个 key，格式与 app 一致；
// run-as 里相对路径的 cwd 不可靠，一律 push 到 /data/local/tmp 再用绝对路径 cp。
func cmdSeedEdge(args []string) {
	fs := flag.NewFlagSet("seed-edge", flag.ExitOnError)
	host := fs.String("host", os.Getenv("DROIDPOOL_EDGE_HOST"), "Edge 主机（或设 DROIDPOOL_EDGE_HOST）")
	port := fs.Int("port", 8090, "Edge 端口")
	pkg := fs.String("pkg", cashierPkg, "应用包名")
	fs.Parse(args)
	if *host == "" {
		fatal("未指定 Edge 主机：--host 或 DROIDPOOL_EDGE_HOST")
	}

	pin, err := edgeCertPin(*host, *port)
	if err != nil {
		fatal("取 %s:%d 的证书 pin 失败: %v", *host, *port, err)
	}
	xml := fmt.Sprintf(`<?xml version='1.0' encoding='utf-8' standalone='yes' ?>
<map>
    <string name="flutter.edge_endpoint_v1">{"host":"%s","port":%d}</string>
    <string name="flutter.edge_cert_pins_v1">{"%s:%d":"%s"}</string>
</map>
`, *host, *port, *host, *port, pin)
	tmp, err := os.CreateTemp("", "fsp-*.xml")
	if err != nil {
		fatal("%v", err)
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(xml)
	tmp.Close()

	if out, err := adbDev("push", tmp.Name(), "/data/local/tmp/fsp.xml").CombinedOutput(); err != nil {
		fatal("push 失败: %v\n%s", err, out)
	}
	prefs := "/data/data/" + *pkg + "/shared_prefs"
	sh := fmt.Sprintf("run-as %s mkdir -p %s && run-as %s cp /data/local/tmp/fsp.xml %s/FlutterSharedPreferences.xml && am force-stop %s",
		*pkg, prefs, *pkg, prefs, *pkg)
	if out, err := adbDev("shell", sh).CombinedOutput(); err != nil {
		fatal("写入 shared_prefs 失败（包装了吗？）: %v\n%s", err, out)
	}
	fmt.Printf("已写入 Edge 端点 %s:%d（pin %s…）\n", *host, *port, pin[:12])
}

// cmdRun 一步到位：装包 → 写 Edge 端点 → 启动 → 自动过掉首启的两步引导。
//
// 设备每次 claim 都是干净的（上个租约归还时数据目录被清空），所以这几步每次都要做。
// 首启引导：隐私政策「同意并继续」→ 设备角色「共享收银机」→ 登录页。
// 这里只把 agent 送到登录页；登录要选员工、输 PIN，属于验证流程的一部分，由 agent 自己做。
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	apk := fs.String("apk", "build/app/outputs/flutter-apk/app-debug.apk", "apk 路径")
	pkg := fs.String("pkg", cashierPkg, "应用包名")
	activity := fs.String("activity", "cn.daboshi.cashier_app.MainActivity", "启动 Activity")
	noSeed := fs.Bool("no-seed", false, "不写 Edge 端点（走引导页手填）")
	noOnboard := fs.Bool("no-onboard", false, "不自动过引导页")
	fs.Parse(args)

	if _, err := os.Stat(*apk); err != nil {
		fatal("找不到 apk %s（先 flutter build apk --debug --target-platform android-arm64）", *apk)
	}
	fmt.Printf("→ 安装 %s\n", *apk)
	if out, err := adbDev("install", "-r", "-t", *apk).CombinedOutput(); err != nil {
		fatal("安装失败: %v\n%s", err, out)
	}
	if !*noSeed {
		fmt.Println("→ 写入 Edge 端点")
		cmdSeedEdge(nil)
	}
	fmt.Println("→ 启动")
	if out, err := adbDev("shell", "am", "start", "-W", "-n", *pkg+"/"+*activity).CombinedOutput(); err != nil {
		fatal("启动失败: %v\n%s", err, out)
	}
	if *noOnboard {
		return
	}
	fmt.Println("→ 过引导页")
	for i := 0; i < 8; i++ {
		time.Sleep(2 * time.Second)
		descs := uiDescs()
		switch {
		case containsAny(descs, "jingli", "选择员工", "测试并连接"):
			fmt.Println("已到登录页，接下来选员工、输 PIN 由你来")
			return
		case containsAny(descs, "同意并继续"):
			tapDesc("同意并继续")
		case containsAny(descs, "这台设备是"):
			tapDescFragment("共享收银机")
		}
	}
	fmt.Println("引导页状态未知，用 droidpool ui-dump 看一眼")
}

// uiDescs 取当前界面所有 content-desc（uiautomator dump，约 2.6 s）。
func uiDescs() string {
	out, _ := adbDev("shell", "rm -f /sdcard/ui.xml; uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; cat /sdcard/ui.xml").Output()
	return string(out)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// boundsCenter 从 dump 里找 content-desc 匹配的节点，返回中心坐标。exact 决定精确还是片段匹配。
func boundsCenter(dump, desc string, exact bool) (int, int, bool) {
	var pat string
	if exact {
		pat = `content-desc="` + regexp.QuoteMeta(desc) + `"[^>]*bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"`
	} else {
		pat = `content-desc="[^"]*` + regexp.QuoteMeta(desc) + `[^"]*"[^>]*bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"`
	}
	m := regexp.MustCompile(pat).FindStringSubmatch(dump)
	if m == nil {
		return 0, 0, false
	}
	x1, _ := strconv.Atoi(m[1])
	y1, _ := strconv.Atoi(m[2])
	x2, _ := strconv.Atoi(m[3])
	y2, _ := strconv.Atoi(m[4])
	return (x1 + x2) / 2, (y1 + y2) / 2, true
}

func tapDesc(desc string) {
	if x, y, ok := boundsCenter(uiDescs(), desc, true); ok {
		adbDev("shell", "input", "tap", strconv.Itoa(x), strconv.Itoa(y)).Run()
	}
}

func tapDescFragment(desc string) {
	if x, y, ok := boundsCenter(uiDescs(), desc, false); ok {
		adbDev("shell", "input", "tap", strconv.Itoa(x), strconv.Itoa(y)).Run()
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
  seed-edge 给已装的 cashier-app 写 Edge 端点 + 证书 pin（免走引导页）
            [--host <edge-host>] [--port 8090]，或设 DROIDPOOL_EDGE_HOST
  run       一步到位：装包 → seed-edge → 启动 → 自动过引导页到登录页
            [--apk build/app/outputs/flutter-apk/app-debug.apk] [--no-seed] [--no-onboard]
  heartbeat 发一次心跳（告诉 watchdog 自己还活着）
  watch     持续心跳，跑长任务时后台挂着，防止被空闲闸回收

环境变量:
  DROIDPOOL_URL       控制面地址（必填，如 http://droidpool.example:8600）
  DROIDPOOL_TOKEN     鉴权 token（必填）
  DROIDPOOL_EDGE_HOST seed-edge / run 写入的后端主机（或用 --host）
  DROIDPOOL_HEARTBEAT_SEC  watch 的心跳间隔秒数（默认 60）

watchdog：控制面会回收「久未活动」的租约（默认空闲 30 分钟），防止 agent
僵死后一直占着机器。每条 droidpool 命令都会顺手发心跳；跑长任务时用
droidpool watch & 挂个后台心跳。
`)
}
