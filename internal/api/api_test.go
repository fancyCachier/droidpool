package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
			ID: "dev" + string(rune('0'+i)), Node: "n1", Container: "c",
			ADBAddr: "192.168.14.54:556" + string(rune('0'+i)),
			State:   pool.StateReady, CreatedAt: time.Now(),
		}
		if err := st.UpsertDevice(d); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	s := &Server{
		Store: st, Token: token,
		DefaultTTL: time.Hour, MaxTTL: 4 * time.Hour, SwapGuardMiB: 256,
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
	return map[string]any{"owner": "woo@" + host, "host": host, "worktree": wt, "branch": "fix/x", "head_sha": "abc"}
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

// 节点一旦开始换页就不该再放新租约进来：Phase 1 实测 swap 从 0 变正的同时
// 失败率从 0 跳到 5.6%，p95 冲到 2.66×。
func TestClaimRejectedUnderMemoryPressure(t *testing.T) {
	press := fakeHealth{h: &node.Health{MemTotalMiB: 15843, MemAvailMiB: 3000, SwapUsedMiB: 590}}
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
	healthy := &fakeHealth{h: &node.Health{SwapUsedMiB: 0}}
	s, h := newServer(t, 2, healthy)

	first := decode[claimResp](t, do(t, h, "POST", "/api/leases", claimBody("mac", "wt-a"), true))

	// 之后节点进入换页
	s.Health = fakeHealth{h: &node.Health{SwapUsedMiB: 990}}

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
	_, h := newServer(t, 2, fakeHealth{h: &node.Health{SwapUsedMiB: 590, TempC: 66.5}})
	rec := do(t, h, "GET", "/api/health", nil, false)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["under_pressure"] != true {
		t.Errorf("swap 超阈值时 under_pressure 应为 true，得到 %v", body["under_pressure"])
	}
	counts, _ := body["devices"].(map[string]any)
	if counts["ready"] != float64(2) {
		t.Errorf("应报告 2 台 ready，得到 %v", counts)
	}
}
