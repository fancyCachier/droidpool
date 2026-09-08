package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fancyCachier/droidpool/internal/node"
	"github.com/fancyCachier/droidpool/internal/pool"
	"github.com/fancyCachier/droidpool/internal/store"
)

const token = "test-token"

type fakeHealth struct {
	h   *node.Health
	err error
}

func (f fakeHealth) Health() (*node.Health, error) { return f.h, f.err }

func newServer(t *testing.T, devices int, health NodeHealth) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for i := 1; i <= devices; i++ {
		d := &pool.Device{
			ID: fmt.Sprintf("dev%d", i), Node: "n1", Container: "c",
			ADBAddr: fmt.Sprintf("192.168.14.54:%d", 5560+i),
			State:   pool.StateReady, CreatedAt: time.Now(),
		}
		if err := st.UpsertDevice(d); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	s := &Server{
		Store: st, Token: token,
		DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour, MinAvailMiB: 2048,
		Health: health,
		NewID:  func() string { n++; return "L" + string(rune('0'+n)) },
	}
	return s, s.Routes()
}

func do(t *testing.T, h http.Handler, method, path string, body any, withAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, rec.Body.String())
	}
	return v
}

func claimBody(host, wt string) map[string]any {
	return map[string]any{"owner": "dev@" + host, "host": host, "worktree": wt, "branch": "fix/x", "head_sha": "abc"}
}

func TestAuthRequired(t *testing.T) {
	_, h := newServer(t, 1, nil)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/leases"},
		{"GET", "/api/leases"},
		{"GET", "/api/devices"},
		{"DELETE", "/api/leases/L1"},
	} {
		rec := do(t, h, tc.method, tc.path, claimBody("m", "w"), false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 无 token 应 401，得到 %d", tc.method, tc.path, rec.Code)
		}
	}
	// 错误的 token 同样拒绝
	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.Header.Set("Authorization", "Bearer 猜的")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错误 token 应 401，得到 %d", rec.Code)
	}
	// 健康检查不鉴权，供探活
	if rec := do(t, h, "GET", "/api/health", nil, false); rec.Code != http.StatusOK {
		t.Errorf("/api/health 应免鉴权，得到 %d", rec.Code)
	}
}

func TestClaimAndRelease(t *testing.T) {
	_, h := newServer(t, 2, nil)

	rec := do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("首次 claim 应 201，得到 %d: %s", rec.Code, rec.Body)
	}
	got := decode[claimResp](t, rec)
	if got.LeaseID == "" || got.DeviceID == "" {
		t.Fatalf("响应缺字段: %+v", got)
	}
	if got.ADBAddr == "" {
		t.Error("响应必须带 adb_addr，agent 靠它连设备")
	}
	if got.Reused {
		t.Error("首次 claim 不应标记为复用")
	}

	// 幂等：同一 worktree 再来一次
	rec = do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true)
	if rec.Code != http.StatusOK {
		t.Errorf("幂等 claim 应 200 而非 201，得到 %d", rec.Code)
	}
	again := decode[claimResp](t, rec)
	if !again.Reused || again.LeaseID != got.LeaseID {
		t.Errorf("应复用原租约 %s，得到 %+v", got.LeaseID, again)
	}

	// 归还
	if rec := do(t, h, "DELETE", "/api/leases/"+got.LeaseID, nil, true); rec.Code != http.StatusNoContent {
		t.Errorf("归还应 204，得到 %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/leases/"+got.LeaseID, nil, true); rec.Code != http.StatusNotFound {
		t.Errorf("重复归还应 404，得到 %d", rec.Code)
	}
}

func TestClaimPoolExhausted(t *testing.T) {
	_, h := newServer(t, 1, nil)
	do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true)

	rec := do(t, h, "POST", "/api/leases", claimBody("mac", "wt-b"), true)
	if rec.Code != http.StatusConflict {
		t.Fatalf("池满应 409，得到 %d", rec.Code)
	}
	if body := decode[errBody](t, rec); body.Error != "pool_exhausted" {
		t.Errorf("错误类型应为 pool_exhausted，得到 %q", body.Error)
	}
}

