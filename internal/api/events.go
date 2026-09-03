package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event 推给设备墙的一条状态变更。
type Event struct {
	Kind string    `json:"kind"` // claim | release | human | reset | device | node
	Data any       `json:"data,omitempty"`
	At   time.Time `json:"at"`
}

// Hub 把状态变更广播给所有在看设备墙的浏览器。
//
// 订阅者用带缓冲的 channel，写不进去就丢弃该条消息而不是阻塞发布方：
// 一个卡住的浏览器不该拖慢租约接口。设备墙本来就会定期拉全量快照，
// 丢一条增量事件最多晚几秒看到，不会永久不一致。
type Hub struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

// Subscribe 返回一个事件通道。Hub 为 nil 时返回一个永不来消息的通道，
// 让 SSE 端点退化为「只有心跳」而不是把连接打崩。
func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 16)
	if h == nil {
		return ch
	}
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// Publish 广播一条事件。永不阻塞。
func (h *Hub) Publish(kind string, data any) {
	if h == nil {
		return
	}
	ev := Event{Kind: kind, Data: data, At: time.Now()}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // 订阅者太慢，丢弃这条
		}
	}
}

// Subscribers 当前在看设备墙的连接数。用于决定要不要做后台采集：
// 没人看时就别去打扰节点（Phase 1 实测 8 台 1fps 截图会吃掉可观的 CPU）。
func (h *Hub) Subscribers() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// handleEvents 是设备墙的实时通道（SSE）。
// 只推「有什么变了」，画面仍走 HTTP 拉取——图是二进制，塞进 SSE 要 base64
// 膨胀三分之一，且会把慢客户端的积压变成内存问题。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// 反向代理（如 nginx）默认会缓冲，缓冲下 SSE 完全不工作
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := s.Events.Subscribe()
	defer s.Events.Unsubscribe(ch)

	// 首帧给一份全量快照，浏览器不必再单独拉一次
	s.writeSSE(w, flusher, Event{Kind: "snapshot", Data: s.wallSnapshot(), At: time.Now()})

	// 心跳兼做节点状态刷新：节点指标没有「变更事件」可挂，只能定期采。
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			// 任何变更都推一份新快照：设备墙要的是「现在什么样」，
			// 让前端自己合并增量只会引入两边状态不一致的 bug。
			ev.Data = s.wallSnapshot()
			s.writeSSE(w, flusher, ev)
		case <-tick.C:
			s.writeSSE(w, flusher, Event{Kind: "tick", Data: s.wallSnapshot(), At: time.Now()})
		}
	}
}

func (s *Server) writeSSE(w http.ResponseWriter, f http.Flusher, ev Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, b)
	f.Flush()
}
