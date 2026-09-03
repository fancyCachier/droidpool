package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/http"
	"testing"
	"time"
)

func TestChecksumDistinguishesFrames(t *testing.T) {
	a := make([]byte, 4096)
	b := make([]byte, 4096)
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(i)
	}
	if checksum(a) != checksum(b) {
		t.Error("相同内容应得到相同指纹，否则去重完全失效")
	}
	// 改一个抽样点必须被发现
	b[0] = 0xFF
	if checksum(a) == checksum(b) {
		t.Error("首字节变化未被发现")
	}
	// 长度变化必须被发现（JPEG 内容一变长度通常就变）
	if checksum(a) == checksum(a[:4000]) {
		t.Error("长度变化未被发现")
	}
	// 空输入不该 panic
	checksum(nil)
	checksum([]byte{})
}

// 任意单字节变化都必须被发现：漏判会把「画面变了」当成「没变」而跳帧，
// 界面直接冻住。抽样式指纹过不了这一条，所以必须全量哈希。
func TestChecksumCatchesAnySingleByteChange(t *testing.T) {
	base := make([]byte, 100000)
	for i := range base {
		base[i] = byte(i * 7)
	}
	h := checksum(base)
	// 覆盖头、中、尾以及若干随机位置
	for _, pos := range []int{0, 1, 999, 50000, 73331, len(base) - 2, len(base) - 1} {
		mod := make([]byte, len(base))
		copy(mod, base)
		mod[pos] ^= 0xFF
		if checksum(mod) == h {
			t.Errorf("第 %d 字节的变化未被发现，该帧会被误判为静止而跳过", pos)
		}
	}
}

// fakeInjector 记录走控制通道的调用。
type fakeInjector struct{ calls []string }

func (f *fakeInjector) Tap(x, y int) error {
	f.calls = append(f.calls, fmt.Sprintf("tap %d %d", x, y))
	return nil
}
func (f *fakeInjector) Swipe(x1, y1, x2, y2 int, d time.Duration) error {
	f.calls = append(f.calls, fmt.Sprintf("swipe %d %d %d %d %v", x1, y1, x2, y2, d))
	return nil
}
func (f *fakeInjector) Key(k uint32) error {
	f.calls = append(f.calls, fmt.Sprintf("key %d", k))
	return nil
}
func (f *fakeInjector) Text(s string) error { f.calls = append(f.calls, "text "+s); return nil }

func TestInjectViaControllerMapsAllTypes(t *testing.T) {
	f := &fakeInjector{}
	for _, req := range []inputReq{
		{Type: "tap", X: 10, Y: 20},
		{Type: "swipe", X: 1, Y: 2, X2: 3, Y2: 4, MS: 250},
		{Type: "key", Key: "back"},
		{Type: "text", Text: "hi"},
	} {
		if err := injectViaController(f, req); err != nil {
			t.Errorf("%s: %v", req.Type, err)
		}
	}
	want := []string{"tap 10 20", "swipe 1 2 3 4 250ms", "key 4", "text hi"}
	if len(f.calls) != len(want) {
		t.Fatalf("调用 = %v", f.calls)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Errorf("第 %d 条 = %q，期望 %q", i, f.calls[i], want[i])
		}
	}
}

func TestInjectViaControllerRejectsUnknown(t *testing.T) {
	f := &fakeInjector{}
	if err := injectViaController(f, inputReq{Type: "shake"}); !errors.Is(err, errBadInputType) {
		t.Errorf("未知类型应返回 errBadInputType，得到 %v", err)
	}
	if err := injectViaController(f, inputReq{Type: "key", Key: "不存在"}); err == nil {
		t.Error("未知按键名应报错")
	}
	if len(f.calls) != 0 {
		t.Errorf("非法请求不应触发任何注入: %v", f.calls)
	}
}

// 所有设备墙上的按键名都必须能映射到 keycode，否则 H.264 模式下那个键会失灵，
// 而 adb 模式下却是好的——这种「只在一条路径上坏」的问题最难被发现。
func TestKeyToCodeCoversWallButtons(t *testing.T) {
	for _, k := range []string{"back", "home", "apps", "power", "enter", "del",
		"up", "down", "left", "right", "volup", "voldn"} {
		if _, ok := keyToCode[k]; !ok {
			t.Errorf("按键 %q 没有 keycode 映射", k)
		}
	}
}