func TestClaimValidation(t *testing.T) {
	_, h := newServer(t, 1, nil)
	for _, body := range []map[string]any{
		{"host": "mac"},    // 缺 worktree
		{"worktree": "wt"}, // 缺 host
		{},                 // 都缺
	} {
		if rec := do(t, h, "POST", "/api/leases", body, true); rec.Code != http.StatusBadRequest {
			t.Errorf("缺幂等键字段应 400，得到 %d（body=%v）", rec.Code, body)
		}
	}
	// 非法 JSON
	req := httptest.NewRequest("POST", "/api/leases", bytes.NewBufferString("{不是json"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应 400，得到 %d", rec.Code)
	}
}

func TestClaimTTLClamped(t *testing.T) {
	s, h := newServer(t, 1, nil)
	fixed := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return fixed }

	body := claimBody("mac", "wt-a")
	body["ttl_min"] = 100000 // 远超 MaxTTL
	rec := do(t, h, "POST", "/api/leases", body, true)
	got := decode[claimResp](t, rec)
	want := fixed.Add(4 * time.Hour) // MaxTTL
	if !got.ExpiresAt.Equal(want) {
		t.Errorf("超长 TTL 应被夹到 MaxTTL %v，得到 %v", want, got.ExpiresAt)
	}
}

func TestRenew(t *testing.T) {
	s, h := newServer(t, 1, nil)
	fixed := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return fixed }
	rec := do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true)
	l := decode[claimResp](t, rec)

	s.Now = func() time.Time { return fixed.Add(30 * time.Minute) }
	rec = do(t, h, "POST", "/api/leases/"+l.LeaseID+"/renew", map[string]any{"ttl_min": 60}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("续约应 200，得到 %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	want := fixed.Add(90 * time.Minute)
	if !body.ExpiresAt.Equal(want) {
		t.Errorf("续约后到期应为 %v，得到 %v", want, body.ExpiresAt)
	}

	if rec := do(t, h, "POST", "/api/leases/不存在/renew", nil, true); rec.Code != http.StatusNotFound {
		t.Errorf("续不存在的租约应 404，得到 %d", rec.Code)
	}
}

func TestHumanTakeover(t *testing.T) {
	_, h := newServer(t, 1, nil)
	l := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))

	rec := do(t, h, "POST", "/api/leases/"+l.LeaseID+"/human",
		map[string]any{"takeover": true, "note": "要人工扫码"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("设接管应 200，得到 %d", rec.Code)
	}
	ls := decode[[]*pool.Lease](t, do(t, h, "GET", "/api/leases", nil, true))
	if len(ls) != 1 || !ls[0].HumanTakeover || ls[0].HumanNote != "要人工扫码" {
		t.Errorf("接管标志未反映到列表: %+v", ls)
	}

	do(t, h, "POST", "/api/leases/"+l.LeaseID+"/human", map[string]any{"takeover": false}, true)
	ls = decode[[]*pool.Lease](t, do(t, h, "GET", "/api/leases", nil, true))
	if ls[0].HumanTakeover {
		t.Error("交还后接管标志应清除")
	}
}

// 节点没有余量再装一台设备时就不该放新租约进来。
// 判据用可用内存而非 swap：swap_used 是滞后且黏滞的症状，压测结束数小时后
// 读数依然很高，拿它当闸会在盒子空着时一直拒人（实测残留 426 MiB / 可用 10.9 GB）。
func TestClaimRejectedUnderMemoryPressure(t *testing.T) {
	// 可用内存低于闸门（2048）即拒绝：再放一台进来就会开始换页
	press := fakeHealth{h: &node.Health{MemTotalMiB: 15843, MemAvailMiB: 900, SwapUsedMiB: 590}}
	_, h := newServer(t, 2, press)

	rec := do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("换页中应 503，得到 %d: %s", rec.Code, rec.Body)
	}
	if body := decode[errBody](t, rec); body.Error != "under_pressure" {
		t.Errorf("错误类型应为 under_pressure，得到 %q", body.Error)
	}
}

// 但已持有租约的 agent 重复 claim（幂等复用）不占新设备，必须放行，
// 否则它在压力期间连自己的 adb 地址都查不到。
func TestIdempotentClaimAllowedUnderPressure(t *testing.T) {
	healthy := &fakeHealth{h: &node.Health{MemAvailMiB: 10000}}
	s, h := newServer(t, 2, healthy)

	first := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))

	// 之后节点进入换页
	s.Health = fakeHealth{h: &node.Health{MemAvailMiB: 800}}

	rec := do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("已持有者复用应放行（200），得到 %d: %s", rec.Code, rec.Body)
	}
	again := decode[claimResp](t, rec)
	if !again.Reused || again.LeaseID != first.LeaseID {
		t.Errorf("应返回原租约，得到 %+v", again)
	}
	// 而新来的仍被拒
	if rec := do(t, h, "POST", "/api/leases", claimBody("mac", "wt-b"), true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("换页期间新租约仍应 503，得到 %d", rec.Code)
	}
}

