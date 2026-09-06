package api

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fancyCachier/droidpool/internal/scrcpy"
)

// fakeDevice 用两对内存管道冒充 scrcpy 会话：video 端由测试喂帧，control 端收注入消息。
// ready 在会话建好后关闭——handler 在另一个 goroutine 里赋值，测试要等它。
type fakeDevice struct {
	video, control net.Conn
	ready          chan struct{}
}

func wsTestServer(t *testing.T) (*Server, *fakeDevice, string) {
	t.Helper()
	s, h := newServer(t, 1, nil)
	s.Scrcpy.ServerJar = "fake.jar"
	fd := &fakeDevice{ready: make(chan struct{})}
	s.StartScrcpy = func(context.Context, scrcpy.Options) (*scrcpy.Session, error) {
		vc, vs := net.Pipe()
		cc, cs := net.Pipe()
		fd.video, fd.control = vs, cs
		close(fd.ready)
		t.Cleanup(func() { vs.Close(); cs.Close() })
		return scrcpy.NewSession(vc, cc, 1366, 768), nil
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return s, fd, "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/devices/dev1/ws"
}

func wsDial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func wsReadText(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("期望文本消息，得到 %v", typ)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("不是 JSON: %s", data)
	}
	return m
}

func wsSend(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readExact(t *testing.T, r net.Conn, n int) []byte {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatalf("读 %d 字节失败: %v", n, err)
	}
	return b
}

func wsHello(t *testing.T, c *websocket.Conn) {
	t.Helper()
	m := wsReadText(t, c)
	if m["t"] != "hello" || m["w"] != float64(1366) || m["h"] != float64(768) {
		t.Fatalf("hello = %v", m)
	}
}

