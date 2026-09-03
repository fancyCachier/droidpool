package adb

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	out   map[string][]byte
	err   map[string]error
}

func (f *fakeRunner) Output(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for k, e := range f.err {
		if strings.Contains(joined, k) {
			return nil, e
		}
	}
	for k, v := range f.out {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return nil, nil
}

func (f *fakeRunner) last(sub string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if strings.Contains(strings.Join(f.calls[i], " "), sub) {
			return f.calls[i]
		}
	}
	return nil
}

// pngOf 造一张纯色 PNG，模拟 screencap 输出。
func pngOf(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func newClient(f *fakeRunner) *Client { return &Client{Runner: f} }

func TestTapUsesDeviceCoordinates(t *testing.T) {
	f := &fakeRunner{}
	c := newClient(f)
	if err := c.Tap(context.Background(), "1.2.3.4:5561", 683, 632); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.last("input tap"), " ")
	if !strings.Contains(got, "-s 1.2.3.4:5561") {
		t.Errorf("应指定设备序列号，得到 %s", got)
	}
	if !strings.Contains(got, "input tap 683 632") {
		t.Errorf("坐标应原样传给设备，得到 %s", got)
	}
}

func TestSwipeDefaultDuration(t *testing.T) {
	f := &fakeRunner{}
	c := newClient(f)
	c.Swipe(context.Background(), "d", 1, 2, 3, 4, 0)
	if got := strings.Join(f.last("input swipe"), " "); !strings.HasSuffix(got, "1 2 3 4 300") {
		t.Errorf("时长为 0 时应用默认 300ms，得到 %s", got)
	}
	c.Swipe(context.Background(), "d", 1, 2, 3, 4, 800)
	if got := strings.Join(f.last("input swipe"), " "); !strings.HasSuffix(got, "800") {
		t.Errorf("显式时长应生效，得到 %s", got)
	}
}

func TestKeyWhitelist(t *testing.T) {
	f := &fakeRunner{}
	c := newClient(f)
	if err := c.Key(context.Background(), "d", "back"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.last("keyevent"), " "); !strings.Contains(got, "KEYCODE_BACK") {
		t.Errorf("back 应映射为 KEYCODE_BACK，得到 %s", got)
	}
	// 白名单之外的一律拒绝，不允许注入任意 keycode
	for _, bad := range []string{"KEYCODE_BACK", "26", "任意", ""} {
		if err := c.Key(context.Background(), "d", bad); err == nil {
			t.Errorf("按键 %q 不在白名单，应报错", bad)
		}
	}
}

func TestTextEscaping(t *testing.T) {
	f := &fakeRunner{}
	c := newClient(f)
	// adb input text 不认裸空格，必须转成 %s，否则只输入到第一个空格
	if err := c.Text(context.Background(), "d", "hello world"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.last("input text"), " "); !strings.Contains(got, "hello%sworld") {
		t.Errorf("空格应转义为 %%s，得到 %s", got)
	}
	// shell 元字符要转义，避免被 adb shell 解释
	c.Text(context.Background(), "d", "a$b`c")
	got := strings.Join(f.last("input text"), " ")
	if !strings.Contains(got, `\$`) || !strings.Contains(got, "\\`") {
		t.Errorf("shell 元字符应被转义，得到 %s", got)
	}
	// 空串不该发命令
	before := len(f.calls)
	c.Text(context.Background(), "d", "")
	if len(f.calls) != before {
		t.Error("空文本不应发出 adb 命令")
	}
}

func TestScreenshotScalesAndReportsOriginalSize(t *testing.T) {
	f := &fakeRunner{out: map[string][]byte{"screencap": pngOf(1366, 768)}}
	c := newClient(f)
	jpg, size, err := c.ScreenshotJPEG(context.Background(), "d", 480, 70)
	if err != nil {
		t.Fatal(err)
	}
	// 返回的必须是原始尺寸：前端靠它把点击坐标从缩略图换算回设备坐标，
	// 报成缩略图尺寸的话点击会全部偏移。
	if size.X != 1366 || size.Y != 768 {
		t.Errorf("应返回原始分辨率 1366x768，得到 %v", size)
	}
	img, _, err := image.Decode(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("输出应是可解码的图片: %v", err)
	}
	if img.Bounds().Dx() != 480 {
		t.Errorf("应缩放到 480 宽，得到 %d", img.Bounds().Dx())
	}
	// 等比缩放，不能变形
	if h := img.Bounds().Dy(); h != 768*480/1366 {
		t.Errorf("高度应等比缩放，得到 %d", h)
	}
	// 缩略图要显著小于原图，否则设备墙带宽白费
	if len(jpg) >= len(pngOf(1366, 768)) {
		t.Errorf("缩略图 %d 字节没有比原图更小", len(jpg))
	}
}

