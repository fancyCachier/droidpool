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
type h264Sessions struct {
	mu    sync.Mutex
	inUse map[string]bool
	next  int32
}

func (h *h264Sessions) acquire(id string, portBase int) (port int, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inUse == nil {
		h.inUse = map[string]bool{}
	}
	if h.inUse[id] {
		return 0, false
	}
	h.inUse[id] = true
	h.next = (h.next + 1) % 64
	return portBase + int(h.next), true
}

func (h *h264Sessions) release(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.inUse, id)
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
	port, ok := s.h264.acquire(d.ID, s.Scrcpy.PortBase)
	if !ok {
		writeErr(w, http.StatusConflict, "session_busy", "这台设备已有一路 H.264 会话")
		return
	}
	defer s.h264.release(d.ID)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sess, err := scrcpy.Start(ctx, scrcpy.Options{
		Serial: d.ADBAddr, ServerJar: s.Scrcpy.ServerJar, LocalPort: port,
		MaxFPS: s.Scrcpy.MaxFPS, BitRate: s.Scrcpy.BitRate,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "scrcpy_failed", err.Error())
		return
	}
	defer sess.Close()

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
				continue // 静止，不是故障
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
