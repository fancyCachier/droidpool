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
	"context"
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

	"github.com/fancyCachier/droidpool/internal/uiagent"
)

// stateFile 本地租约记录的路径。设了会话键就带后缀，多个会话共用一个目录时各记各的。
func stateFile() string {
	if s := sessionKey(); s != "" {
		return ".droidpool." + s
	}
	return ".droidpool"
}

// sessionKey 返回 DROIDPOOL_SESSION（已做文件名安全化），未设置时为空。
//
// 幂等键原本只有 (host, worktree)，假设「同一主机同一目录再来一次 = 同一个 agent 在重试」。
// dsh 这类多会话宿主在同一台机器、同一个检出里跑好几个 agent，这个假设不成立：
// 后来者全都「复用既有租约」挤到同一台设备上（2026-09-04 线上 20 条租约 19 条在 1 号机）。
// 会话键把每个会话分开，本地记录也随之分开；dsh 插件会自动注入，人手工用时可以不设。
func sessionKey() string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, os.Getenv("DROIDPOOL_SESSION"))
}

// claimWorktree 组装发给控制面的幂等键后半段：worktree 名，带会话键时加 @会话。
func claimWorktree(worktree, session string) string {
	if session == "" {
		return worktree
	}
	return worktree + "@" + session
}

type client struct {
	base  string
	token string
}

func (c *client) do(method, path string, body any, out any) (int, error) {
	return c.doWithTimeout(30*time.Second, method, path, body, out)
}