// 取不到节点健康数据时不能连累正常业务（探测失败 ≠ 节点有压力）。
func TestClaimAllowedWhenHealthUnavailable(t *testing.T) {
	_, h := newServer(t, 1, fakeHealth{err: errors.New("ssh 不通")})
	if rec := do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true); rec.Code != http.StatusCreated {
		t.Errorf("健康探测失败时不应拒绝 claim，得到 %d", rec.Code)
	}
}

func TestListEndpointsReturnArrayNotNull(t *testing.T) {
	_, h := newServer(t, 0, nil)
	// 空池时必须返回 []，返回 null 会让前端 .map 崩掉
	if body := do(t, h, "GET", "/api/devices", nil, true).Body.String(); body != "[]\n" {
		t.Errorf("空设备列表应返回 []，得到 %q", body)
	}
	if body := do(t, h, "GET", "/api/leases", nil, true).Body.String(); body != "[]\n" {
		t.Errorf("空租约列表应返回 []，得到 %q", body)
	}
}

func TestHealthEndpointReportsPressure(t *testing.T) {
	_, h := newServer(t, 2, fakeHealth{h: &node.Health{MemAvailMiB: 900, SwapUsedMiB: 590, TempC: 66.5}})
	rec := do(t, h, "GET", "/api/health", nil, false)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["under_pressure"] != true {
		t.Errorf("可用内存低于闸门时 under_pressure 应为 true，得到 %v", body["under_pressure"])
	}
	counts, _ := body["devices"].(map[string]any)
	if counts["ready"] != float64(2) {
		t.Errorf("应报告 2 台 ready，得到 %v", counts)
	}
}

// claim 必须刷新活跃度：否则 agent 反复 claim（比如重启后重连）却不做别的操作时，
// watchdog 会把它误判为僵死并收走设备。
func TestClaimRefreshesActivity(t *testing.T) {
	s, h := newServer(t, 1, nil)
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return t0 }
	l := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))

	// 半小时后再 claim（幂等复用路径）
	t1 := t0.Add(30 * time.Minute)
	s.Now = func() time.Time { return t1 }
	do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true)

	got, err := s.Store.GetLease(l.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeenAt.Equal(t1.UTC()) {
		t.Errorf("复用 claim 应把活跃度刷新到 %v，得到 %v", t1.UTC(), got.LastSeenAt)
	}
}

func TestHeartbeatRefreshesActivityWithoutExtendingTTL(t *testing.T) {
	s, h := newServer(t, 1, nil)
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return t0 }
	l := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))
	before, _ := s.Store.GetLease(l.LeaseID)

	t1 := t0.Add(20 * time.Minute)
	s.Now = func() time.Time { return t1 }
	if rec := do(t, h, "POST", "/api/leases/"+l.LeaseID+"/heartbeat", nil, true); rec.Code != http.StatusOK {
		t.Fatalf("心跳应 200，得到 %d", rec.Code)
	}
	after, _ := s.Store.GetLease(l.LeaseID)
	if !after.LastSeenAt.Equal(t1.UTC()) {
		t.Errorf("心跳应刷新活跃度到 %v，得到 %v", t1.UTC(), after.LastSeenAt)
	}
	// 心跳只证明活着，不等于续租——否则 agent 一直心跳就能无限占机
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("心跳不应延长到期时间：%v → %v", before.ExpiresAt, after.ExpiresAt)
	}

	if rec := do(t, h, "POST", "/api/leases/不存在/heartbeat", nil, true); rec.Code != http.StatusNotFound {
		t.Errorf("对不存在的租约心跳应 404，得到 %d", rec.Code)
	}
}

func TestRenewRefreshesActivity(t *testing.T) {
	s, h := newServer(t, 1, nil)
	t0 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return t0 }
	l := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))

	t1 := t0.Add(15 * time.Minute)
	s.Now = func() time.Time { return t1 }
	do(t, h, "POST", "/api/leases/"+l.LeaseID+"/renew", map[string]any{"ttl_min": 60}, true)

	got, _ := s.Store.GetLease(l.LeaseID)
	if !got.LastSeenAt.Equal(t1.UTC()) {
		t.Errorf("续约也应刷新活跃度，得到 %v", got.LastSeenAt)
	}
}

