package scrcpy

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// ctrlSession 造一个带控制 socket 的会话，返回服务端读端。
func ctrlSession(t *testing.T, w, h int) (*Controller, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	s := &Session{control: client, Width: w, Height: h}
	c, err := s.Control()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return c, server
}

func readN(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatalf("读 %d 字节失败: %v", n, err)
	}
	return b
}

func TestFloatToU16FP(t *testing.T) {
	cases := map[float64]uint16{
		0:    0,
		0.5:  0x8000,
		1:    0xffff, // f=1.0 会溢出到 0x10000，必须钳到 0xffff
		1.5:  0xffff,
		-0.1: 0,
	}
	for in, want := range cases {
		if got := floatToU16FP(in); got != want {
			t.Errorf("floatToU16FP(%v) = %#x，期望 %#x", in, got, want)
		}
	}
}

// 触摸消息是固定 32 字节，字段布局必须与 scrcpy 4.1 的 control_msg.c 逐字节对齐，
// 错一个偏移服务端就会解出荒谬坐标或直接断开。
func TestTouchMsgLayout(t *testing.T) {
	c, _ := ctrlSession(t, 1366, 768)
	b := c.touchMsg(motionActionDown, 100, 200, 1.0, buttonPrimary)
	if len(b) != 32 {
		t.Fatalf("触摸消息应为 32 字节，得到 %d", len(b))
	}
	if b[0] != msgInjectTouchEvent {
		t.Errorf("类型字节 = %d，期望 %d", b[0], msgInjectTouchEvent)
	}
	if b[1] != motionActionDown {
		t.Errorf("action = %d", b[1])
	}
	if got := binary.BigEndian.Uint64(b[2:10]); got != pointerIDFinger {
		t.Errorf("pointer_id = %#x，期望通用手指 %#x", got, pointerIDFinger)
	}
	if x := binary.BigEndian.Uint32(b[10:14]); x != 100 {
		t.Errorf("x = %d", x)
	}
	if y := binary.BigEndian.Uint32(b[14:18]); y != 200 {
		t.Errorf("y = %d", y)
	}
	// 屏幕尺寸是 16 位，紧跟坐标之后；服务端用它校验坐标范围
	if w := binary.BigEndian.Uint16(b[18:20]); w != 1366 {
		t.Errorf("screen w = %d", w)
	}
	if h := binary.BigEndian.Uint16(b[20:22]); h != 768 {
		t.Errorf("screen h = %d", h)
	}
	if p := binary.BigEndian.Uint16(b[22:24]); p != 0xffff {
		t.Errorf("pressure 1.0 应编码为 0xffff，得到 %#x", p)
	}
	if ab := binary.BigEndian.Uint32(b[24:28]); ab != buttonPrimary {
		t.Errorf("action_button = %d", ab)
	}
	if bt := binary.BigEndian.Uint32(b[28:32]); bt != buttonPrimary {
		t.Errorf("buttons = %d", bt)
	}
}

func TestTapSendsDownThenUp(t *testing.T) {
	c, srv := ctrlSession(t, 1366, 768)
	done := make(chan error, 1)
	go func() { done <- c.Tap(683, 384) }()

	down := readN(t, srv, 32)
	up := readN(t, srv, 32)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if down[1] != motionActionDown || up[1] != motionActionUp {
		t.Errorf("应先 DOWN(%d) 后 UP(%d)，得到 %d, %d", motionActionDown, motionActionUp, down[1], up[1])
	}
	// 抬起时压力为 0、按钮为 0，否则某些 View 会当成仍按着
	if p := binary.BigEndian.Uint16(up[22:24]); p != 0 {
		t.Errorf("UP 的 pressure 应为 0，得到 %#x", p)
	}
	if bt := binary.BigEndian.Uint32(up[28:32]); bt != 0 {
		t.Errorf("UP 的 buttons 应为 0，得到 %d", bt)
	}
}

func TestTapRejectsOutOfBounds(t *testing.T) {
	c, _ := ctrlSession(t, 1366, 768)
	for _, xy := range [][2]int{{-1, 0}, {0, -1}, {1366, 0}, {0, 768}, {5000, 5000}} {
		if err := c.Tap(xy[0], xy[1]); err == nil {
			t.Errorf("坐标 %v 超出屏幕应被拒绝", xy)
		}
	}
}

