package api

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/fancyCachier/droidpool/internal/scrcpy"
)

// 设备操作的双向通道：一条 WebSocket 同时下发 H.264 画面、上收实时输入。
//
// 之前画面走 multipart HTTP、输入走每手势一次 POST：浏览器要等 pointerup 才能
// 决定是点还是滑，再由服务端插值回放——拖动是松手后设备才开始动，长按、拖拽、
// 滑块、双指缩放都做不了。改成指针事件按发生顺序实时转发之后，浏览器的
// pointerdown/move/up 直接对应 scrcpy 的 DOWN/MOVE/UP，设备侧看到的就是一根
// 真实的手指。顺带绕开 HTTP/1.1 每域名 6 条连接的上限：墙面 N 路 MJPEG + SSE
// 再开放大页很容易撞上。
//
// 线协议（都很小，方便在浏览器控制台里排查）：
//
//	服务端 → 浏览器  文本 JSON  {"t":"hello","device":"…","w":1366,"h":768}   握手完成，可以开始解码
//	                            {"t":"error","msg":"…"}                        某条输入被拒，连接不断
//	                 二进制     [flags 1 字节][H.264 访问单元]   flags: bit0 = 参数集(SPS/PPS)，bit1 = 关键帧
//	浏览器 → 服务端  文本 JSON  {"t":"touch","a":"down|move|up","id":0,"x":..,"y":..,"p":1}
//	                            {"t":"scroll","x":..,"y":..,"dx":0,"dy":-1}   dx/dy ∈ [-1,1]，与 Android AXIS_*SCROLL 同向
//	                            {"t":"key","key":"back"}  {"t":"text","text":"…"}
//	                            {"t":"tap",…} / {"t":"swipe",…}   与 /input 同义，给退化路径用
//
// 前置失败（未配 scrcpy-server、设备侧起不来）用 close 帧的 reason 告诉浏览器，
// 浏览器据此退回截图流——升级前的 HTTP 状态码在浏览器的 WebSocket API 里拿不到。
// 被别的页面接管时 reason 是 taken_over，浏览器看到它就不再重连，否则两个页面会
// 互相踢下线没完没了。
//
// 一条踩过的坑：coder/websocket 对传给 Read / Write / Ping 的 ctx 一旦取消，
// 会**直接断 TCP 而不发 close 帧**，浏览器只能看到 1006。所以这里所有 WebSocket
// 调用都不用会话 ctx，只用 Background 加超时；会话要结束时显式 Close 发 close 帧，
// 靠对端回的 close 帧让阻塞中的 Read 返回。

const (
	wsFlagConfig = 1 << 0
	wsFlagKey    = 1 << 1
	// wsWriteTimeout 单帧写超时。浏览器长时间收不动（标签页被冻结、网络断了）
	// 就断开让它重连，否则帧在这边堆着，scrcpy 那头的编码器也会被背压卡住。
	wsWriteTimeout = 5 * time.Second
	wsPingInterval = 15 * time.Second
	// wsReadLimit 输入消息都是几十字节的 JSON，文本注入最长 300 字节；再大就是异常。
	wsReadLimit = 64 << 10
	// wsCloseReasonMax 是 RFC 6455 对 close 帧 reason 的长度上限。
	wsCloseReasonMax = 123
)

var (
	errTakenOver  = errors.New("被同设备的新连接接管")
	errDeviceGone = errors.New("设备侧 scrcpy 进程已退出")
	errClientGone = errors.New("浏览器已断开")
)

// scrcpyStarter 是启动 scrcpy 会话的入口，签名与 scrcpy.Start 一致。
type ScrcpyStarter func(ctx context.Context, opt scrcpy.Options) (*scrcpy.Session, error)

func (s *Server) scrcpyStart(ctx context.Context, opt scrcpy.Options) (*scrcpy.Session, error) {
	if s.StartScrcpy != nil {
		return s.StartScrcpy(ctx, opt)
	}
	return scrcpy.Start(ctx, opt)
}

