package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fancyCachier/droidpool/internal/scrcpy"
)

// ScrcpyConfig 由守护进程注入；ServerJar 为空则 H.264 端点整体不可用，
// 前端会自动退回 screencap 那条（慢但零依赖）。
type ScrcpyConfig struct {
	ServerJar string
	PortBase  int // 每个会话占一个 adb forward 端口
	MaxFPS    int
	BitRate   int
}

// h264Sessions 保证同一台设备同时只有一个 scrcpy 会话：
// scrcpy 服务端每个实例都会独占编码器，开两个只会互相拖慢。
//
// 但「已有会话就拒绝」在实践中是错的：操作人员按一次 reload，旧页面的流还没
// 断干净新页面就来了，被自己上一次的会话挡在外面拿到 409。所以改成**后来者接管**：
// 新连接到达时取消旧会话，旧的流循环收到取消信号后退出并清理。
type h264Sessions struct {
	mu   sync.Mutex
	live map[string]*liveSession
	next int32
}

type liveSession struct {
	cancel context.CancelFunc
	ctrl   inputInjector
	gen    uint64 // 代数：release 时用它确认清理的是自己那一代，不是接管者
}

// inputInjector 是 /input 需要的最小能力，scrcpy.Controller 实现它。
type inputInjector interface {
	Tap(x, y int) error
	Swipe(x1, y1, x2, y2 int, dur time.Duration) error
	Key(keycode uint32) error
	Text(s string) error
}

// acquire 为设备登记一个新会话，取消同设备上已有的旧会话。返回本会话的代数与端口。
func (h *h264Sessions) acquire(id string, portBase int, cancel context.CancelFunc) (gen uint64, port int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.live == nil {
		h.live = map[string]*liveSession{}
	}
	if old, ok := h.live[id]; ok {
		old.cancel() // 让旧的流循环退出；它的 release 会因代数不符而不动新条目
	}
	h.next++
	gen = uint64(h.next)
	h.live[id] = &liveSession{cancel: cancel, gen: gen}
	// 端口按代数轮换，避免旧会话还没放掉 forward 时新会话撞上同一个端口
	return gen, portBase + int(gen%64)
}

func (h *h264Sessions) setController(id string, gen uint64, c inputInjector) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ls, ok := h.live[id]; ok && ls.gen == gen {
		ls.ctrl = c
	}
}

// controller 返回设备当前的快速输入通道，没有活跃会话时返回 nil。
func (h *h264Sessions) controller(id string) inputInjector {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ls, ok := h.live[id]; ok {
		return ls.ctrl
	}
	return nil
}

// release 只清理属于本代的条目：若已被接管，新条目不能被旧会话的收尾误删。
func (h *h264Sessions) release(id string, gen uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ls, ok := h.live[id]; ok && ls.gen == gen {
		delete(h.live, id)
	}
}

// handleH264Stream 用 multipart 推 H.264 访问单元。
//
// 复用 multipart 而不是新引一个 WebSocket 库：浏览器侧已经有一套跑通的
// multipart 解析（原来喂 createImageBitmap，现在喂 WebCodecs），零新依赖。
// 每个 part 的头里带 config/key 标志，前端据此决定何时能开始解码。
func (s *Server) handleH264Stream(w http.ResponseWriter, r *http.Request) {
	if s.Scrcpy.ServerJar == "" {
		writeErr(w, http.StatusServiceUnavailable, "no_scrcpy", "未配置 scrcpy-server，H.264 流不可用")
		return
	}
	d, err := s.Store.GetDevice(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "设备不存在")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "不支持流式响应")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	gen, port := s.h264.acquire(d.ID, s.Scrcpy.PortBase, cancel)
	defer s.h264.release(d.ID, gen)
	sess, err := scrcpy.Start(ctx, scrcpy.Options{
		Serial: d.ADBAddr, ServerJar: s.Scrcpy.ServerJar, LocalPort: port,
		MaxFPS: s.Scrcpy.MaxFPS, BitRate: s.Scrcpy.BitRate,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "scrcpy_failed", err.Error())
		return
	}
	defer sess.Close()
	if ctrl, err := sess.Control(); err == nil {
		s.h264.setController(d.ID, gen, ctrl)
	}

	const boundary = "droidpoolh264"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Device-Width", strconv.Itoa(sess.Width))
	w.Header().Set("X-Device-Height", strconv.Itoa(sess.Height))
	w.WriteHeader(http.StatusOK)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// 画面静止时 scrcpy 不出帧，用读超时把控制权还回来检查 ctx，
		// 否则浏览器关了这边还挂在 Read 上，会话与设备端进程都泄漏。
		_ = sess.SetReadDeadline(time.Now().Add(5 * time.Second))
		f, err := sess.ReadFrame()
		if err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				// 静止不是故障，但要确认服务端进程还活着——设备侧进程死了
				// 视频 socket 不一定立刻报错，会一直「静止」下去，会话表里
				// 留着一个僵尸控制器，/input 往死 socket 写还显示成功。
				if !sess.Alive() {
					return
				}
				continue
			}
			return
		}
		flags := ""
		if f.Config {
			flags += "config "
		}
		if f.KeyFrame {
			flags += "key"
		}
		if _, err := fmt.Fprintf(w,
			"--%s\r\nContent-Type: video/h264\r\nContent-Length: %d\r\nX-Frame-Flags: %s\r\n\r\n",
			boundary, len(f.Data), flags); err != nil {
			return
		}
		if _, err := w.Write(f.Data); err != nil {
			return
		}
		if _, err := w.Write([]byte("\r\n")); err != nil {
			return
		}
		flusher.Flush()
	}
}
