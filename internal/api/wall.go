package api

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Screener 是设备墙需要的 adb 能力子集（由 internal/adb.Client 实现）。
type Screener interface {
	ScreenshotJPEG(ctx context.Context, serial string, maxW, quality int) ([]byte, image.Point, error)
	// ScreenshotPassthrough 原样返回设备的 PNG，不解码不缩放（全分辨率流用）。
	ScreenshotPassthrough(ctx context.Context, serial string) ([]byte, image.Point, error)
	Tap(ctx context.Context, serial string, x, y int) error
	Swipe(ctx context.Context, serial string, x1, y1, x2, y2, durationMS int) error
	Key(ctx context.Context, serial, name string) error
	Text(ctx context.Context, serial, s string) error
	Connect(ctx context.Context, addr string) error
}

// shotCache 缓存截图，避免多个浏览器标签页各自打 adb。
// Phase 1 实测 screencap 稳态 0.3 s，缓存 1 s 足以把并发看客压成一次调用。
type shotCache struct {
	mu    sync.Mutex
	items map[string]*shotEntry
}

type shotEntry struct {
	mu   sync.Mutex
	data []byte
	size image.Point
	at   time.Time
}

func (c *shotCache) entry(key string) *shotEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]*shotEntry{}
	}
	e, ok := c.items[key]
	if !ok {
		e = &shotEntry{}
		c.items[key] = e
	}
	return e
}

// get 取一张不超过 ttl 的截图，过期则重新抓。
func (c *shotCache) get(ctx context.Context, s Screener, serial string, maxW, quality int, ttl time.Duration) ([]byte, image.Point, error) {
	e := c.entry(fmt.Sprintf("%s|%d|%d", serial, maxW, quality))
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.data != nil && time.Since(e.at) < ttl {
		return e.data, e.size, nil
	}
	data, size, err := s.ScreenshotJPEG(ctx, serial, maxW, quality)
	if err != nil {
		return nil, image.Point{}, err
	}
	e.data, e.size, e.at = data, size, time.Now()
	return data, size, nil
}

// handleScreenshot 返回一台设备的截图。
// ?w= 缩放宽度（0 = 原始分辨率），?q= JPEG 质量。
// 响应头 X-Device-Width/Height 带原始分辨率，前端靠它把点击坐标换算回设备坐标。
func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if s.Screen == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_adb", "未配置 adb，设备墙不可用")
		return
	}
	d, err := s.Store.GetDevice(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "设备不存在")
		return
	}
	maxW := atoiDefault(r.URL.Query().Get("w"), 480)
	quality := atoiDefault(r.URL.Query().Get("q"), 70)
	ttl := time.Second
	if maxW == 0 {
		ttl = 300 * time.Millisecond // 放大视图要跟手，缓存短一些
	}
	data, size, err := s.shots.get(r.Context(), s.Screen, d.ADBAddr, maxW, quality, ttl)
	if err != nil {
		// 设备可能正在重建；返回 503 让前端显示占位而不是把整页打崩
		writeErr(w, http.StatusServiceUnavailable, "capture_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Device-Width", strconv.Itoa(size.X))
	w.Header().Set("X-Device-Height", strconv.Itoa(size.Y))
	w.Write(data)
}

type inputReq struct {
	Type string `json:"type"` // tap | swipe | key | text
	X    int    `json:"x"`
	Y    int    `json:"y"`
	X2   int    `json:"x2"`
	Y2   int    `json:"y2"`
	MS   int    `json:"ms"`
	Key  string `json:"key"`
	Text string `json:"text"`
}

// handleInput 把操作人员在设备墙上的动作注入设备。
func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	if s.Screen == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_adb", "未配置 adb，设备墙不可用")
		return
	}
	d, err := s.Store.GetDevice(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "设备不存在")
		return
	}
	var req inputReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	ctx := r.Context()
	switch req.Type {
	case "tap":
		err = s.Screen.Tap(ctx, d.ADBAddr, req.X, req.Y)
	case "swipe":
		err = s.Screen.Swipe(ctx, d.ADBAddr, req.X, req.Y, req.X2, req.Y2, req.MS)
	case "key":
		err = s.Screen.Key(ctx, d.ADBAddr, req.Key)
	case "text":
		err = s.Screen.Text(ctx, d.ADBAddr, req.Text)
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "type 必须是 tap/swipe/key/text 之一")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "input_failed", err.Error())
		return
	}
	// 操作过后缓存立刻失效，否则下一帧还是旧画面，点了像没反应
	s.shots.invalidate(d.ADBAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (c *shotCache) invalidate(serial string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.items {
		if len(k) >= len(serial) && k[:len(serial)] == serial {
			e.mu.Lock()
			e.data = nil
			e.mu.Unlock()
		}
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return def
	}
	return v
}

// wallDevice 是设备墙一格所需的全部信息。
type wallDevice struct {
	ID       string     `json:"id"`
	ADBAddr  string     `json:"adb_addr"`
	State    string     `json:"state"`
	Owner    string     `json:"owner,omitempty"`
	Worktree string     `json:"worktree,omitempty"`
	Branch   string     `json:"branch,omitempty"`
	HeadSHA  string     `json:"head_sha,omitempty"`
	Expires  *time.Time `json:"expires_at,omitempty"`
	Human    bool       `json:"human_takeover"`
	Note     string     `json:"human_note,omitempty"`
	LeaseID  string     `json:"lease_id,omitempty"`
}

// wallSnapshot 组装设备墙需要的完整状态。SSE 与 HTTP 两条路共用它，
// 避免两处各拼一份、字段慢慢漂移。
func (s *Server) wallSnapshot() map[string]any {
	devices, err := s.Store.ListDevices()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	leases, err := s.Store.ListLeases()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	byDevice := map[string]int{}
	for i, l := range leases {
		byDevice[l.DeviceID] = i
	}
	out := make([]wallDevice, 0, len(devices))
	for _, d := range devices {
		wd := wallDevice{ID: d.ID, ADBAddr: d.ADBAddr, State: string(d.State)}
		if i, ok := byDevice[d.ID]; ok {
			l := leases[i]
			exp := l.ExpiresAt
			wd.Owner, wd.Worktree, wd.Branch, wd.HeadSHA = l.Owner, l.Worktree, l.Branch, l.HeadSHA
			wd.Expires, wd.Human, wd.Note, wd.LeaseID = &exp, l.HumanTakeover, l.HumanNote, l.ID
		}
		out = append(out, wd)
	}
	resp := map[string]any{"devices": out, "now": time.Now()}
	if s.Health != nil {
		if h, err := s.Health.Health(); err == nil {
			resp["node"] = h
			resp["under_pressure"] = h.UnderPressure(s.MinAvailMiB)
		}
	}
	return resp
}

// handleWallData 设备墙的数据源。SSE 不可用时（老浏览器、代理不透传）
// 前端会退回轮询这个接口。
func (s *Server) handleWallData(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.wallSnapshot())
}

