package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHubBroadcasts(t *testing.T) {
	h := NewHub()
	a, b := h.Subscribe(), h.Subscribe()
	defer h.Unsubscribe(a)
	defer h.Unsubscribe(b)

	if h.Subscribers() != 2 {
		t.Errorf("应有 2 个订阅者，得到 %d", h.Subscribers())
	}
	h.Publish("claim", map[string]any{"lease": "L1"})

	for i, ch := range []chan Event{a, b} {
		select {
		case ev := <-ch:
			if ev.Kind != "claim" {
				t.Errorf("订阅者 %d 收到 kind=%q", i, ev.Kind)
			}
			if ev.At.IsZero() {
				t.Errorf("订阅者 %d 收到的事件没有时间戳", i)
			}
		case <-time.After(time.Second):
			t.Errorf("订阅者 %d 没收到广播", i)
		}
	}
}

// 一个卡住的浏览器不该拖慢租约接口：写不进就丢，绝不阻塞发布方。
func TestHubNeverBlocksOnSlowSubscriber(t *testing.T) {
	h := NewHub()
	ch := h.Subscribe()
	defer h.Unsubscribe(ch)

	done := make(chan struct{})
	go func() {
		// 远超 channel 缓冲（16），慢订阅者一条都不读
		for i := 0; i < 500; i++ {
			h.Publish("tick", i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("发布被慢订阅者阻塞了")
	}
	// 缓冲里的照常可读，只是后面的被丢弃
	select {
	case <-ch:
	default:
		t.Error("缓冲内的事件应仍可读")
	}
}

func TestHubUnsubscribeIsIdempotent(t *testing.T) {
	h := NewHub()
	ch := h.Subscribe()
	h.Unsubscribe(ch)
	// 重复退订不能 panic（close 已关闭的 channel 会 panic）
	h.Unsubscribe(ch)
	if h.Subscribers() != 0 {
		t.Errorf("退订后不应还有订阅者，得到 %d", h.Subscribers())
	}
	// 退订后再发布不应 panic
	h.Publish("claim", nil)
}

func TestHubNilSafe(t *testing.T) {
	var h *Hub
	h.Publish("claim", nil) // 不该 panic
	if h.Subscribers() != 0 {
		t.Error("nil Hub 的订阅者数应为 0")
	}
}

func TestHubConcurrentPublishAndSubscribe(t *testing.T) {
	h := NewHub()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.Publish("tick", nil) }()
		go func() {
			defer wg.Done()
			ch := h.Subscribe()
			time.Sleep(time.Millisecond)
			h.Unsubscribe(ch)
		}()
	}
	wg.Wait()
}

func TestSSEStreamsSnapshotThenEvents(t *testing.T) {
	s, h := newServer(t, 2, nil)
	s.Events = NewHub()

	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type 应为 text/event-stream，得到 %q", ct)
	}
	// 反向代理会缓冲 SSE，缓冲下整条通道失效
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("应设置 X-Accel-Buffering: no，否则经 nginx 时 SSE 不工作")
	}

	rd := bufio.NewReader(resp.Body)
	// 首帧必须是全量快照，前端才不用另外拉一次
	first, err := readSSEEvent(rd)
	if err != nil {
		t.Fatalf("读首帧失败: %v", err)
	}
	if first.name != "snapshot" {
		t.Errorf("首帧应为 snapshot，得到 %q", first.name)
	}
	if !strings.Contains(first.data, `"devices"`) {
		t.Errorf("快照应含设备列表，得到 %s", first.data)
	}

	// 变更事件推送
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.Events.Publish("claim", map[string]any{"lease": "L9"})
	}()
	second, err := readSSEEvent(rd)
	if err != nil {
		t.Fatalf("读第二帧失败: %v", err)
	}
	// 心跳也可能先到，两者都合法
	if second.name != "claim" && second.name != "tick" {
		t.Errorf("第二帧应为 claim 或 tick，得到 %q", second.name)
	}
	// 无论哪种，都要带完整快照——前端只认「现在什么样」
	if !strings.Contains(second.data, `"devices"`) {
		t.Errorf("事件应携带完整快照，得到 %s", second.data)
	}
}

type sseEvent struct{ name, data string }

func readSSEEvent(rd *bufio.Reader) (sseEvent, error) {
	var ev sseEvent
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		case line == "" && ev.name != "":
			return ev, nil
		}
	}
}

// 客户端断开后订阅要被清理，否则每次刷新页面都泄漏一个 goroutine 和 channel。
func TestSSEUnsubscribesOnDisconnect(t *testing.T) {
	s, h := newServer(t, 1, nil)
	s.Events = NewHub()
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rd := bufio.NewReader(resp.Body)
	if _, err := readSSEEvent(rd); err != nil { // 等连接真正建立
		t.Fatal(err)
	}
	if s.Events.Subscribers() != 1 {
		t.Fatalf("应有 1 个订阅者，得到 %d", s.Events.Subscribers())
	}

	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Events.Subscribers() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("断开后订阅未清理，仍有 %d 个", s.Events.Subscribers())
}

// Hub 为 nil 时 SSE 端点应退化为只有心跳，而不是 panic 打崩连接。
func TestSSEWithNilHub(t *testing.T) {
	_, h := newServer(t, 1, nil) // 没有设置 s.Events
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("nil Hub 时 SSE 应仍可连接: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d", resp.StatusCode)
	}
	ev, err := readSSEEvent(bufio.NewReader(resp.Body))
	if err != nil {
		t.Fatalf("应能读到首帧快照: %v", err)
	}
	if ev.name != "snapshot" {
		t.Errorf("首帧应为 snapshot，得到 %q", ev.name)
	}
}

func TestNilHubSubscribeIsSafe(t *testing.T) {
	var h *Hub
	ch := h.Subscribe() // 不该 panic
	if ch == nil {
		t.Fatal("应返回一个可用的通道")
	}
	h.Unsubscribe(ch) // 也不该 panic
}
