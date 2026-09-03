// Package adb 封装设备墙需要的 adb 操作：截图与输入注入。
// 控制面跑在哪台机器，adb 就在哪台跑；设备是 <ip:port> 形式的网络设备。
package adb

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // screencap -p 出的是 PNG
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Runner 执行 adb 命令，抽出接口便于测试。
type Runner interface {
	Output(ctx context.Context, args ...string) ([]byte, error)
}

type execRunner struct{ bin string }

func (r execRunner) Output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("adb %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type Client struct {
	Runner Runner
	// 按设备加锁，而不是一把全局锁。同一台设备上并发 screencap 会拿到半截
	// 数据（设备墙上表现为花屏），但不同设备之间没有这个问题——用全局锁会
	// 把 N 台设备的抓图排成一队，设备墙的帧率被硬生生除以 N。
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(bin string) *Client {
	if bin == "" {
		bin = "adb"
	}
	return &Client{Runner: execRunner{bin: bin}}
}

// capLockFor 取某台设备的**抓图**锁。
//
// 只有 screencap 需要串行：同一设备并发抓图会拿到半截帧，画面花屏。
// input（tap/swipe/key/text）不读帧缓冲，和抓图没有共享状态，**绝不能**
// 跟抓图抢同一把锁——连续流几乎一直握着抓图锁，输入排在后面会被拖到
// 1.7 s 才发出去（实测），人点下去像没反应，于是重复点造成误操作。
func (c *Client) capLockFor(serial string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks == nil {
		c.locks = map[string]*sync.Mutex{}
	}
	m, ok := c.locks[serial]
	if !ok {
		m = &sync.Mutex{}
		c.locks[serial] = m
	}
	return m
}

// run 不加锁，供输入等不需要串行的操作使用。
func (c *Client) run(ctx context.Context, serial string, args ...string) ([]byte, error) {
	full := append([]string{"-s", serial}, args...)
	return c.Runner.Output(ctx, full...)
}

// runCapture 串行化同一设备上的抓图。
func (c *Client) runCapture(ctx context.Context, serial string, args ...string) ([]byte, error) {
	m := c.capLockFor(serial)
	m.Lock()
	defer m.Unlock()
	return c.run(ctx, serial, args...)
}

// Connect 连接网络设备（幂等，已连时也返回成功）。
func (c *Client) Connect(ctx context.Context, addr string) error {
	out, err := c.Runner.Output(ctx, "connect", addr)
	if err != nil {
		return err
	}
	if s := string(out); strings.Contains(s, "failed") || strings.Contains(s, "refused") {
		return fmt.Errorf("连接 %s 失败: %s", addr, strings.TrimSpace(s))
	}
	return nil
}

// ScreencapPNG 抓一张全分辨率截图（PNG）。
func (c *Client) ScreencapPNG(ctx context.Context, serial string) ([]byte, error) {
	return c.runCapture(ctx, serial, "exec-out", "screencap", "-p")
}

// pngSize 从 PNG 头部读出宽高，不做全图解码。
// IHDR 紧跟 8 字节签名，宽高各 4 字节大端。
func pngSize(b []byte) (image.Point, bool) {
	if len(b) < 24 || string(b[1:4]) != "PNG" {
		return image.Point{}, false
	}
	w := int(b[16])<<24 | int(b[17])<<16 | int(b[18])<<8 | int(b[19])
	h := int(b[20])<<24 | int(b[21])<<16 | int(b[22])<<8 | int(b[23])
	if w <= 0 || h <= 0 {
		return image.Point{}, false
	}
	return image.Point{X: w, Y: h}, true
}

// ScreenshotPassthrough 原样返回设备给出的 PNG，只从头部读尺寸。
//
// 全分辨率场景（放大操作视图）不需要缩放，解码再编码是纯浪费：实测
// adb 抓一帧 0.35 s，而服务端转码又要 0.3 s，帧率直接对半砍。
// 直通后帧时间就等于 adb 的 0.35 s。
func (c *Client) ScreenshotPassthrough(ctx context.Context, serial string) ([]byte, image.Point, error) {
	raw, err := c.ScreencapPNG(ctx, serial)
	if err != nil {
		return nil, image.Point{}, err
	}
	size, ok := pngSize(raw)
	if !ok {
		return nil, image.Point{}, fmt.Errorf("设备返回的不是合法 PNG（可能正在重启），%d 字节", len(raw))
	}
	return raw, size, nil
}

// ScreenshotJPEG 抓图并缩放成 JPEG。设备墙上一屏几台设备，全分辨率 PNG
// 太大（1366×768 约 88 KB，8 台 1 fps 就是 700 KB/s），缩到 480 宽的 JPEG
// 约 20 KB，肉眼看「在哪一页」完全够用。
// maxW <= 0 表示不缩放，quality <= 0 用 70。
func (c *Client) ScreenshotJPEG(ctx context.Context, serial string, maxW, quality int) ([]byte, image.Point, error) {
	raw, err := c.ScreencapPNG(ctx, serial)
	if err != nil {
		return nil, image.Point{}, err
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, image.Point{}, fmt.Errorf("解码截图失败（设备可能刚重启）: %w", err)
	}
	size := src.Bounds().Size()
	dst := src
	if maxW > 0 && size.X > maxW {
		h := size.Y * maxW / size.X
		scaled := image.NewRGBA(image.Rect(0, 0, maxW, h))
		// ApproxBiLinear 在缩略图尺度上够用，比 CatmullRom 快得多
		xdraw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), src, src.Bounds(), xdraw.Src, nil)
		dst = scaled
	}
	if quality <= 0 {
		quality = 70
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, size, err
	}
	// 返回的是**原始**分辨率，前端按它把点击坐标换算回设备坐标
	return buf.Bytes(), size, nil
}

