// droidpool-mcp 是 droidpool 的 MCP server（stdio），给任何支持 MCP 的 agent 用。
//
// 它不重新实现业务，只是把控制面 HTTP API 包成 MCP tool，并把 agent 该知道的
// 用法与坑写进 tool 描述里——agent 不必读文档就能正确用池子。
//
// 配置：DROIDPOOL_URL、DROIDPOOL_TOKEN（与 CLI 相同）。
// 注册示例（Claude Code）：
//
//	claude mcp add droidpool -e DROIDPOOL_URL=http://... -e DROIDPOOL_TOKEN=... -- droidpool-mcp
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fancyCachier/droidpool/internal/localstate"
)

type client struct {
	base, token string
	http        *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	return c.doWith(c.http, ctx, method, path, body)
}

// doWith 用指定的 http.Client 发请求：identity 要同步等设备重建（几十秒），默认 60 s 的客户端不够。
func (c *client) doWith(h *http.Client, ctx context.Context, method, path string, body any) (int, []byte, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, &buf)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// errText 把控制面的错误 JSON 翻成一句话。
func errText(code int, raw []byte) string {
	var e struct{ Error, Message string }
	_ = json.Unmarshal(raw, &e)
	switch code {
	case http.StatusConflict:
		return "池中无空闲设备（409）。等一会儿再试，或去设备墙看谁占着。这不是 bug。"
	case http.StatusServiceUnavailable:
		return "节点内存不足，暂不接受新租约（503）。等别人 release。这不是 bug。"
	case http.StatusUnauthorized:
		return "token 无效（401）：检查 DROIDPOOL_TOKEN。"
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Error, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", code, strings.TrimSpace(string(raw)))
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func fail(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// ---------- tools ----------

type claimIn struct {
	Worktree string `json:"worktree" jsonschema:"worktree 名（通常是仓库/工作区目录名）。同一 host+worktree 重复 claim 返回同一台设备，不会占第二台"`
	Branch   string `json:"branch,omitempty" jsonschema:"当前分支，仅用于设备墙显示"`
	HeadSHA  string `json:"head_sha,omitempty" jsonschema:"当前 HEAD 短 SHA，仅用于设备墙显示"`
	TTLMin   int    `json:"ttl_min,omitempty" jsonschema:"租期分钟数，默认 240。超过 30 分钟的长任务记得定期调 heartbeat"`
}

type claimOut struct {
	LeaseID   string `json:"lease_id"`
	DeviceID  string `json:"device_id"`
	ADBAddr   string `json:"adb_addr"`
	ExpiresAt string `json:"expires_at"`
	Reused    bool   `json:"reused"`
	Next      string `json:"next"`
}

func (c *client) claim(ctx context.Context, _ *mcp.CallToolRequest, in claimIn) (*mcp.CallToolResult, claimOut, error) {
	host, _ := os.Hostname()
	if in.Worktree == "" {
		in.Worktree = filepath.Base(mustCwd())
	}
	code, raw, err := c.do(ctx, "POST", "/api/leases", map[string]any{
		"owner": os.Getenv("USER") + "@" + host, "host": host,
		"worktree": in.Worktree, "branch": in.Branch, "head_sha": in.HeadSHA, "ttl_min": in.TTLMin,
	})
	if err != nil {
		return fail("连不上控制面: " + err.Error()), claimOut{}, nil
	}
	if code >= 400 {
		return fail(errText(code, raw)), claimOut{}, nil
	}
	var out claimOut
	_ = json.Unmarshal(raw, &out)
	// 顺手 adb connect，省 agent 一步；失败不阻断（agent 机器上可能没 adb）
	_ = exec.CommandContext(ctx, "adb", "connect", out.ADBAddr).Run()
	out.Next = fmt.Sprintf("设备已独占。用 `adb -s %s ...` 驱动。装包与首启引导可用 droidpool_run。用完调 droidpool_release。", out.ADBAddr)
	return text(fmt.Sprintf("已分配设备 %s（adb %s，租约 %s，到期 %s%s）\n%s",
		out.DeviceID, out.ADBAddr, out.LeaseID, out.ExpiresAt, map[bool]string{true: "，复用既有租约", false: ""}[out.Reused], out.Next)), out, nil
}

type leaseIn struct {
	LeaseID string `json:"lease_id" jsonschema:"claim 返回的 lease_id"`
}

func (c *client) release(ctx context.Context, _ *mcp.CallToolRequest, in leaseIn) (*mcp.CallToolResult, any, error) {
	code, raw, err := c.do(ctx, "DELETE", "/api/leases/"+in.LeaseID, nil)
	if err != nil {
		return fail(err.Error()), nil, nil
	}
	if code == http.StatusNotFound {
		return text("租约已不存在（可能已过期被 watchdog 回收），无需再 release。"), nil, nil
	}
	if code >= 400 {
		return fail(errText(code, raw)), nil, nil
	}
	return text("已归还，设备正在后台复位（约 20 秒回到 ready）。"), nil, nil
}

func (c *client) heartbeat(ctx context.Context, _ *mcp.CallToolRequest, in leaseIn) (*mcp.CallToolResult, any, error) {
	code, raw, err := c.do(ctx, "POST", "/api/leases/"+in.LeaseID+"/heartbeat", nil)
	if err != nil {
		return fail(err.Error()), nil, nil
	}
	if code >= 400 {
		return fail(errText(code, raw)), nil, nil
	}
	return text("心跳已记录。注意：心跳只证明活着，不延长 TTL。"), nil, nil
}

type statusOut struct {
	Found         bool   `json:"found"`
	DeviceID      string `json:"device_id,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	HumanTakeover bool   `json:"human_takeover"`
	HumanNote     string `json:"human_note,omitempty"`
}

func (c *client) status(ctx context.Context, _ *mcp.CallToolRequest, in leaseIn) (*mcp.CallToolResult, statusOut, error) {
	code, raw, err := c.do(ctx, "GET", "/api/leases", nil)
	if err != nil {
		return fail(err.Error()), statusOut{}, nil
	}
	if code >= 400 {
		return fail(errText(code, raw)), statusOut{}, nil
	}
	var leases []struct {
		ID, DeviceID, ExpiresAt, HumanNote string
		HumanTakeover                      bool `json:"human_takeover"`
	}
	_ = json.Unmarshal(raw, &leases)
	for _, l := range leases {
		if l.ID != in.LeaseID {
			continue
		}
		out := statusOut{Found: true, DeviceID: l.DeviceID, ExpiresAt: l.ExpiresAt, HumanTakeover: l.HumanTakeover, HumanNote: l.HumanNote}
		if l.HumanTakeover {
			return text(fmt.Sprintf("⚠️ 人工接管中（%s）。立刻停止操作设备，操作人员正在设备墙上操作，两边同时点会互相干扰。稍后再查。", l.HumanNote)), out, nil
		}
		return text(fmt.Sprintf("租约有效，设备 %s，到期 %s。", l.DeviceID, l.ExpiresAt)), out, nil
	}
	return text("租约不存在——可能已过期被 watchdog 回收。设备已被复位，之前装的包没了，请重新 claim。"), statusOut{}, nil
}

type runIn struct {
	ADBAddr  string `json:"adb_addr" jsonschema:"claim 返回的 adb 地址"`
	APK      string `json:"apk" jsonschema:"apk 路径"`
	EdgeHost string `json:"edge_host,omitempty" jsonschema:"后端主机；不填用 DROIDPOOL_EDGE_HOST"`
	EdgePort int    `json:"edge_port,omitempty" jsonschema:"后端端口，默认 8090"`
}

// run 委托给 CLI：装包 / 写端点 / 过引导那套逻辑已在 CLI 里验证过，不重写一遍。
func (c *client) run(ctx context.Context, _ *mcp.CallToolRequest, in runIn) (*mcp.CallToolResult, any, error) {
	cli, err := exec.LookPath("droidpool")
	if err != nil {
		return fail("宿主 PATH 里没有 droidpool CLI；run 依赖它做装包与引导。装好后重试，或自己用 adb install + seed 端点。"), nil, nil
	}
	// CLI 靠本地租约记录找设备：写到 CLI 会读的那个位置（worktree 顶层 + 会话键，共用 internal/localstate）。
	// 原先固定写当前目录的 .droidpool：带会话键时 CLI 读的是另一个文件；已有 CLI 记录时还会把 lease_id 冲掉，
	// 之后 droidpool release 就还不了。所以已有记录只更新 adb 地址。
	if err := writeADBAddr(localstate.Path("."), in.ADBAddr); err != nil {
		return fail("写本地租约记录失败: " + err.Error()), nil, nil
	}
	args := []string{"run", "--apk", in.APK}
	if in.EdgeHost != "" {
		args = append(args, "--host", in.EdgeHost)
	}
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Env = os.Environ()
	if in.EdgeHost != "" {
		cmd.Env = append(cmd.Env, "DROIDPOOL_EDGE_HOST="+in.EdgeHost)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fail(string(out)), nil, nil
	}
	return text(string(out)), nil, nil
}

// writeADBAddr 在本地租约记录里写入 adb 地址，保留已有字段（lease_id、device_id 等）。
func writeADBAddr(path, addr string) error {
	rec := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &rec)
	}
	rec["adb_addr"] = addr
	b, _ := json.MarshalIndent(rec, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

func (c *client) devices(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	code, raw, err := c.do(ctx, "GET", "/api/wall", nil)
	if err != nil {
		return fail(err.Error()), nil, nil
	}
	if code >= 400 {
		return fail(errText(code, raw)), nil, nil
	}
	var wall struct {
		Devices []struct {
			ID, State, Worktree, Owner string
		}
		UnderPressure bool `json:"under_pressure"`
	}
	_ = json.Unmarshal(raw, &wall)
	var b strings.Builder
	for _, d := range wall.Devices {
		fmt.Fprintf(&b, "%-12s %-10s", d.ID, d.State)
		if d.Worktree != "" {
			fmt.Fprintf(&b, " %s (%s)", d.Worktree, d.Owner)
		}
		b.WriteString("\n")
	}
	if wall.UnderPressure {
		b.WriteString("⚠️ 节点内存不足，新 claim 会被拒。\n")
	}
	return text(b.String()), nil, nil
}

// devErr 设备接口的错误：控制面给了原因就照抄（这里的 503 是「未接管理」之类，
// 不是 claim 那种内存不足），否则退回 errText。
func devErr(code int, raw []byte) string {
	var e struct{ Error, Message string }
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		return e.Message
	}
	return errText(code, raw)
}

type identityIn struct {
	DeviceID     string `json:"device_id" jsonschema:"claim 返回的 device_id"`
	Model        string `json:"model,omitempty" jsonschema:"Build.MODEL，如 X1。不 reset 时必填"`
	Brand        string `json:"brand,omitempty" jsonschema:"Build.BRAND，如 ACME。brand 与 manufacturer 至少给一个，互相兜底"`
	Manufacturer string `json:"manufacturer,omitempty" jsonschema:"Build.MANUFACTURER，默认同 brand"`
	Device       string `json:"device,omitempty" jsonschema:"Build.DEVICE，默认由型号派生（小写字母数字）"`
	Name         string `json:"name,omitempty" jsonschema:"Build.PRODUCT，默认同 device"`
	Serial       string `json:"serial,omitempty" jsonschema:"序列号（字母数字），默认按设备派生、每台不同"`
	Reset        bool   `json:"reset,omitempty" jsonschema:"true = 撤销覆盖，回到池子默认"`
	ADBAddr      string `json:"adb_addr,omitempty" jsonschema:"claim 返回的 adb 地址；给了就在重建后顺手 adb connect"`
}

type identityOut struct {
	Identity map[string]string `json:"identity,omitempty"`
	Rebuilt  bool              `json:"rebuilt"`
}

func (c *client) identity(ctx context.Context, _ *mcp.CallToolRequest, in identityIn) (*mcp.CallToolResult, identityOut, error) {
	if in.DeviceID == "" {
		return fail("需要 device_id（claim 返回）。"), identityOut{}, nil
	}
	body := map[string]string{}
	if !in.Reset {
		if in.Model == "" {
			return fail("要么给 model（可带 brand / manufacturer / device / name / serial），要么 reset=true。"), identityOut{}, nil
		}
		body = map[string]string{
			"model": in.Model, "brand": in.Brand, "manufacturer": in.Manufacturer,
			"device": in.Device, "name": in.Name, "serial": in.Serial,
		}
	}
	// 重建同步等，60 s 不够；控制面自己的上限是 3 分钟
	code, raw, err := c.doWith(&http.Client{Timeout: 3 * time.Minute}, ctx, "PUT", "/api/devices/"+in.DeviceID+"/identity", body)
	if err != nil {
		return fail("连不上控制面: " + err.Error()), identityOut{}, nil
	}
	if code >= 400 {
		return fail(devErr(code, raw)), identityOut{}, nil
	}
	var out identityOut
	_ = json.Unmarshal(raw, &out)
	var b strings.Builder
	if out.Identity == nil {
		b.WriteString("已回到镜像原样（redroid 默认型号）。")
	} else {
		fmt.Fprintf(&b, "型号: %s / %s / %s（device=%s product=%s serial=%s）。",
			out.Identity["brand"], out.Identity["model"], out.Identity["manufacturer"],
			out.Identity["device"], out.Identity["name"], out.Identity["serial"])
	}
	if !out.Rebuilt {
		b.WriteString("\n型号没变或设备正在重建中，本次未重建。")
		return text(b.String()), out, nil
	}
	b.WriteString("\n设备已重建：数据清空、之前装的包没了，接着用 droidpool_run 装包。")
	if in.ADBAddr != "" {
		// 容器换了，adb 那头的连接已断，顺手接回来；失败不阻断
		_ = exec.CommandContext(ctx, "adb", "connect", in.ADBAddr).Run()
	}
	return text(b.String()), out, nil
}

type locationIn struct {
	DeviceID string `json:"device_id" jsonschema:"claim 返回的 device_id"`
	Location string `json:"location,omitempty" jsonschema:"纬度,经度（WGS84），如 23.1291,113.2644；留空 = 撤销覆盖，回到池子默认"`
}

func (c *client) location(ctx context.Context, _ *mcp.CallToolRequest, in locationIn) (*mcp.CallToolResult, any, error) {
	if in.DeviceID == "" {
		return fail("需要 device_id（claim 返回）。"), nil, nil
	}
	code, raw, err := c.do(ctx, "PUT", "/api/devices/"+in.DeviceID+"/location", map[string]string{"location": in.Location})
	if err != nil {
		return fail("连不上控制面: " + err.Error()), nil, nil
	}
	if code >= 400 {
		return fail(devErr(code, raw)), nil, nil
	}
	var out struct {
		MockLocation string `json:"mock_location"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.MockLocation == "" {
		return text("已撤销定位覆盖，回到池子默认。"), nil, nil
	}
	return text(fmt.Sprintf("mock 定位: %s（gps 与 network provider，Location.isMock() 为 true）。", out.MockLocation)), nil, nil
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "unknown"
	}
	return d
}

func main() {
	base := strings.TrimRight(os.Getenv("DROIDPOOL_URL"), "/")
	token := os.Getenv("DROIDPOOL_TOKEN")
	if base == "" || token == "" {
		log.Fatal("需要 DROIDPOOL_URL 与 DROIDPOOL_TOKEN 环境变量")
	}
	c := &client{base: base, token: token, http: &http.Client{Timeout: 60 * time.Second}}

	srv := mcp.NewServer(&mcp.Implementation{Name: "droidpool", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: `droidpool 给每个 agent 一台独占的 Android 设备。要在 Android 上验证应用时：
1. droidpool_claim（传 worktree 名）拿设备，返回 adb_addr 与 lease_id
2. droidpool_run 装包、写后端端点、过首启引导到登录页
3. 用 adb -s <adb_addr> 驱动 UI
4. droidpool_release 归还
要改设备报的机型（Build.MODEL 等）用 droidpool_identity：在 claim 之后、run 之前做，会重建设备；mock 定位用 droidpool_location，即时生效。
别绕过池子直接 adb connect 节点。30 分钟无活动租约会被收回，长任务定期 droidpool_heartbeat。
droidpool_status 报人工接管时立刻停手。池里测不了微信登录/扫码/蓝牙/USB。`,
	})

	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_claim",
		Description: "从设备池取一台独占 Android 设备。幂等：同一 worktree 重复调用返回同一台。返回 adb_addr 供后续 adb -s 使用。"}, c.claim)
	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_run",
		Description: "在已 claim 的设备上：安装 apk → 写后端端点与证书 pin → 启动 → 自动过首启两步引导到登录页。登录本身由你驱动。"}, c.run)
	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_status",
		Description: "查租约是否仍有效、是否被人工接管。接管中必须停止操作设备。"}, c.status)
	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_heartbeat",
		Description: "告诉 watchdog 自己还活着（30 分钟无活动会被收回）。不延长 TTL。"}, c.heartbeat)
	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_release",
		Description: "归还设备。设备会被清空重建，装的包与登录态全部消失。"}, c.release)
	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_devices",
		Description: "列出池中所有设备及占用情况，看池满时谁占着。"}, c.devices)
	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_identity",
		Description: "改设备对外报的硬件型号（Build.MODEL/BRAND/MANUFACTURER/DEVICE/PRODUCT + 序列号）。会重建设备：数据清空、租约保留、adb 地址不变，约 20~40 s，所以在 claim 之后、droidpool_run 之前做。reset=true 撤销覆盖。"}, c.identity)
	mcp.AddTool(srv, &mcp.Tool{Name: "droidpool_location",
		Description: "给设备设 mock 定位（纬度,经度），即时生效，复位后由控制面重放；Location.isMock() 为 true，高德/百度 SDK 默认丢弃 mock 位置。location 留空 = 撤销覆盖。"}, c.location)

	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
