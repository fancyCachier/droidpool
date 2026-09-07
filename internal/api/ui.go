package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fancyCachier/droidpool/internal/uiagent"
)

// UIConfig 界面层级接口的参数。DexPath 为空时该接口返回 503，其余功能不受影响。
type UIConfig struct {
	DexPath  string // uiagent.dex 路径（device/uiagent/build.sh 编出来）
	PortBase int    // 每台设备占一个本机端口做 adb forward
}

// uiSessions 按设备维持 uiagent 会话。
//
// 池化的收益就是这个功能的全部意义：冷启动要推 dex、拉 ART 进程、等
// UiAutomation 连上，约 860 ms；接上现成会话之后一次 dump 约 20 ms
// （对比 `uiautomator dump` 的约 380 ms，2026-09-07 实测）。droidpoold 是
// 长活进程，会话留在这里就不必每次重来。
type uiSessions struct {
	mu   sync.Mutex
	live map[string]*uiagent.Session
}

// get 取该设备的会话，没有就建一个。同设备的并发请求会串行等在锁上——
// dump 是毫秒级，而并发建会话只会互相踩（agent 一台设备只该有一个）。
func (u *uiSessions) get(ctx context.Context, id, serial string, cfg UIConfig, portFor func(string) int) (*uiagent.Session, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.live == nil {
		u.live = map[string]*uiagent.Session{}
	}
	if s, ok := u.live[id]; ok {
		return s, nil
	}
	s, err := uiagent.Start(ctx, uiagent.Options{
		Serial: serial, DexPath: cfg.DexPath, LocalPort: portFor(id),
	})
	if err != nil {
		return nil, err
	}
	u.live[id] = s
	return s, nil
}

// drop 丢掉某台设备的会话。设备复位后容器是新的，旧会话的 forward 指向的
// 端口后面没人了，必须丢掉重建，否则一直报连不上。
func (u *uiSessions) drop(id string) {
	u.mu.Lock()
	s := u.live[id]
	delete(u.live, id)
	u.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// closeAll 关停时收掉所有会话，返回数量。
func (u *uiSessions) closeAll() int {
	u.mu.Lock()
	live := u.live
	u.live = nil
	u.mu.Unlock()
	for _, s := range live {
		_ = s.Close()
	}
	return len(live)
}

// handleUIDump 返回设备当前的界面层级 XML。
func (s *Server) handleUIDump(w http.ResponseWriter, r *http.Request) {
	if s.UI.DexPath == "" {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "未配置 uiagent.dex，界面层级接口不可用")
		return
	}
	id := r.PathValue("id")
	d, err := s.Store.GetDevice(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "设备不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()

	sess, err := s.ui.get(ctx, id, d.ADBAddr, s.UI, s.uiPortFor)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "uiagent", err.Error())
		return
	}
	xml, err := sess.Dump()
	if err != nil {
		// 设备可能复位过：丢掉会话让下次重建，否则会一直对着死掉的 forward 报错
		s.ui.drop(id)
		writeErr(w, http.StatusBadGateway, "uiagent", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml))
}

// uiPortFor 给设备分配一个稳定的本机端口做 adb forward。
//
// 取设备 id 末尾的序号（3588-a-1 → 1）。稳定是有意的：重启 droidpoold 后
// 同一台设备还是同一个端口，排查时 `adb forward --list` 能直接对上号。
func (s *Server) uiPortFor(id string) int {
	base := s.UI.PortBase
	if base == 0 {
		base = 27400
	}
	return base + trailingNumber(id)
}

// trailingNumber 返回字符串末尾那串数字，没有则 0。
func trailingNumber(s string) int {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return 0
	}
	return n
}