type recordingResetter struct {
	mu    sync.Mutex
	reset []string
	done  chan string
}

func (r *recordingResetter) Reset(_ context.Context, id string) error {
	r.mu.Lock()
	r.reset = append(r.reset, id)
	r.mu.Unlock()
	if r.done != nil {
		r.done <- id
	}
	return nil
}

// release 之后必须有人去复位，否则设备永远卡在 resetting——首次部署时就撞上了。
func TestReleaseTriggersReset(t *testing.T) {
	s, h := newServer(t, 1, nil)
	rr := &recordingResetter{done: make(chan string, 1)}
	s.Resetter = rr
	l := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))

	if rec := do(t, h, "DELETE", "/api/leases/"+l.LeaseID, nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("release 应 204，得到 %d", rec.Code)
	}
	select {
	case id := <-rr.done:
		if id != l.DeviceID {
			t.Errorf("应复位设备 %s，得到 %s", l.DeviceID, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("release 后 2s 内没有触发复位，设备会卡在 resetting")
	}
}

// 没配 Resetter 时 release 仍要成功（只是设备留在 resetting 等别的机制处理），不能 panic。
func TestReleaseWithoutResetter(t *testing.T) {
	_, h := newServer(t, 1, nil)
	l := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))
	if rec := do(t, h, "DELETE", "/api/leases/"+l.LeaseID, nil, true); rec.Code != http.StatusNoContent {
		t.Errorf("无 Resetter 时 release 也应 204，得到 %d", rec.Code)
	}
}

type fakeEgress struct {
	mu  sync.Mutex
	set map[string]string
	err error
}

func (f *fakeEgress) SetEgress(_ context.Context, id, proxy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.set == nil {
		f.set = map[string]string{}
	}
	f.set[id] = proxy
	return nil
}

func TestSetEgressAppliesAndValidates(t *testing.T) {
	s, h := newServer(t, 1, nil)
	fe := &fakeEgress{}
	s.Egress = fe

	rec := do(t, h, "PUT", "/api/devices/dev1/egress",
		map[string]any{"proxy": "socks5://10.0.0.9:1080"}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 %d：%s", rec.Code, rec.Body)
	}
	if fe.set["dev1"] != "socks5://10.0.0.9:1080" {
		t.Errorf("未下发：%v", fe.set)
	}

	// 空串 = 直连，是合法输入
	if rec := do(t, h, "PUT", "/api/devices/dev1/egress",
		map[string]any{"proxy": ""}, true); rec.Code != http.StatusOK {
		t.Errorf("留空应当表示直连，实际 %d：%s", rec.Code, rec.Body)
	}

	// http:// 会被 gost 当成 HTTP 代理，症状是静悄悄连不上外网，必须挡住
	for _, bad := range []string{"http://1.2.3.4:8080", "1.2.3.4:1080", "socks5://noport"} {
		if rec := do(t, h, "PUT", "/api/devices/dev1/egress",
			map[string]any{"proxy": bad}, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%q 应当被拒，实际 %d", bad, rec.Code)
		}
	}
}

func TestSetEgressNeedsBackend(t *testing.T) {
	s, h := newServer(t, 1, nil)
	s.Egress = &fakeEgress{}
	// 与设备墙其余接口一样不要 token —— 页面自己没有
	if rec := do(t, h, "PUT", "/api/devices/dev1/egress",
		map[string]any{"proxy": ""}, false); rec.Code != http.StatusOK {
		t.Errorf("设备墙接口不该要 token，实际 %d：%s", rec.Code, rec.Body)
	}
	// 没接后端时明确报 503，而不是假装成功
	s.Egress = nil
	if rec := do(t, h, "PUT", "/api/devices/dev1/egress",
		map[string]any{"proxy": ""}, true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未接后端应当 503，实际 %d", rec.Code)
	}
}

func TestSetEgressUnknownDevice(t *testing.T) {
	s, h := newServer(t, 1, nil)
	s.Egress = &fakeEgress{err: store.ErrNotFound}
	if rec := do(t, h, "PUT", "/api/devices/nope/egress",
		map[string]any{"proxy": ""}, true); rec.Code != http.StatusNotFound {
		t.Errorf("设备不存在应当 404，实际 %d", rec.Code)
	}
}

type fakeCamera struct {
	mu  sync.Mutex
	set map[string]string
	err error
}

func (f *fakeCamera) SetCamera(_ context.Context, id, rtsp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.set == nil {
		f.set = map[string]string{}
	}
	f.set[id] = rtsp
	return nil
}

