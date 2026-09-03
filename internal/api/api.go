// Package api 提供 droidpoold 的 HTTP 接口（内网，Bearer token 鉴权）。
package api

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fancyCachier/droidpool/internal/node"
	"github.com/fancyCachier/droidpool/internal/pool"
	"github.com/fancyCachier/droidpool/internal/store"
)

//go:embed web/*.html
var webFS embed.FS

// Clock 便于测试注入时间。
type Clock func() time.Time

// NodeHealth 取节点健康快照，用于 claim 前的准入判断。
type NodeHealth interface {
	Health() (*node.Health, error)
}

type Server struct {
	Store       *store.Store
	Token       string
	DefaultTTL  time.Duration
	MaxTTL      time.Duration
	MinAvailMiB int
	Now         Clock
	Health      NodeHealth // 可为 nil（无节点健康源时跳过准入检查）
	NewID       func() string
	Log         *slog.Logger
	// Screen 提供截图与输入注入；为 nil 时设备墙的图像与操作接口返回 503，
	// 租约接口不受影响。
	Screen Screener
	shots  shotCache
	// Events 为 nil 时不广播，SSE 端点仍可用但只有心跳。
	Events  *Hub
	streams atomic.Int32
	// Scrcpy 为空时 H.264 端点返回 503，前端自动退回 screencap 流。
	Scrcpy ScrcpyConfig
	h264   h264Sessions
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) newID() string {
	if s.NewID != nil {
		return s.NewID()
	}
	return fmt.Sprintf("L%d", time.Now().UnixNano())
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth) // 不鉴权，给探活用
	mux.Handle("POST /api/leases", s.auth(s.handleClaim))
	mux.Handle("GET /api/leases", s.auth(s.handleListLeases))
	mux.Handle("POST /api/leases/{id}/renew", s.auth(s.handleRenew))
	mux.Handle("POST /api/leases/{id}/heartbeat", s.auth(s.handleHeartbeat))
	mux.Handle("POST /api/leases/{id}/human", s.auth(s.handleHuman))
	mux.Handle("DELETE /api/leases/{id}", s.auth(s.handleRelease))
	mux.Handle("GET /api/devices", s.auth(s.handleListDevices))

	// 设备墙：内网工具，不鉴权。
	// agent 侧的租约接口仍要 token（它们会改变谁持有哪台机器）；
	// 设备墙只是给同一内网的操作人员看画面、点屏幕，加 token 只会挡住自己人。
	mux.HandleFunc("GET /api/wall", s.handleWallData)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/devices/{id}/screenshot.jpg", s.handleScreenshot)
	mux.HandleFunc("GET /api/devices/{id}/stream.mjpg", s.handleStream)
	mux.HandleFunc("GET /api/devices/{id}/stream.h264", s.handleH264Stream)
	mux.HandleFunc("POST /api/devices/{id}/input", s.handleInput)
	mux.HandleFunc("GET /{$}", s.servePage("web/wall.html"))
	mux.HandleFunc("GET /device/{id}", s.servePage("web/device.html"))
	return mux
}

