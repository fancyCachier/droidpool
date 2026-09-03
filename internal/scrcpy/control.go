package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// 控制消息类型，取 scrcpy 4.1 `enum sc_control_msg_type` 的声明顺序（从 0 起）。
const (
	msgInjectKeycode    = 0
	msgInjectText       = 1
	msgInjectTouchEvent = 2
	msgInjectScroll     = 3
	msgBackOrScreenOn   = 4
)

// Android 输入常量（android/input.h）。
const (
	keyActionDown = 0
	keyActionUp   = 1

	motionActionDown = 0
	motionActionUp   = 1
	motionActionMove = 2

	buttonPrimary = 1
)

// pointerIDFinger 是 scrcpy 定义的「通用手指」指针 id（UINT64_C(-2)）。
// 用它而不是鼠标 id，注入出来的就是触摸事件，Flutter 的手势识别按触屏走。
const pointerIDFinger = ^uint64(1)

// injectTextMax 是 scrcpy 服务端接受的单条文本上限。
const injectTextMax = 300

// Controller 通过 scrcpy 的控制 socket 注入输入。
//
// 相比 `adb shell input tap`，这条路省掉了每次 fork adb 进程、建 shell、
// 起 Java 进程的固定成本（实测 70~120 ms），直接写一个 32 字节的消息到
// 已建立的 socket，服务端用 InputManager.injectInputEvent 注入。
type Controller struct {
	mu   sync.Mutex
	s    *Session
	w, h int // 注入时要带上屏幕尺寸，服务端据此校验坐标
}

// Control 返回该会话的控制器。会话未连接控制 socket 时返回错误。
func (s *Session) Control() (*Controller, error) {
	if s.control == nil {
		return nil, errors.New("控制 socket 未连接")
	}
	return &Controller{s: s, w: s.Width, h: s.Height}, nil
}

func (c *Controller) write(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.s.closed.Load() {
		return errors.New("会话已关闭")
	}
	_ = c.s.control.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.s.control.Write(b); err != nil {
		// 写失败基本意味着服务端已死。主动关掉会话，让流循环与会话表一起收尾，
		// 否则下一次 /input 还会被路由到这个死通道上。
		c.s.Close()
		return fmt.Errorf("控制通道写入失败（会话已关闭）: %w", err)
	}
	return nil
}

// floatToU16FP 把 [0,1] 的浮点压成 16 位定点，与 scrcpy 的 sc_float_to_u16fp 一致：
// 乘 2^16 后钳到 0xffff（f=1.0 时恰好溢出到 0x10000）。
func floatToU16FP(f float64) uint16 {
	if f <= 0 {
		return 0
	}
	if f >= 1 {
		return 0xffff
	}
	return uint16(math.Floor(f * 65536))
}

// touchMsg 组一条 INJECT_TOUCH_EVENT（固定 32 字节）。
func (c *Controller) touchMsg(action byte, x, y int, pressure float64, buttons uint32) []byte {
	b := make([]byte, 32)
	b[0] = msgInjectTouchEvent
	b[1] = action
	binary.BigEndian.PutUint64(b[2:10], pointerIDFinger)
	binary.BigEndian.PutUint32(b[10:14], uint32(int32(x)))
	binary.BigEndian.PutUint32(b[14:18], uint32(int32(y)))
	binary.BigEndian.PutUint16(b[18:20], uint16(c.w))
	binary.BigEndian.PutUint16(b[20:22], uint16(c.h))
	binary.BigEndian.PutUint16(b[22:24], floatToU16FP(pressure))
	binary.BigEndian.PutUint32(b[24:28], buttons) // action_button
	binary.BigEndian.PutUint32(b[28:32], buttons) // buttons
	return b
}

func (c *Controller) checkXY(x, y int) error {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return fmt.Errorf("坐标 (%d,%d) 超出屏幕 %dx%d", x, y, c.w, c.h)
	}
	return nil
}

// Tap 点按。按下与抬起两条消息，中间留一个短暂间隔让系统识别为 tap 而不是 move。
func (c *Controller) Tap(x, y int) error {
	if err := c.checkXY(x, y); err != nil {
		return err
	}
	if err := c.write(c.touchMsg(motionActionDown, x, y, 1, buttonPrimary)); err != nil {
		return err
	}
	time.Sleep(40 * time.Millisecond)
	return c.write(c.touchMsg(motionActionUp, x, y, 0, 0))
}

// Swipe 滑动。用 MOVE 事件插值，让 Flutter 的滚动物理能识别出速度；
// 一步到位的 down→up 会被当成点击而不是拖动。
func (c *Controller) Swipe(x1, y1, x2, y2 int, dur time.Duration) error {
	if err := c.checkXY(x1, y1); err != nil {
		return err
	}
	if err := c.checkXY(x2, y2); err != nil {
		return err
	}
	if dur <= 0 {
		dur = 300 * time.Millisecond
	}
	steps := int(dur / (16 * time.Millisecond)) // 约 60 Hz
	if steps < 2 {
		steps = 2
	}
	if steps > 60 {
		steps = 60
	}
	if err := c.write(c.touchMsg(motionActionDown, x1, y1, 1, buttonPrimary)); err != nil {
		return err
	}
	tick := dur / time.Duration(steps)
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := x1 + int(float64(x2-x1)*t)
		y := y1 + int(float64(y2-y1)*t)
		if err := c.write(c.touchMsg(motionActionMove, x, y, 1, buttonPrimary)); err != nil {
			return err
		}
		time.Sleep(tick)
	}
	return c.write(c.touchMsg(motionActionUp, x2, y2, 0, 0))
}

// keyMsg 组一条 INJECT_KEYCODE（固定 14 字节）。
func keyMsg(action byte, keycode uint32) []byte {
	b := make([]byte, 14)
	b[0] = msgInjectKeycode
	b[1] = action
	binary.BigEndian.PutUint32(b[2:6], keycode)
	// repeat=0, metastate=0
	return b
}

// Key 按一个 Android keycode（KEYCODE_BACK=4 等）。
func (c *Controller) Key(keycode uint32) error {
	if err := c.write(keyMsg(keyActionDown, keycode)); err != nil {
		return err
	}
	return c.write(keyMsg(keyActionUp, keycode))
}

// Text 注入文本。服务端限 300 字节，超出直接拒绝而不是静默截断——
// 截断会让「输入了但少了几个字」这种问题很难被发现。
func (c *Controller) Text(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > injectTextMax {
		return fmt.Errorf("文本 %d 字节超过上限 %d", len(s), injectTextMax)
	}
	b := make([]byte, 1+4+len(s))
	b[0] = msgInjectText
	binary.BigEndian.PutUint32(b[1:5], uint32(len(s)))
	copy(b[5:], s)
	return c.write(b)
}

// Android keycode 表，只列设备墙用到的。
const (
	KeycodeBack       = 4
	KeycodeHome       = 3
	KeycodeAppSwitch  = 187
	KeycodePower      = 26
	KeycodeEnter      = 66
	KeycodeDel        = 67
	KeycodeDpadUp     = 19
	KeycodeDpadDown   = 20
	KeycodeDpadLeft   = 21
	KeycodeDpadRight  = 22
	KeycodeVolumeUp   = 24
	KeycodeVolumeDown = 25
)