func TestScreenshotNoUpscale(t *testing.T) {
	f := &fakeRunner{out: map[string][]byte{"screencap": pngOf(320, 240)}}
	c := newClient(f)
	jpg, _, err := c.ScreenshotJPEG(context.Background(), "d", 480, 70)
	if err != nil {
		t.Fatal(err)
	}
	img, _, _ := image.Decode(bytes.NewReader(jpg))
	if img.Bounds().Dx() != 320 {
		t.Errorf("小于目标宽度时不应放大，得到 %d", img.Bounds().Dx())
	}
}

func TestScreenshotRejectsGarbage(t *testing.T) {
	// 设备刚重启时 screencap 可能返回空或半截数据
	f := &fakeRunner{out: map[string][]byte{"screencap": []byte("not a png")}}
	c := newClient(f)
	if _, _, err := c.ScreenshotJPEG(context.Background(), "d", 480, 70); err == nil {
		t.Error("非图片数据应报错而不是返回空图")
	}
}

func TestConnectDetectsFailureInOutput(t *testing.T) {
	// adb connect 失败时退出码仍是 0，只能看输出
	f := &fakeRunner{out: map[string][]byte{"connect": []byte("failed to connect to 1.2.3.4:5561")}}
	c := newClient(f)
	if err := c.Connect(context.Background(), "1.2.3.4:5561"); err == nil {
		t.Error("输出含 failed 时应报错，不能因为退出码为 0 就当成功")
	}
	f2 := &fakeRunner{out: map[string][]byte{"connect": []byte("connected to 1.2.3.4:5561")}}
	if err := newClient(f2).Connect(context.Background(), "1.2.3.4:5561"); err != nil {
		t.Errorf("连接成功不应报错: %v", err)
	}
}

func TestAlive(t *testing.T) {
	f := &fakeRunner{out: map[string][]byte{"sys.boot_completed": []byte("1\n")}}
	if !newClient(f).Alive(context.Background(), "d") {
		t.Error("boot_completed=1 应判为存活")
	}
	f2 := &fakeRunner{out: map[string][]byte{"sys.boot_completed": []byte("0\n")}}
	if newClient(f2).Alive(context.Background(), "d") {
		t.Error("boot_completed=0 不应判为存活")
	}
	f3 := &fakeRunner{err: map[string]error{"getprop": errors.New("device offline")}}
	if newClient(f3).Alive(context.Background(), "d") {
		t.Error("adb 报错时不应判为存活")
	}
}

// 并发截图必须串行化：多路并发打同一个 adb server 会拿到半截数据，
// 设备墙上表现为花屏。
func TestConcurrentCallsAreSerialized(t *testing.T) {
	var (
		mu                sync.Mutex
		inFlight, maxSeen int
	)
	f := &fakeRunner{}
	c := &Client{Runner: runnerFunc(func(_ context.Context, _ ...string) ([]byte, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxSeen {
			maxSeen = inFlight
		}
		mu.Unlock()
		defer func() { mu.Lock(); inFlight--; mu.Unlock() }()
		return pngOf(64, 48), nil
	})}
	_ = f

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.ScreencapPNG(context.Background(), "d") }()
	}
	wg.Wait()
	if maxSeen > 1 {
		t.Errorf("同时有 %d 个 adb 调用在跑，应被串行化", maxSeen)
	}
}

type runnerFunc func(context.Context, ...string) ([]byte, error)

func (f runnerFunc) Output(ctx context.Context, args ...string) ([]byte, error) {
	return f(ctx, args...)
}