func TestSetCameraAppliesAndValidates(t *testing.T) {
	s, h := newServer(t, 1, nil)
	fc := &fakeCamera{}
	s.Camera = fc

	if rec := do(t, h, "PUT", "/api/devices/dev1/camera",
		map[string]any{"rtsp": "rtsp://cam.lan/live"}, false); rec.Code != http.StatusOK {
		t.Fatalf("状态 %d：%s", rec.Code, rec.Body)
	}
	if fc.set["dev1"] != "rtsp://cam.lan/live" {
		t.Errorf("未下发：%v", fc.set)
	}
	// 空串 = 停流，是合法输入
	if rec := do(t, h, "PUT", "/api/devices/dev1/camera",
		map[string]any{"rtsp": ""}, false); rec.Code != http.StatusOK {
		t.Errorf("留空应当表示停流，实际 %d", rec.Code)
	}
	// 非 rtsp 的地址 ffmpeg 也会试着打开，失败后容器反复重启，
	// 而设备侧只表现为「相机 0 个」，必须在这里挡住
	for _, bad := range []string{"http://cam/live", "cam.lan/live", "rtsp://a b/live"} {
		if rec := do(t, h, "PUT", "/api/devices/dev1/camera",
			map[string]any{"rtsp": bad}, false); rec.Code != http.StatusBadRequest {
			t.Errorf("%q 应当被拒，实际 %d", bad, rec.Code)
		}
	}
}

func TestSetCameraNeedsBackend(t *testing.T) {
	s, h := newServer(t, 1, nil)
	s.Camera = nil
	if rec := do(t, h, "PUT", "/api/devices/dev1/camera",
		map[string]any{"rtsp": ""}, false); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未接后端应当 503，实际 %d", rec.Code)
	}
}

func TestMaskURLCredentials(t *testing.T) {
	cases := map[string]string{
		"rtsp://admin:secret@10.0.0.1:554/live": "rtsp://admin:***@10.0.0.1:554/live",
		"rtsps://u:p@h/x":                       "rtsps://u:***@h/x",
		"rtsp://10.0.0.1:554/live":              "rtsp://10.0.0.1:554/live", // 没凭据就不动
		"":                                      "",
		"rtsp://onlyuser@h/x":                   "rtsp://onlyuser:***@h/x",
		"notaurl":                               "notaurl",
	}
	for in, want := range cases {
		if got := maskURLCredentials(in); got != want {
			t.Errorf("maskURLCredentials(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// /api/wall 是不鉴权的，一条带凭据的摄像头地址等于向整个内网广播。
func TestWallMasksCameraCredentials(t *testing.T) {
	s, h := newServer(t, 1, nil)
	d, err := s.Store.GetDevice("dev1")
	if err != nil {
		t.Fatal(err)
	}
	d.CameraRTSP = "rtsp://admin:hunter2@cam.lan/live"
	if err := s.Store.UpsertDevice(d); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetDeviceCamera("dev1", "rtsp://admin:hunter2@cam.lan/live"); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, "GET", "/api/wall", nil, false)
	body := rec.Body.String()
	if strings.Contains(body, "hunter2") {
		t.Errorf("/api/wall 泄露了摄像头凭据：%s", body)
	}
	if !strings.Contains(body, "admin:***@cam.lan") {
		t.Errorf("应当显示打码后的地址，实际：%s", body)
	}
}

// PUT 的响应同样不能把凭据原样回显——它会被写进日志、SSE、页面。
func TestSetCameraResponseIsMasked(t *testing.T) {
	s, h := newServer(t, 1, nil)
	s.Camera = &fakeCamera{}
	rec := do(t, h, "PUT", "/api/devices/dev1/camera",
		map[string]any{"rtsp": "rtsp://admin:hunter2@cam.lan/live"}, false)
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("响应泄露了凭据：%s", rec.Body)
	}
}

type fakeIdentity struct {
	mu    sync.Mutex
	got   map[string]*pool.Identity
	calls int
}

func (f *fakeIdentity) SetIdentity(_ context.Context, id string, ident *pool.Identity) (*pool.Identity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.got == nil {
		f.got = map[string]*pool.Identity{}
	}
	f.got[id] = ident
	f.calls++
	return ident, true, nil
}

type fakeLocation struct {
	mu  sync.Mutex
	got map[string]string
}

func (f *fakeLocation) SetLocation(_ context.Context, id, loc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.got == nil {
		f.got = map[string]string{}
	}
	f.got[id] = loc
	return nil
}

func TestSetIdentityNormalizesAndValidates(t *testing.T) {
	s, h := newServer(t, 1, nil)
	fi := &fakeIdentity{}
	s.Identity = fi
	rec := do(t, h, "PUT", "/api/devices/dev1/identity", map[string]any{"model": "X1", "brand": "ACME"}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 %d：%s", rec.Code, rec.Body)
	}
	got := fi.got["dev1"]
	if got == nil || got.Manufacturer != "ACME" || got.Device != "x1" || got.Name != "x1" {
		t.Errorf("应把补齐后的身份交给后端: %+v", got)
	}
	resp := decode[map[string]any](t, rec)
	if resp["rebuilt"] != true {
		t.Errorf("响应应告知是否重建: %v", resp)
	}
	// 全空 = 撤销覆盖
	if rec := do(t, h, "PUT", "/api/devices/dev1/identity", map[string]any{}, false); rec.Code != http.StatusOK {
		t.Errorf("空身份应表示撤销，实际 %d", rec.Code)
	}
	if fi.got["dev1"] != nil {
		t.Errorf("撤销应传 nil，实际 %+v", fi.got["dev1"])
	}
	// 非法值进不了 sed
	for _, bad := range []map[string]any{
		{"model": "a/b", "brand": "x"},
		{"brand": "x"},  // 没型号
		{"model": "X1"}, // 没品牌没厂商
		{"model": "X1", "brand": "x", "serial": "AB-1"},
	} {
		if rec := do(t, h, "PUT", "/api/devices/dev1/identity", bad, false); rec.Code != http.StatusBadRequest {
			t.Errorf("%v 应当被拒，实际 %d", bad, rec.Code)
		}
	}
	if fi.calls != 2 {
		t.Errorf("被拒的请求不该到后端，实际调用 %d 次", fi.calls)
	}
}

func TestSetIdentityAndLocationNeedBackend(t *testing.T) {
	s, h := newServer(t, 1, nil)
	s.Identity, s.Location = nil, nil
	if rec := do(t, h, "PUT", "/api/devices/dev1/identity", map[string]any{"model": "X1", "brand": "x"}, false); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未接后端应当 503，实际 %d", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/devices/dev1/location", map[string]any{"location": "1,2"}, false); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未接后端应当 503，实际 %d", rec.Code)
	}
}