// wsInput 是浏览器上行的一条输入。字段按类型取用，其余为零值。
type wsInput struct {
	T    string  `json:"t"`
	A    string  `json:"a"`
	ID   int     `json:"id"`
	X    int     `json:"x"`
	Y    int     `json:"y"`
	P    float64 `json:"p"`
	DX   float64 `json:"dx"`
	DY   float64 `json:"dy"`
	X2   int     `json:"x2"`
	Y2   int     `json:"y2"`
	MS   int     `json:"ms"`
	Key  string  `json:"key"`
	Text string  `json:"text"`
}

// pointerInjector 是 WebSocket 通道需要的控制器能力，scrcpy.Controller 实现它。
type pointerInjector interface {
	inputInjector
	Touch(action scrcpy.TouchAction, pointerID uint64, x, y int, pressure float64) error
	Scroll(x, y int, hscroll, vscroll float64) error
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "设备不存在")
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// 画面已经是 H.264，再压一遍只费 CPU
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept 已经写了 HTTP 错误响应
	}
	defer c.CloseNow()
	c.SetReadLimit(wsReadLimit)

	if s.Scrcpy.ServerJar == "" {
		_ = c.Close(websocket.StatusPolicyViolation, "no_scrcpy")
		return
	}

	// 取消原因要区分开：被接管 → 告诉浏览器别重连；设备侧断了 → 让浏览器重连；
	// 浏览器自己走了 → 什么都不用说。
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	gen, port := s.h264.acquire(d.ID, s.Scrcpy.PortBase, func() { cancel(errTakenOver) })
	defer s.h264.release(d.ID, gen)

	sess, err := s.scrcpyStart(ctx, scrcpy.Options{
		Serial: d.ADBAddr, ServerJar: s.Scrcpy.ServerJar, LocalPort: port,
		MaxFPS: s.Scrcpy.MaxFPS, BitRate: s.Scrcpy.BitRate,
	})
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, closeReason("scrcpy_failed: "+err.Error()))
		return
	}
	defer sess.Close()
	ctrl, err := sess.Control()
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, closeReason("no_control: "+err.Error()))
		return
	}
	s.h264.setController(d.ID, gen, ctrl)

	if err := wsSendJSON(c, map[string]any{
		"t": "hello", "device": sess.Device, "w": sess.Width, "h": sess.Height,
	}); err != nil {
		return
	}

	// 画面静止时泵在 ReadFrame 上等；会话被取消就把读超时拨到过去，让它立刻醒来
	stopPoke := context.AfterFunc(ctx, func() { _ = sess.SetReadDeadline(time.Unix(1, 0)) })
	defer stopPoke()
	go func() {
		err := wsPumpVideo(ctx, c, sess)
		switch cause := context.Cause(ctx); {
		case cause == nil:
			// 泵自己停了 = 设备侧断流，不是浏览器的问题：GoingAway 让它稍后重连
			_ = c.Close(websocket.StatusGoingAway, closeReason("video_ended: "+err.Error()))
		case errors.Is(cause, errTakenOver):
			_ = c.Close(websocket.StatusGoingAway, "taken_over")
		default:
			// ping 失败、浏览器已断：没什么好说的，直接断，让下面的 Read 返回
			_ = c.CloseNow()
		}
		cancel(err)
	}()
	go wsKeepalive(ctx, c, cancel)

	// 读循环故意不用会话 ctx（见文件头注释）：靠对端的 close 帧或 TCP 断开返回
	for {
		typ, data, err := c.Read(context.Background())
		if err != nil {
			cancel(fmt.Errorf("%w: %w", errClientGone, err))
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var in wsInput
		if err := json.Unmarshal(data, &in); err != nil {
			_ = wsSendJSON(c, map[string]any{"t": "error", "msg": "不是合法 JSON"})
			continue
		}
		if err := dispatchWSInput(ctrl, in); err != nil {
			_ = wsSendJSON(c, map[string]any{"t": "error", "msg": err.Error()})
			continue
		}
		if !(in.T == "touch" && in.A == "move") {
			// 操作过后缩略图缓存立刻失效，墙面才能尽快反映变化；move 每秒几十条，不值得
			s.shots.invalidate(d.ADBAddr)
		}
	}
}

