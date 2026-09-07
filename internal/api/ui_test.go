package api

import (
	"net/http"
	"testing"
)

func TestTrailingNumber(t *testing.T) {
	cases := map[string]int{
		"3588-a-1":  1,
		"3588-a-12": 12,
		"dev8":      8,
		"no-digits": 0,
		"":          0,
	}
	for in, want := range cases {
		if got := trailingNumber(in); got != want {
			t.Errorf("trailingNumber(%q) = %d，期望 %d", in, got, want)
		}
	}
}

// 每台设备要拿到互不相同且稳定的端口，否则两台设备的 adb forward 会打架。
func TestUIPortForIsStableAndDistinct(t *testing.T) {
	s := &Server{UI: UIConfig{PortBase: 27400}}
	seen := map[int]string{}
	for _, id := range []string{"3588-a-1", "3588-a-2", "3588-a-8"} {
		p := s.uiPortFor(id)
		if prev, dup := seen[p]; dup {
			t.Errorf("%s 与 %s 撞到同一个端口 %d", id, prev, p)
		}
		seen[p] = id
		if p != s.uiPortFor(id) {
			t.Errorf("%s 的端口不稳定", id)
		}
	}
	if got := s.uiPortFor("3588-a-1"); got != 27401 {
		t.Errorf("3588-a-1 应当是 27401，实际 %d", got)
	}
}

// 没配 dex 时要明确报 503，而不是拿一个空会话去连然后超时——
// 后者会让调用方等 40 秒才知道功能根本没开。
func TestUIDumpUnavailableWithoutDex(t *testing.T) {
	_, h := newServer(t, 1, nil)
	rec := do(t, h, "GET", "/api/devices/dev1/ui", nil, false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未配 dex 应当 503，实际 %d：%s", rec.Code, rec.Body)
	}
}

func TestUIDumpUnknownDevice(t *testing.T) {
	s, h := newServer(t, 1, nil)
	s.UI = UIConfig{DexPath: "/nonexistent.dex", PortBase: 27400}
	rec := do(t, h, "GET", "/api/devices/nope/ui", nil, false)
	if rec.Code != http.StatusNotFound {
		t.Errorf("设备不存在应当 404，实际 %d", rec.Code)
	}
}

// 空池 closeAll 不能 panic —— 关停路径上会无条件调它。
func TestUISessionsCloseAllOnEmptyPool(t *testing.T) {
	u := &uiSessions{}
	if n := u.closeAll(); n != 0 {
		t.Errorf("空池 closeAll 应当返回 0，实际 %d", n)
	}
	u.drop("不存在的设备") // 也不该 panic
}