// 有活跃 scrcpy 会话时 /input 必须走控制通道；没有时退回 adb。
func TestInputPrefersControllerWhenPresent(t *testing.T) {
	s, h := newServer(t, 1, nil)
	scr := &recordingScreen{}
	s.Screen = scr
	dev := "dev1"

	// 无会话：走 adb
	rec := do(t, h, "POST", "/api/devices/"+dev+"/input", map[string]any{"type": "tap", "x": 5, "y": 6}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("adb 路径应 200，得到 %d: %s", rec.Code, rec.Body)
	}
	var r1 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &r1)
	if r1["via"] != "adb" || len(scr.taps) != 1 {
		t.Errorf("无会话时应走 adb，via=%v taps=%v", r1["via"], scr.taps)
	}

	// 登记会话控制器后：走 scrcpy，adb 不再被调用
	inj := &fakeInjector{}
	gen, _ := s.h264.acquire(dev, 27200, func() {})
	s.h264.setController(dev, gen, inj)
	rec = do(t, h, "POST", "/api/devices/"+dev+"/input", map[string]any{"type": "tap", "x": 7, "y": 8}, false)
	var r2 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &r2)
	if r2["via"] != "scrcpy" {
		t.Errorf("有会话时应走 scrcpy，via=%v", r2["via"])
	}
	if len(inj.calls) != 1 || inj.calls[0] != "tap 7 8" {
		t.Errorf("控制通道应收到 tap 7 8，得到 %v", inj.calls)
	}
	if len(scr.taps) != 1 {
		t.Errorf("有会话时不应再走 adb，adb taps=%v", scr.taps)
	}

	// 会话释放后回到 adb
	s.h264.release(dev, gen)
	do(t, h, "POST", "/api/devices/"+dev+"/input", map[string]any{"type": "tap", "x": 9, "y": 9}, false)
	if len(scr.taps) != 2 {
		t.Errorf("会话释放后应回到 adb，adb taps=%v", scr.taps)
	}
}

// recordingScreen 是记录调用的 Screener 假实现。
type recordingScreen struct{ taps []string }

func (r *recordingScreen) ScreenshotJPEG(context.Context, string, int, int) ([]byte, image.Point, error) {
	return nil, image.Point{}, errors.New("未实现")
}
func (r *recordingScreen) ScreenshotPassthrough(context.Context, string) ([]byte, image.Point, error) {
	return nil, image.Point{}, errors.New("未实现")
}
func (r *recordingScreen) Tap(_ context.Context, _ string, x, y int) error {
	r.taps = append(r.taps, fmt.Sprintf("%d,%d", x, y))
	return nil
}
func (r *recordingScreen) Swipe(context.Context, string, int, int, int, int, int) error { return nil }
func (r *recordingScreen) Key(context.Context, string, string) error                    { return nil }
func (r *recordingScreen) Text(context.Context, string, string) error                   { return nil }
func (r *recordingScreen) Connect(context.Context, string) error                        { return nil }

// reload 时旧页面的流还没断、新页面就来了。必须让新连接接管旧会话，
// 而不是拿 409 把操作人员挡在外面——reload 是最常见的动作。
func TestH264SessionTakeoverOnReload(t *testing.T) {
	var h h264Sessions
	oldCancelled := false
	gen1, port1 := h.acquire("dev1", 27200, func() { oldCancelled = true })
	h.setController("dev1", gen1, &fakeInjector{})

	// 新会话到来：旧的应被取消，新的成为当前
	newInj := &fakeInjector{}
	gen2, port2 := h.acquire("dev1", 27200, func() {})
	if !oldCancelled {
		t.Fatal("新会话到来时应取消旧会话")
	}
	if gen2 == gen1 {
		t.Fatal("代数应递增")
	}
	if port1 == port2 {
		t.Error("新旧会话不应共用端口，旧的 forward 可能还没放掉")
	}
	h.setController("dev1", gen2, newInj)
	if h.controller("dev1") != newInj {
		t.Error("当前控制器应是新会话的")
	}

	// 旧会话的收尾不能把新会话的条目误删
	h.release("dev1", gen1)
	if h.controller("dev1") != newInj {
		t.Error("旧会话 release 后新会话的控制器应仍在")
	}
	// 新会话自己 release 才清掉
	h.release("dev1", gen2)
	if h.controller("dev1") != nil {
		t.Error("新会话 release 后应无控制器")
	}
}

// 旧会话迟到的 setController 不能覆盖新会话的控制器。
func TestH264StaleSetControllerIgnored(t *testing.T) {
	var h h264Sessions
	gen1, _ := h.acquire("dev1", 27200, func() {})
	gen2, _ := h.acquire("dev1", 27200, func() {})
	newInj := &fakeInjector{}
	h.setController("dev1", gen2, newInj)
	h.setController("dev1", gen1, &fakeInjector{}) // 迟到的旧会话
	if h.controller("dev1") != newInj {
		t.Error("旧代数的 setController 应被忽略")
	}
}

func TestH264SessionsIsolatedPerDevice(t *testing.T) {
	var h h264Sessions
	c1 := false
	h.acquire("dev1", 27200, func() { c1 = true })
	h.acquire("dev2", 27200, func() {})
	if c1 {
		t.Error("另一台设备的会话不应影响本设备")
	}
}