// wsPumpVideo 把 scrcpy 的访问单元逐个推到浏览器。返回值说明为什么停：
// ctx 取消（浏览器断开 / 被接管）、设备侧断流、或写超时。
func wsPumpVideo(ctx context.Context, c *websocket.Conn, sess *scrcpy.Session) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 画面静止时 scrcpy 不出帧，用读超时把控制权还回来检查 ctx 与进程存活
		_ = sess.SetReadDeadline(time.Now().Add(5 * time.Second))
		f, err := sess.ReadFrame()
		if err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				if !sess.Alive() {
					return errDeviceGone
				}
				continue
			}
			return err
		}
		var flags byte
		if f.Config {
			flags |= wsFlagConfig
		}
		if f.KeyFrame {
			flags |= wsFlagKey
		}
		if err := wsWriteFrame(c, flags, f.Data); err != nil {
			return err
		}
	}
}

func wsWriteFrame(c *websocket.Conn, flags byte, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
	defer cancel()
	w, err := c.Writer(ctx, websocket.MessageBinary)
	if err != nil {
		return err
	}
	// 分两次写省掉一次整帧拷贝；Writer 会把它们合成一条消息
	if _, err := w.Write([]byte{flags}); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.Close()
}

func wsSendJSON(c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsWriteTimeout)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

// wsKeepalive 定期 ping。浏览器标签页被杀、网线被拔时 TCP 不会主动报错，
// 没有它这条会话会一直挂着占住设备的 scrcpy 编码器。
func wsKeepalive(ctx context.Context, c *websocket.Conn, cancel context.CancelCauseFunc) {
	tick := time.Tick(wsPingInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		pctx, done := context.WithTimeout(context.Background(), wsWriteTimeout)
		err := c.Ping(pctx)
		done()
		if err != nil {
			cancel(fmt.Errorf("ping 失败: %w", err))
			return
		}
	}
}

// dispatchWSInput 把一条上行输入送进控制通道。
func dispatchWSInput(ctrl pointerInjector, in wsInput) error {
	switch in.T {
	case "touch":
		var a scrcpy.TouchAction
		switch in.A {
		case "down":
			a = scrcpy.TouchDown
		case "move":
			a = scrcpy.TouchMove
		case "up":
			a = scrcpy.TouchUp
		default:
			return fmt.Errorf("未知触摸动作 %q", in.A)
		}
		if in.ID < 0 || in.ID >= scrcpy.MaxPointers {
			return fmt.Errorf("指针 id %d 超出 0~%d", in.ID, scrcpy.MaxPointers-1)
		}
		p := in.P
		if a != scrcpy.TouchUp {
			p = cmp.Or(p, 1) // 浏览器一般不带压力，缺省按实按处理
		}
		return ctrl.Touch(a, uint64(in.ID), in.X, in.Y, p)
	case "scroll":
		return ctrl.Scroll(in.X, in.Y, in.DX, in.DY)
	case "tap", "swipe", "key", "text":
		return injectViaController(ctrl, inputReq{
			Type: in.T, X: in.X, Y: in.Y, X2: in.X2, Y2: in.Y2, MS: in.MS, Key: in.Key, Text: in.Text,
		})
	default:
		return fmt.Errorf("%w: %q", errBadInputType, in.T)
	}
}

// closeReason 把 reason 截到 RFC 允许的长度，且不切坏 UTF-8。
func closeReason(s string) string {
	if len(s) <= wsCloseReasonMax {
		return s
	}
	return strings.ToValidUTF8(s[:wsCloseReasonMax-3], "") + "…"
}