func TestSetLocationValidatesAndNormalizes(t *testing.T) {
	s, h := newServer(t, 1, nil)
	fl := &fakeLocation{}
	s.Location = fl
	rec := do(t, h, "PUT", "/api/devices/dev1/location", map[string]any{"location": " 23.1291, 113.2644 "}, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 %d：%s", rec.Code, rec.Body)
	}
	if fl.got["dev1"] != "23.1291,113.2644" {
		t.Errorf("应规范化后下发，实际 %q", fl.got["dev1"])
	}
	if rec := do(t, h, "PUT", "/api/devices/dev1/location", map[string]any{"location": ""}, false); rec.Code != http.StatusOK {
		t.Errorf("留空应表示撤销，实际 %d", rec.Code)
	}
	for _, bad := range []string{"abc", "91,0", "1,2,3", "0,181"} {
		if rec := do(t, h, "PUT", "/api/devices/dev1/location", map[string]any{"location": bad}, false); rec.Code != http.StatusBadRequest {
			t.Errorf("%q 应当被拒，实际 %d", bad, rec.Code)
		}
	}
}

// 设备墙与 /api/devices 都要能看到当前的身份与定位覆盖，操作人员才知道这台机器在装谁。
func TestWallExposesIdentityAndLocation(t *testing.T) {
	s, h := newServer(t, 1, nil)
	id := pool.Identity{Model: "X1", Brand: "ACME"}.Normalized()
	if err := s.Store.SetDeviceIdentity("dev1", &id); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetDeviceLocation("dev1", "23.1,113.2"); err != nil {
		t.Fatal(err)
	}
	body := do(t, h, "GET", "/api/wall", nil, false).Body.String()
	if !strings.Contains(body, `"model":"X1"`) || !strings.Contains(body, `"mock_location":"23.1,113.2"`) {
		t.Errorf("设备墙数据缺身份或定位: %s", body)
	}
	body = do(t, h, "GET", "/api/devices", nil, true).Body.String()
	if !strings.Contains(body, `"model":"X1"`) {
		t.Errorf("/api/devices 缺身份: %s", body)
	}
}