// Tap 点击设备坐标。
func (c *Client) Tap(ctx context.Context, serial string, x, y int) error {
	_, err := c.run(ctx, serial, "shell", "input", "tap", strconv.Itoa(x), strconv.Itoa(y))
	return err
}

// Swipe 滑动。durationMS <= 0 时用 300ms。
func (c *Client) Swipe(ctx context.Context, serial string, x1, y1, x2, y2, durationMS int) error {
	if durationMS <= 0 {
		durationMS = 300
	}
	_, err := c.run(ctx, serial, "shell", "input", "swipe",
		strconv.Itoa(x1), strconv.Itoa(y1), strconv.Itoa(x2), strconv.Itoa(y2), strconv.Itoa(durationMS))
	return err
}

// keyNames 是设备墙上按钮允许的按键白名单。
// 不接受任意 keycode：设备墙是内网工具，但把「任意按键注入」开成通用通道
// 没有必要，白名单足够覆盖返回/主页/任务/电源/输入法确认。
var keyNames = map[string]string{
	"back":  "KEYCODE_BACK",
	"home":  "KEYCODE_HOME",
	"apps":  "KEYCODE_APP_SWITCH",
	"power": "KEYCODE_POWER",
	"enter": "KEYCODE_ENTER",
	"del":   "KEYCODE_DEL",
	"up":    "KEYCODE_DPAD_UP",
	"down":  "KEYCODE_DPAD_DOWN",
	"left":  "KEYCODE_DPAD_LEFT",
	"right": "KEYCODE_DPAD_RIGHT",
	"volup": "KEYCODE_VOLUME_UP",
	"voldn": "KEYCODE_VOLUME_DOWN",
}

// Key 按一个白名单里的键。
func (c *Client) Key(ctx context.Context, serial, name string) error {
	code, ok := keyNames[name]
	if !ok {
		return fmt.Errorf("不支持的按键 %q", name)
	}
	_, err := c.run(ctx, serial, "shell", "input", "keyevent", code)
	return err
}

// Text 输入文本。
// adb input text 不认空格与部分符号，空格要转成 %s；中文根本进不去
// （需要 IME），调用方应知道这个限制。
func (c *Client) Text(ctx context.Context, serial, s string) error {
	if s == "" {
		return nil
	}
	esc := strings.NewReplacer(
		" ", "%s", "'", "\\'", "\"", "\\\"", "&", "\\&",
		"<", "\\<", ">", "\\>", "(", "\\(", ")", "\\)",
		"|", "\\|", ";", "\\;", "$", "\\$", "`", "\\`",
	).Replace(s)
	_, err := c.run(ctx, serial, "shell", "input", "text", esc)
	return err
}

// Alive 报告设备是否还应答，供健康检查用。
func (c *Client) Alive(ctx context.Context, serial string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.run(ctx, serial, "shell", "getprop", "sys.boot_completed")
	return err == nil && strings.TrimSpace(string(out)) == "1"
}

// CurrentApp 返回前台 Activity，设备墙上显示「这台在跑什么」。
func (c *Client) CurrentApp(ctx context.Context, serial string) string {
	out, err := c.run(ctx, serial, "shell", "dumpsys", "window", "|", "grep", "-m1", "mCurrentFocus")
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if i := strings.Index(s, " u0 "); i >= 0 {
		s = s[i+4:]
	}
	return strings.TrimSuffix(strings.TrimSpace(s), "}")
}