// 主链路：握手 → 画面帧带标志位下行 → 触摸/滚轮/按键上行直达控制 socket。
func TestWSHelloFramesAndLiveInput(t *testing.T) {
	_, fd, url := wsTestServer(t)
	c := wsDial(t, url)
	wsHello(t, c)
	<-fd.ready

	// 设备侧出一帧关键帧（第 62 位是 scrcpy 的 key 标志）
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint64(hdr[0:8], uint64(1)<<62|12345)
	binary.BigEndian.PutUint32(hdr[8:12], 3)
	_ = fd.video.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := fd.video.Write(append(hdr, 'a', 'b', 'c')); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	typ, data, err := c.Read(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary || string(data) != "\x02abc" {
		t.Errorf("帧消息应为 [key 标志 0x02]+负载，得到 %v %q", typ, data)
	}

	// 触摸按下：32 字节 INJECT_TOUCH_EVENT，pointer id 与坐标原样透传
	wsSend(t, c, map[string]any{"t": "touch", "a": "down", "id": 2, "x": 100, "y": 200})
	b := readExact(t, fd.control, 32)
	if b[0] != 2 || b[1] != 0 {
		t.Errorf("应为 touch(2)/down(0)，得到 %d/%d", b[0], b[1])
	}
	if id := binary.BigEndian.Uint64(b[2:10]); id != 2 {
		t.Errorf("pointer id = %d", id)
	}
	if x, y := binary.BigEndian.Uint32(b[10:14]), binary.BigEndian.Uint32(b[14:18]); x != 100 || y != 200 {
		t.Errorf("坐标 = %d,%d", x, y)
	}
	if p := binary.BigEndian.Uint16(b[22:24]); p != 0xffff {
		t.Errorf("未带压力的 down 应按 1.0 处理，得到 %#x", p)
	}

	// 抬起时越界：钳到边缘而不是拒绝
	wsSend(t, c, map[string]any{"t": "touch", "a": "up", "id": 2, "x": -7, "y": 5000})
	b = readExact(t, fd.control, 32)
	if b[1] != 1 {
		t.Errorf("应为 up(1)，得到 %d", b[1])
	}
	if x, y := binary.BigEndian.Uint32(b[10:14]), binary.BigEndian.Uint32(b[14:18]); x != 0 || y != 767 {
		t.Errorf("越界坐标应钳到 0,767，得到 %d,%d", x, y)
	}

	// 滚轮：21 字节 INJECT_SCROLL_EVENT
	wsSend(t, c, map[string]any{"t": "scroll", "x": 10, "y": 20, "dx": 0, "dy": -1})
	b = readExact(t, fd.control, 21)
	if b[0] != 3 {
		t.Errorf("应为 scroll(3)，得到 %d", b[0])
	}
	if vs := int16(binary.BigEndian.Uint16(b[15:17])); vs != -0x8000 {
		t.Errorf("vscroll -1 应为 -0x8000，得到 %#x", vs)
	}

	// 按键：沿用 /input 的按键名映射，down + up 各 14 字节
	wsSend(t, c, map[string]any{"t": "key", "key": "back"})
	b = readExact(t, fd.control, 28)
	if b[0] != 0 || binary.BigEndian.Uint32(b[2:6]) != 4 || b[14] != 0 || b[15] != 1 {
		t.Errorf("back 应产生 keycode 4 的 down/up，得到 % x", b)
	}

	// 非法输入只回 error 消息，连接不断
	wsSend(t, c, map[string]any{"t": "touch", "a": "poke", "id": 0})
	if m := wsReadText(t, c); m["t"] != "error" {
		t.Errorf("非法动作应回 error，得到 %v", m)
	}
	wsSend(t, c, map[string]any{"t": "touch", "a": "down", "id": 0, "x": 1, "y": 1})
	if b = readExact(t, fd.control, 32); b[1] != 0 {
		t.Errorf("出错后连接应仍可用，得到 action %d", b[1])
	}
}

// 未配 scrcpy-server：升级成功后立刻用 close 帧告知原因，浏览器据此退回截图流。
func TestWSNoScrcpyClosesWithReason(t *testing.T) {
	s, _, url := wsTestServer(t)
	s.Scrcpy.ServerJar = ""
	c := wsDial(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("应收到 close 帧，得到 %v", err)
	}
	if ce.Code != websocket.StatusPolicyViolation || ce.Reason != "no_scrcpy" {
		t.Errorf("close = %d %q", ce.Code, ce.Reason)
	}
}

// scrcpy 起不来：同样走 close 帧，reason 带上原因。
func TestWSScrcpyStartFailure(t *testing.T) {
	s, _, url := wsTestServer(t)
	s.StartScrcpy = func(context.Context, scrcpy.Options) (*scrcpy.Session, error) {
		return nil, fmt.Errorf("adb: device offline")
	}
	c := wsDial(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusInternalError ||
		!strings.HasPrefix(ce.Reason, "scrcpy_failed: adb: device offline") {
		t.Errorf("应收到 scrcpy_failed 的 close 帧，得到 %v", err)
	}
	if s.h264.controller("dev1") != nil {
		t.Error("启动失败不应留下控制器")
	}
}

// reload 时新连接接管旧连接：旧的收到 taken_over（浏览器看到它就不重连），
// 新的正常工作，/input 也路由到新会话。
func TestWSTakeoverTellsOldConnection(t *testing.T) {
	s, fd, url := wsTestServer(t)
	old := wsDial(t, url)
	wsHello(t, old)
	<-fd.ready

	// 第二次启动要给一套新管道
	fd2 := &fakeDevice{ready: make(chan struct{})}
	s.StartScrcpy = func(context.Context, scrcpy.Options) (*scrcpy.Session, error) {
		vc, vs := net.Pipe()
		cc, cs := net.Pipe()
		fd2.video, fd2.control = vs, cs
		close(fd2.ready)
		t.Cleanup(func() { vs.Close(); cs.Close() })
		return scrcpy.NewSession(vc, cc, 1366, 768), nil
	}
	fresh := wsDial(t, url)
	wsHello(t, fresh)
	<-fd2.ready

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := old.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Reason != "taken_over" {
		t.Fatalf("旧连接应收到 taken_over，得到 %v", err)
	}
	if s.h264.controller("dev1") == nil {
		t.Fatal("接管后新会话的控制器应仍在")
	}
	wsSend(t, fresh, map[string]any{"t": "touch", "a": "down", "id": 0, "x": 9, "y": 9})
	if b := readExact(t, fd2.control, 32); b[1] != 0 {
		t.Errorf("新连接的输入应到达新会话，得到 action %d", b[1])
	}
}

// 浏览器断开后会话要立刻收尾：设备侧连接关闭、控制器从会话表移除，
// 否则 /input 会往死 socket 写还返回成功。
func TestWSDisconnectClosesSession(t *testing.T) {
	s, fd, url := wsTestServer(t)
	c := wsDial(t, url)
	wsHello(t, c)
	<-fd.ready
	if s.h264.controller("dev1") == nil {
		t.Fatal("会话建立后应登记控制器")
	}
	if err := c.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.h264.controller("dev1") != nil {
		if time.Now().After(deadline) {
			t.Fatal("断开后控制器应被清掉")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = fd.video.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := fd.video.Write(make([]byte, 12)); err == nil {
		t.Error("断开后设备侧视频连接应已关闭")
	}
}

// 设备侧断流（视频 socket 关闭）：告诉浏览器 video_ended 让它重连，而不是 1006。
func TestWSDeviceGoneTellsBrowserToReconnect(t *testing.T) {
	_, fd, url := wsTestServer(t)
	c := wsDial(t, url)
	wsHello(t, c)
	<-fd.ready
	fd.video.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusGoingAway || !strings.HasPrefix(ce.Reason, "video_ended") {
		t.Errorf("应收到 video_ended 的 GoingAway，得到 %v", err)
	}
}

type recordingPointer struct {
	fakeInjector
	touches []string
	scrolls []string
}

func (r *recordingPointer) Touch(a scrcpy.TouchAction, id uint64, x, y int, p float64) error {
	r.touches = append(r.touches, fmt.Sprintf("%d %d %d,%d %.2f", a, id, x, y, p))
	return nil
}
func (r *recordingPointer) Scroll(x, y int, h, v float64) error {
	r.scrolls = append(r.scrolls, fmt.Sprintf("%d,%d %.2f %.2f", x, y, h, v))
	return nil
}

func TestDispatchWSInputValidation(t *testing.T) {
	r := &recordingPointer{}
	ok := []wsInput{
		{T: "touch", A: "down", ID: 0, X: 1, Y: 2},         // 缺省压力 → 1
		{T: "touch", A: "move", ID: 9, X: 3, Y: 4, P: 0.5}, // 最大合法 id
		{T: "touch", A: "up", ID: 0, X: 5, Y: 6, P: 0.7},   // up 的压力由控制器归零，这里原样传
		{T: "scroll", X: 7, Y: 8, DX: 0.5, DY: -1},
		{T: "tap", X: 1, Y: 1},
		{T: "key", Key: "home"},
		{T: "text", Text: "hi"},
	}
	for _, in := range ok {
		if err := dispatchWSInput(r, in); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
	wantT := []string{"0 0 1,2 1.00", "2 9 3,4 0.50", "1 0 5,6 0.70"}
	if len(r.touches) != len(wantT) {
		t.Fatalf("touches = %v", r.touches)
	}
	for i := range wantT {
		if r.touches[i] != wantT[i] {
			t.Errorf("touch[%d] = %q，期望 %q", i, r.touches[i], wantT[i])
		}
	}
	if len(r.scrolls) != 1 || r.scrolls[0] != "7,8 0.50 -1.00" {
		t.Errorf("scrolls = %v", r.scrolls)
	}
	if len(r.calls) != 3 || r.calls[0] != "tap 1 1" || r.calls[1] != "key 3" || r.calls[2] != "text hi" {
		t.Errorf("退化类型应交给 injectViaController，得到 %v", r.calls)
	}

	bad := []wsInput{
		{T: "touch", A: "poke", ID: 0},
		{T: "touch", A: "down", ID: -1},
		{T: "touch", A: "down", ID: scrcpy.MaxPointers},
		{T: "shake"},
	}
	before := len(r.touches)
	for _, in := range bad {
		if err := dispatchWSInput(r, in); err == nil {
			t.Errorf("%+v 应被拒绝", in)
		}
	}
	if len(r.touches) != before {
		t.Errorf("非法输入不应触发注入: %v", r.touches[before:])
	}
	if err := dispatchWSInput(r, wsInput{T: "shake"}); !errors.Is(err, errBadInputType) {
		t.Errorf("未知类型应返回 errBadInputType，得到 %v", err)
	}
}

func TestCloseReasonFitsRFCLimit(t *testing.T) {
	long := "scrcpy_failed: " + strings.Repeat("设备", 100)
	got := closeReason(long)
	if len(got) > wsCloseReasonMax {
		t.Errorf("reason 长 %d 字节，超过 %d", len(got), wsCloseReasonMax)
	}
	if !strings.HasPrefix(got, "scrcpy_failed: ") || !strings.HasSuffix(got, "…") {
		t.Errorf("截断后应保留前缀并以省略号结尾，得到 %q", got)
	}
	if short := "no_control"; closeReason(short) != short {
		t.Errorf("短 reason 不应被改动")
	}
}
