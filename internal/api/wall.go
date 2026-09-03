package api

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Screener 是设备墙需要的 adb 能力子集（由 internal/adb.Client 实现）。
type Screener interface {
	ScreenshotJPEG(ctx context.Context, serial string, maxW, quality int) ([]byte, image.Point, error)
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

// handleWallData 设备墙的数据源：设备 + 其上的租约，一次取齐。
func (s *Server) handleWallData(w http.ResponseWriter, _ *http.Request) {
	devices, err := s.Store.ListDevices()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	leases, err := s.Store.ListLeases()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
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
	writeJSON(w, http.StatusOK, resp)
}