func (s *Server) servePage(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		b, err := webFS.ReadFile(name)
		if err != nil {
			http.Error(w, "页面缺失", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	}
}

// auth 校验 Bearer token。内网工具不做用户体系，agent 与设备墙共用一个 token。
func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// 长度先比，避免把 token 长度也泄露给暴力枚举
		if s.Token == "" || got != s.Token {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "token 无效")
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// QueuePosition 仅在池满时有意义
	QueuePosition int `json:"queue_position,omitempty"`
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, errBody{Error: kind, Message: msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	devices, err := s.Store.ListDevices()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	counts := map[string]int{}
	for _, d := range devices {
		counts[string(d.State)]++
	}
	resp := map[string]any{"ok": true, "devices": counts}
	if s.Health != nil {
		if h, err := s.Health.Health(); err == nil {
			resp["node"] = h
			resp["under_pressure"] = h.UnderPressure(s.MinAvailMiB)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type claimReq struct {
	Owner    string `json:"owner"`
	Host     string `json:"host"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	HeadSHA  string `json:"head_sha"`
	TTLMin   int    `json:"ttl_min"`
}

type claimResp struct {
	LeaseID   string    `json:"lease_id"`
	DeviceID  string    `json:"device_id"`
	ADBAddr   string    `json:"adb_addr"`
	ExpiresAt time.Time `json:"expires_at"`
	Reused    bool      `json:"reused"`
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req claimReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	if req.Host == "" || req.Worktree == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "host 与 worktree 必填，它们组成幂等键")
		return
	}

	ttl := s.DefaultTTL
	if req.TTLMin > 0 {
		ttl = time.Duration(req.TTLMin) * time.Minute
	}
	if s.MaxTTL > 0 && ttl > s.MaxTTL {
		ttl = s.MaxTTL
	}

	now := s.now()
	l := &pool.Lease{
		ID: s.newID(), Owner: req.Owner, Host: req.Host, Worktree: req.Worktree,
		Branch: req.Branch, HeadSHA: req.HeadSHA,
		CreatedAt: now, ExpiresAt: now.Add(ttl), EdgeMode: pool.EdgeShared,
	}

	// 准入：节点已在换页就不再放新租约进来。Phase 1 实测 swap 一出现，
	// 失败率立刻从 0 跳到 5.6%，再放人进来只会一起变慢。
	// 幂等复用不受此限——那不占新设备。
	if s.Health != nil {
		if h, err := s.Health.Health(); err == nil && h.UnderPressure(s.MinAvailMiB) {
			if _, err := s.Store.GetLeaseByWorktree(req.Host, req.Worktree); errors.Is(err, store.ErrNotFound) {
				writeJSON(w, http.StatusServiceUnavailable, errBody{
					Error:   "under_pressure",
					Message: fmt.Sprintf("节点内存不足（可用 %d MiB < %d MiB），暂不接受新租约", h.MemAvailMiB, s.MinAvailMiB),
				})
				return
			}
		}
	}

	got, reused, err := s.Store.Claim(l, now)
	switch {
	case errors.Is(err, store.ErrNoFreeDevice):
		writeJSON(w, http.StatusConflict, errBody{Error: "pool_exhausted", Message: "池中无空闲设备"})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	d, err := s.Store.GetDevice(got.DeviceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// 刷新活跃度：claim 本身就是一次 agent 活动，复用路径同样算数，
	// 否则 watchdog 会把「反复 claim 但没别的动作」的 agent 误判为僵死。
	if err := s.Store.Touch(got.ID, now); err != nil {
		s.logTouchFailure(got.ID, err)
	}

	if !reused {
		s.Events.Publish("claim", map[string]any{"lease": got.ID, "device": got.DeviceID, "worktree": got.Worktree})
	}

	code := http.StatusCreated
	if reused {
		code = http.StatusOK
	}
	writeJSON(w, code, claimResp{
		LeaseID: got.ID, DeviceID: got.DeviceID, ADBAddr: d.ADBAddr,
		ExpiresAt: got.ExpiresAt, Reused: reused,
	})
}

func (s *Server) handleListLeases(w http.ResponseWriter, _ *http.Request) {
	ls, err := s.Store.ListLeases()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if ls == nil {
		ls = []*pool.Lease{}
	}
	writeJSON(w, http.StatusOK, ls)
}

func (s *Server) handleListDevices(w http.ResponseWriter, _ *http.Request) {
	ds, err := s.Store.ListDevices()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if ds == nil {
		ds = []*pool.Device{}
	}
	writeJSON(w, http.StatusOK, ds)
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		TTLMin int `json:"ttl_min"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 空体合法，用默认 TTL

	ttl := s.DefaultTTL
	if req.TTLMin > 0 {
		ttl = time.Duration(req.TTLMin) * time.Minute
	}
	if s.MaxTTL > 0 && ttl > s.MaxTTL {
		ttl = s.MaxTTL
	}
	newExpiry := s.now().Add(ttl)
	if err := s.Store.Renew(id, newExpiry); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "租约不存在或已归还")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if err := s.Store.Touch(id, s.now()); err != nil {
		s.logTouchFailure(id, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"expires_at": newExpiry})
}

// handleHeartbeat 只刷新活跃度，不动到期时间。
// agent 用它告诉 watchdog「我还活着」，与「我要延长租期」是两回事：
// 前者防僵死误杀，后者才是真的续租。
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	now := s.now()
	if err := s.Store.Touch(id, now); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "租约不存在或已归还")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last_seen_at": now})
}

func (s *Server) logTouchFailure(id string, err error) {
	// 刷新活跃度失败不该让主流程失败——最坏结果是 watchdog 早一点回收，
	// 不是把 agent 的正常操作打回。
	if s.Log != nil {
		s.Log.Warn("刷新租约活跃度失败", "lease", id, "err", err)
	}
}

func (s *Server) handleHuman(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Takeover bool   `json:"takeover"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	if err := s.Store.SetHumanTakeover(id, req.Takeover, req.Note); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "租约不存在或已归还")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.Events.Publish("human", map[string]any{"lease": id, "takeover": req.Takeover})
	writeJSON(w, http.StatusOK, map[string]any{"human_takeover": req.Takeover})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	devID, err := s.Store.Release(id, s.now())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "租约不存在或已归还")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.Events.Publish("release", map[string]any{"lease": id, "device": devID})
	w.WriteHeader(http.StatusNoContent)
}