// 滑动必须有中间 MOVE 事件：一步到位的 down→up 会被 Flutter 当成点击，
// 列表根本不会滚。
func TestSwipeEmitsMoves(t *testing.T) {
	c, srv := ctrlSession(t, 1366, 768)
	done := make(chan error, 1)
	go func() { done <- c.Swipe(683, 600, 683, 200, 160*time.Millisecond) }()

	var actions []byte
	deadline := time.After(3 * time.Second)
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			goto check
		case <-deadline:
			t.Fatal("Swipe 没在 3s 内完成")
		default:
		}
		srv.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		b := make([]byte, 32)
		if _, err := io.ReadFull(srv, b); err == nil {
			actions = append(actions, b[1])
		}
	}
check:
	srv.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	for {
		b := make([]byte, 32)
		if _, err := io.ReadFull(srv, b); err != nil {
			break
		}
		actions = append(actions, b[1])
	}
	if len(actions) < 4 {
		t.Fatalf("滑动至少应有 DOWN + ≥2 MOVE + UP，只收到 %d 条: %v", len(actions), actions)
	}
	if actions[0] != motionActionDown {
		t.Errorf("首条应为 DOWN，得到 %d", actions[0])
	}
	if actions[len(actions)-1] != motionActionUp {
		t.Errorf("末条应为 UP，得到 %d", actions[len(actions)-1])
	}
	moves := 0
	for _, a := range actions[1 : len(actions)-1] {
		if a == motionActionMove {
			moves++
		}
	}
	if moves < 2 {
		t.Errorf("中间应有多条 MOVE，只有 %d 条", moves)
	}
}

func TestKeyMsgLayoutAndDownUp(t *testing.T) {
	c, srv := ctrlSession(t, 1366, 768)
	done := make(chan error, 1)
	go func() { done <- c.Key(KeycodeBack) }()
	down := readN(t, srv, 14)
	up := readN(t, srv, 14)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if down[0] != msgInjectKeycode || down[1] != keyActionDown {
		t.Errorf("DOWN 头 = %v", down[:2])
	}
	if up[1] != keyActionUp {
		t.Errorf("第二条应为 UP，得到 action=%d", up[1])
	}
	if kc := binary.BigEndian.Uint32(down[2:6]); kc != KeycodeBack {
		t.Errorf("keycode = %d，期望 %d", kc, KeycodeBack)
	}
	// repeat 与 metastate 都应为 0，否则按键会被当成长按或带修饰键
	if r := binary.BigEndian.Uint32(down[6:10]); r != 0 {
		t.Errorf("repeat 应为 0，得到 %d", r)
	}
	if m := binary.BigEndian.Uint32(down[10:14]); m != 0 {
		t.Errorf("metastate 应为 0，得到 %d", m)
	}
}

func TestTextMsgLayout(t *testing.T) {
	c, srv := ctrlSession(t, 1366, 768)
	done := make(chan error, 1)
	go func() { done <- c.Text("hi 中") }()
	want := "hi 中"
	b := readN(t, srv, 1+4+len(want))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b[0] != msgInjectText {
		t.Errorf("类型 = %d", b[0])
	}
	// 长度前缀是字节数不是字符数：UTF-8 的「中」占 3 字节
	if n := binary.BigEndian.Uint32(b[1:5]); int(n) != len(want) {
		t.Errorf("长度前缀 = %d，期望字节数 %d", n, len(want))
	}
	if got := string(b[5:]); got != want {
		t.Errorf("文本 = %q", got)
	}
}

func TestTextRejectsOverLimit(t *testing.T) {
	c, _ := ctrlSession(t, 1366, 768)
	long := make([]byte, injectTextMax+1)
	for i := range long {
		long[i] = 'a'
	}
	// 超限必须报错而不是静默截断——截断会让「少了几个字」很难被发现
	if err := c.Text(string(long)); err == nil {
		t.Error("超过 300 字节应报错")
	}
	// 空串不发消息也不报错
	if err := c.Text(""); err != nil {
		t.Errorf("空串不应报错: %v", err)
	}
}

func TestControlRequiresSocket(t *testing.T) {
	s := &Session{}
	if _, err := s.Control(); err == nil {
		t.Error("未连接控制 socket 时应报错")
	}
}

func TestControllerRefusesAfterClose(t *testing.T) {
	c, _ := ctrlSession(t, 1366, 768)
	c.s.closed.Store(true)
	if err := c.Key(KeycodeBack); err == nil {
		t.Error("会话关闭后写入应报错，而不是写到已关的 socket 上 panic")
	}
}