// maxStreams 同时允许的 MJPEG 流数。每条流都在持续打 adb screencap，
// Phase 1 实测单次约 0.3 s 且会吃节点 CPU，放开了会把节点拖垮。
const maxStreams = 4

// handleStream 用 multipart/x-mixed-replace 连续推帧。
//
// 相比「每帧一次 HTTP 拉取」，省掉每帧的连接与请求往返，人工操作时的
// 手感差别很明显。上限由 screencap 本身决定（实测约 0.3 s/帧，即 ~3 fps），
// 再快没有意义——瓶颈在设备侧的软件渲染截屏，不在传输。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if s.Screen == nil {
		writeErr(w, http.StatusServiceUnavailable, "no_adb", "未配置 adb，设备墙不可用")
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
	if n := s.streams.Add(1); n > maxStreams {
		s.streams.Add(-1)
		writeErr(w, http.StatusTooManyRequests, "too_many_streams",
			fmt.Sprintf("同时最多 %d 路实时流，请先关掉别的放大窗口", maxStreams))
		return
	}
	defer s.streams.Add(-1)

	const boundary = "droidpoolframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Device-Width", "0") // 首帧前未知，逐帧头里给准确值
	w.WriteHeader(http.StatusOK)

	quality := atoiDefault(r.URL.Query().Get("q"), 70)
	maxW := atoiDefault(r.URL.Query().Get("w"), 0)

	// 帧去重：借鉴 scrcpy「只在画面变化时出帧」。设备多数时间画面是静止的
	// （等人操作、等网络），照推等同的帧只是白烧节点 CPU 和带宽。
	// 但不能完全不发：浏览器要靠帧到达判断连接还活着，所以静止时降到每 2 s 一帧。
	var lastSum uint64
	lastSent := time.Now()
	const idleKeepalive = 2 * time.Second

	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		var (
			data []byte
			size image.Point
			err  error
			mime = "image/jpeg"
		)
		if maxW == 0 {
			// 全分辨率：直通设备的 PNG，省掉解码+编码的 0.3 s
			data, size, err = s.Screen.ScreenshotPassthrough(r.Context(), d.ADBAddr)
			mime = "image/png"
		} else {
			data, size, err = s.Screen.ScreenshotJPEG(r.Context(), d.ADBAddr, maxW, quality)
		}
		if err != nil {
			// 设备可能正在重建：歇一下再试，不要打死循环
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		sum := checksum(data)
		if sum == lastSum && time.Since(lastSent) < idleKeepalive {
			continue // 画面没变，不占带宽也不让浏览器白解一帧
		}
		lastSum, lastSent = sum, time.Now()

		if _, err := fmt.Fprintf(w,
			"--%s\r\nContent-Type: %s\r\nContent-Length: %d\r\nX-Device-Width: %d\r\nX-Device-Height: %d\r\n\r\n",
			boundary, mime, len(data), size.X, size.Y); err != nil {
			return
		}
		if _, err := w.Write(data); err != nil {
			return // 客户端关了
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return
		}
		flusher.Flush()
	}
}

// checksum 是帧去重用的指纹。
//
// 用全量 crc32（Castagnoli，Go 会走 CPU 的 CRC 指令，约 GB/s 量级）而不是抽样：
// 抽样会漏掉未采样位置的变化，把「画面变了」误判成「没变」而跳过该帧，
// 表现为界面冻住——这比省下的那点 CPU 严重得多。
// 10 万字节全量哈希约 0.1 ms，相对 0.35 s 的帧间隔可以忽略。
func checksum(b []byte) uint64 {
	return uint64(crc32.Checksum(b, crcTable))
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)