func (c *client) doWithTimeout(timeout time.Duration, method, path string, body any, out any) (int, error) {
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
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
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

// gitInfo 从当前目录推导仓库顶层目录、worktree 名、分支与 HEAD。
func gitInfo() (top, worktree, branch, head string) {
	run := func(args ...string) string {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	top = run("rev-parse", "--show-toplevel")
	if top != "" {
		worktree = filepath.Base(top)
	}
	return top, worktree, run("rev-parse", "--abbrev-ref", "HEAD"), run("rev-parse", "--short", "HEAD")
}

type leaseState struct {
	LeaseID  string `json:"lease_id"`
	DeviceID string `json:"device_id"`
	ADBAddr  string `json:"adb_addr"`
}

func saveState(s leaseState) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(stateFile(), b, 0o600)
}

func loadState() (leaseState, error) {
	return loadStateFrom(stateFile())
}

func loadStateFrom(path string) (leaseState, error) {
	var s leaseState
	b, err := os.ReadFile(path)
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
	case "battery":
		touchIfLeased(c)
		cmdBattery(os.Args[2:])
	case "ui-dump":
		touchIfLeased(c)
		cmdUIDump(os.Args[2:])
	case "camera":
		touchIfLeased(c)
		cmdCamera(c, os.Args[2:])
	case "identity":
		touchIfLeased(c)
		cmdIdentity(c, os.Args[2:])
	case "location":
		touchIfLeased(c)
		cmdLocation(c, os.Args[2:])
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
	top, wt, branch, head := gitInfo()
	if wt == "" {
		fatal("当前目录不是 git 仓库，无法推导 worktree 名")
	}
	host, _ := os.Hostname()
	req := map[string]any{
		"owner": os.Getenv("USER") + "@" + host, "host": host,
		"worktree": claimWorktree(wt, sessionKey()), "branch": branch, "head_sha": head,
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
	if resp.Reused && !heldHere(resp.LeaseID, top) {
		fmt.Fprint(os.Stderr, reuseWarning)
	}
	if err := saveState(resp.leaseState); err != nil {
		fatal("写 %s 失败: %v", stateFile(), err)
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

const reuseWarning = `⚠️  复用了既有租约，但本地没有它的记录：这台设备多半正被同一主机上的另一个会话持有，
   两边装包会互相覆盖。多个会话共用一个检出时，给每个会话设不同的 DROIDPOOL_SESSION
  （dsh 插件会自动注入），或者到各自的 worktree 里 claim。
`

// heldHere 报告复用到的租约是不是本目录（或本 worktree 顶层）自己 claim 出来的。
// 都不是，就是幂等键撞了：同一主机上另一个会话正拿着这台设备。
// 两处都查是因为 agent 常在 worktree 顶层 claim、再进子目录干活。
func heldHere(leaseID, top string) bool {
	for _, dir := range []string{".", top} {
		if s, err := loadStateFrom(filepath.Join(dir, stateFile())); err == nil && s.LeaseID == leaseID {
			return true
		}
	}
	return false
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
// cmdCamera 设置这台设备的摄像头画面源。
//
// 走控制面而不是自己去节点上起 ffmpeg：推流容器要跟着设备的生命周期走，
// 设备复位重建后控制面会按库里的记录把它重放回来，agent 自己起的野进程做不到
// ——设备换了而流还对着旧的 /dev/videoN 灌，画面就永远黑着。
func cmdCamera(c *client, args []string) {
	fs := flag.NewFlagSet("camera", flag.ExitOnError)
	rtsp := fs.String("rtsp", "", "画面源，如 rtsp://host/live；留空配合 --off 表示停流")
	off := fs.Bool("off", false, "停流")
	fs.Parse(args)
	if !*off && *rtsp == "" {
		fatal("要么 --rtsp <地址>，要么 --off")
	}
	src := *rtsp
	if *off {
		src = ""
	}
	st, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	var resp struct {
		CameraRTSP string `json:"camera_rtsp"`
	}
	if _, err := c.do("PUT", "/api/devices/"+st.DeviceID+"/camera",
		map[string]string{"rtsp": src}, &resp); err != nil {
		fatal("设置摄像头失败: %v", err)
	}
	if resp.CameraRTSP == "" {
		fmt.Println("已停流（相机随之报 0 个设备）")
	} else {
		fmt.Printf("摄像头画面源: %s\n", resp.CameraRTSP)
	}
}

// cmdIdentity 改这台设备对外报的硬件型号（Build.MODEL / BRAND / …）。
//
// 型号是开机定死的属性，控制面会**重建**这台设备来让它生效：数据清空、
// 租约保留、adb 地址不变。所以要在 claim 之后、装包之前做，做完重连 adb。
func cmdIdentity(c *client, args []string) {
	fs := flag.NewFlagSet("identity", flag.ExitOnError)
	model := fs.String("model", "", "Build.MODEL，如 X1（必填，除非 --reset）")
	brand := fs.String("brand", "", "Build.BRAND，如 ACME（与 --manufacturer 至少给一个，互相兜底）")
	manufacturer := fs.String("manufacturer", "", "Build.MANUFACTURER，默认同 --brand")
	device := fs.String("device", "", "Build.DEVICE，默认由型号派生（小写字母数字）")
	name := fs.String("name", "", "Build.PRODUCT，默认同 --device")
	serial := fs.String("serial", "", "序列号（字母数字），默认按设备 id 派生、每台不同")
	reset := fs.Bool("reset", false, "撤销覆盖，回到池子默认")
	fs.Parse(args)
	body := map[string]string{}
	if !*reset {
		if *model == "" {
			fatal("要么 --model <型号> [--brand <品牌>]，要么 --reset")
		}
		body = map[string]string{
			"model": *model, "brand": *brand, "manufacturer": *manufacturer,
			"device": *device, "name": *name, "serial": *serial,
		}
	}
	st, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	fmt.Println("→ 设备将按新型号重建（数据清空、租约保留），约 20~40 s…")
	var resp struct {
		Identity *struct {
			Model, Brand, Manufacturer, Device, Name, Serial string
		} `json:"identity"`
		Rebuilt bool `json:"rebuilt"`
	}
	// 重建同步等，30 s 不够
	if _, err := c.doWithTimeout(3*time.Minute, "PUT", "/api/devices/"+st.DeviceID+"/identity", body, &resp); err != nil {
		fatal("设置型号失败: %v", err)
	}
	if resp.Identity == nil {
		fmt.Println("已回到镜像原样（redroid 默认型号）")
	} else {
		id := resp.Identity
		fmt.Printf("型号: %s / %s / %s（device=%s product=%s serial=%s）\n",
			id.Brand, id.Model, id.Manufacturer, id.Device, id.Name, id.Serial)
	}
	if !resp.Rebuilt {
		fmt.Println("型号没变或设备正在重建中，本次未重建")
		return
	}
	// 容器换了，adb 那头的连接已断，顺手接回来
	if out, err := exec.Command("adb", "connect", st.ADBAddr).CombinedOutput(); err == nil {
		fmt.Printf("  %s", out)
	}
}

// cmdLocation 给这台设备设 mock 定位，即时生效、复位后由控制面重放。
//
// 走 Android 自带的 test provider：Location.isMock() 为 true，高德/百度 SDK
// 默认会丢弃 mock 位置（高德要 setMockEnable(true)）。
func cmdLocation(c *client, args []string) {
	fs := flag.NewFlagSet("location", flag.ExitOnError)
	off := fs.Bool("off", false, "撤销覆盖，回到池子默认（池子也没配就是不 mock）")
	fs.Parse(args)
	loc := ""
	if !*off {
		if fs.NArg() != 1 {
			fatal("用法：droidpool location <纬度,经度>（如 23.1291,113.2644）| --off")
		}
		loc = fs.Arg(0)
	}
	st, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	var resp struct {
		MockLocation string `json:"mock_location"`
	}
	if _, err := c.do("PUT", "/api/devices/"+st.DeviceID+"/location",
		map[string]string{"location": loc}, &resp); err != nil {
		fatal("设置定位失败: %v", err)
	}
	if resp.MockLocation == "" {
		fmt.Println("已撤销定位覆盖，回到池子默认")
	} else {
		fmt.Printf("mock 定位: %s（gps 与 network provider，isMock=true）\n", resp.MockLocation)
	}
}

// cmdUIDump 取一次界面层级 XML。
//
// 走常驻 agent 而不是 `uiautomator dump`：后者每次都要新起 ART 进程再加载框架 jar，
// 热设备上一次约 380 ms，而树本身只有几十个节点（2026-09-07 实测）。常驻之后约 25 ms。
// 代价是每次调用要拉起一次 agent（约 1~2 s），所以单次调用反而更慢——
// 值当的是 --watch 这种连着取多次的用法，以及后续把会话挂在租约上复用。
func cmdUIDump(args []string) {
	fs := flag.NewFlagSet("ui-dump", flag.ExitOnError)
	dex := fs.String("dex", os.Getenv("DROIDPOOL_UIAGENT_DEX"), "uiagent.dex 路径（或设 DROIDPOOL_UIAGENT_DEX）")
	port := fs.Int("port", 27400, "adb forward 用的本机端口")
	n := fs.Int("n", 1, "连取几次（>1 时每次之间不重启 agent，用来看真实开销）")
	fs.Parse(args)
	if *dex == "" {
		fatal("未指定 uiagent.dex：--dex 或 DROIDPOOL_UIAGENT_DEX（用 device/uiagent/build.sh 编）")
	}
	st, err := loadState()
	if err != nil {
		fatal("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := uiagent.Start(ctx, uiagent.Options{
		Serial: st.ADBAddr, DexPath: *dex, LocalPort: *port,
	})
	if err != nil {
		fatal("启动 uiagent: %v", err)
	}
	defer s.Close()
	for i := 0; i < *n; i++ {
		start := time.Now()
		xml, err := s.Dump()
		if err != nil {
			fatal("dump: %v", err)
		}
		if *n > 1 {
			fmt.Fprintf(os.Stderr, "第 %d 次 %d ms，%d 字节\n", i+1, time.Since(start).Milliseconds(), len(xml))
		}
		fmt.Println(xml)
	}
}

// cmdBattery 给设备伪造一块电池。redroid 默认 present=false、level=0，
// 也就是「没有电池」——需要看电量的应用在这上面拿到的是 0。
//
// 顺序不能反：**level 必须先于 present 设置**。实测（2026-09-06，一次性容器）
// 单独执行 `cmd battery set present 1` 而 level 还是 0 时，Android 会判定电量
// 耗尽并发起关机，容器约 20 s 后以 130（SIGINT，即正常关机）退出。关机是异步的，
// 期间后续命令还能正常返回，所以现场看起来像是「后面某条命令搞的」，很难归因。
// 同样实测：level=0 即使插着电（ac=1、status=charging）也照样关机，
// 所以这里直接把 0 挡在外面，而不是靠「插电就安全」这种假设。
func cmdBattery(args []string) {
	fs := flag.NewFlagSet("battery", flag.ExitOnError)
	level := fs.Int("level", -1, "电量百分比 1~100")
	status := fs.String("status", "discharging", "charging | discharging | full")
	temp := fs.Float64("temp", 0, "电池温度摄氏度，如 31.5；不给则不设")
	reset := fs.Bool("reset", false, "撤销伪造，回到 redroid 默认的「无电池」")
	fs.Parse(args)

	if *reset {
		runBattery("reset")
		fmt.Println("已撤销电池伪造，回到默认的无电池状态")
		return
	}
	if *level < 1 || *level > 100 {
		fatal("--level 必须在 1~100：0 会让 Android 判定电量耗尽并关机（插着电也一样），设备会直接退出")
	}
	st, ok := map[string]string{"charging": "2", "discharging": "3", "full": "5"}[*status]
	if !ok {
		fatal("--status 只能是 charging / discharging / full，收到 %q", *status)
	}

	// level 先于 present，见上面的注释。
	runBattery("set", "level", strconv.Itoa(*level))
	runBattery("set", "present", "1") // 只吃 int，给 "true" 会报 Bad value
	runBattery("set", "status", st)
	if *status == "charging" {
		runBattery("set", "ac", "1")
	} else {
		runBattery("unplug")
	}
	if *temp != 0 {
		// 框架里的单位是 0.1 °C
		runBattery("set", "temp", strconv.Itoa(int(*temp*10)))
	}
	fmt.Printf("电池：%d%% %s\n", *level, *status)
}

func runBattery(args ...string) {
	out, err := adbDev(append([]string{"shell", "cmd", "battery"}, args...)...).CombinedOutput()
	// cmd battery 出错时退出码仍是 0，只在 stdout 上写 Bad value / Unknown set option，
	// 所以退出码和输出都要看。
	if err != nil || bytes.Contains(out, []byte("Bad value")) || bytes.Contains(out, []byte("Unknown")) {
		fatal("cmd battery %s 失败: %v\n%s", strings.Join(args, " "), err, out)
	}
}

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
	_ = os.Remove(stateFile())
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

  claim     取一台设备（幂等：同一主机 + worktree [+ 会话键] 重复调用复用同一台）
  addr      打印 adb 地址，供 flutter run -d $(droidpool addr)
  status    查看租约；人工接管中时以退出码 10 结束
  release   归还设备
  devices   列出池中所有设备
  seed-edge 给已装的 cashier-app 写 Edge 端点 + 证书 pin（免走引导页）
            [--host <edge-host>] [--port 8090]，或设 DROIDPOOL_EDGE_HOST
  battery   伪造一块电池（redroid 默认没有电池，应用读到的电量是 0）
            [--level 1~100] [--status charging|discharging|full] [--temp 31.5] | --reset
  ui-dump   取界面层级 XML。走常驻 agent，单次约 25 ms（uiautomator dump 约 380 ms）
            [--dex uiagent.dex] [--n 1]，或设 DROIDPOOL_UIAGENT_DEX
  camera    给设备接一路 RTSP 当摄像头（redroid 自身没有摄像头）
            --rtsp rtsp://host/live | --off
  identity  改设备对外报的硬件型号（Build.MODEL/BRAND/MANUFACTURER/DEVICE/PRODUCT + 序列号）
            --model X1 --brand ACME [--manufacturer] [--device] [--name] [--serial] | --reset
            会重建设备（数据清空、租约保留、adb 地址不变），在 claim 之后、装包之前做
  location  设 mock 定位，即时生效（Location.isMock() 为 true）
            <纬度,经度> | --off
  run       一步到位：装包 → seed-edge → 启动 → 自动过引导页到登录页
            [--apk build/app/outputs/flutter-apk/app-debug.apk] [--no-seed] [--no-onboard]
  heartbeat 发一次心跳（告诉 watchdog 自己还活着）
  watch     持续心跳，跑长任务时后台挂着，防止被空闲闸回收

环境变量:
  DROIDPOOL_URL       控制面地址（必填，如 http://droidpool.example:8600）
  DROIDPOOL_TOKEN     鉴权 token（必填）
  DROIDPOOL_EDGE_HOST seed-edge / run 写入的后端主机（或用 --host）
  DROIDPOOL_UIAGENT_DEX  ui-dump 用的 uiagent.dex 路径（device/uiagent/build.sh 编出来）
  DROIDPOOL_HEARTBEAT_SEC  watch 的心跳间隔秒数（默认 60）
  DROIDPOOL_SESSION   会话键（可选）。几个 agent 共用一台机器、一个检出时给每个会话设不同的值，
                      否则它们会复用同一条租约挤在一台设备上；dsh 插件会自动注入

watchdog：控制面会回收「久未活动」的租约（默认空闲 30 分钟），防止 agent
僵死后一直占着机器。每条 droidpool 命令都会顺手发心跳；跑长任务时用
droidpool watch & 挂个后台心跳。
`)
}
