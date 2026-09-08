package pool

import (
	"context"
	"strings"
	"testing"
)

func TestIdentityNormalizedFillsDerivedFields(t *testing.T) {
	got := Identity{Model: "X1 Pro", Brand: "ACME"}.Normalized()
	want := Identity{Model: "X1 Pro", Brand: "ACME", Manufacturer: "ACME", Device: "x1pro", Name: "x1pro"}
	if got != want {
		t.Errorf("Normalized = %+v，期望 %+v", got, want)
	}
	// 只给厂商时品牌跟着厂商走；显式给的字段不动
	got = Identity{Model: "X", Manufacturer: "Acme", Device: "dev", Name: "prod"}.Normalized()
	if got.Brand != "Acme" || got.Device != "dev" || got.Name != "prod" {
		t.Errorf("显式字段被改了或品牌没兜底: %+v", got)
	}
}

func TestIdentityValidate(t *testing.T) {
	ok := Identity{Model: "X1", Brand: "ACME"}.Normalized()
	if err := ok.Validate(); err != nil {
		t.Errorf("合法身份被拒: %v", err)
	}
	for name, bad := range map[string]Identity{
		"没型号":         {Brand: "ACME"},
		"没品牌没厂商":      {Model: "X1"},
		"型号带斜杠":       {Model: "a/b", Brand: "x"},
		"型号带 sed 元字符": {Model: "a&b", Brand: "x"},
		"型号带引号":       {Model: "a'b", Brand: "x"},
		"型号太长":        {Model: strings.Repeat("a", 65), Brand: "x"},
		"序列号带横杠":      {Model: "a", Brand: "x", Serial: "AB-1"},
	} {
		if err := bad.Normalized().Validate(); err == nil {
			t.Errorf("%s 应被拒: %+v", name, bad)
		}
	}
}

func TestParseLocation(t *testing.T) {
	lat, lng, err := ParseLocation("23.1291, 113.2644")
	if err != nil || lat != 23.1291 || lng != 113.2644 {
		t.Errorf("ParseLocation = %v %v %v", lat, lng, err)
	}
	if _, _, err := ParseLocation(""); err != nil {
		t.Errorf("空串应合法（表示不 mock）: %v", err)
	}
	for _, bad := range []string{"abc", "1", "1,2,3", "91,0", "0,181", "-91,0", "x,1"} {
		if _, _, err := ParseLocation(bad); err == nil {
			t.Errorf("%q 应被拒", bad)
		}
	}
	if got := FormatLocation(23.1291, 113.2644); got != "23.1291,113.2644" {
		t.Errorf("FormatLocation = %q", got)
	}
}

// 身份是开机定死的，改它必须重建；租约要保住，不然 agent 刚 claim 到的
// 设备就被放回池子了。
func TestSetIdentityRebuildsAndKeepsLease(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateLeased})
	m := newManager(drv, st, 1)
	eff, rebuilt, err := m.SetIdentity(context.Background(), "3588-a-1", &Identity{Model: "X1", Brand: "ACME"})
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt {
		t.Error("身份变了应重建")
	}
	if eff == nil || eff.Manufacturer != "ACME" || eff.Device != "x1" {
		t.Errorf("生效身份应是补齐后的: %+v", eff)
	}
	if len(drv.wiped) != 1 || len(drv.created) != 1 {
		t.Errorf("应清数据并重建一次，实际 wiped=%v created=%v", drv.wiped, drv.created)
	}
	if got := drv.identities["3588-a-1"]; got == nil || got.Model != "X1" {
		t.Errorf("Create 没拿到新身份: %+v", got)
	}
	d, _ := st.GetDevice("3588-a-1")
	if d.State != StateLeased {
		t.Errorf("重建后租约应保留（leased），实际 %s", d.State)
	}
	if d.Identity == nil || d.Identity.Model != "X1" {
		t.Errorf("身份未落库: %+v", d.Identity)
	}
}

// ready 的设备重建期间要先摘出池子，否则半路被 claim 走，结束时的回写又把
// leased 冲成 ready。
func TestSetIdentityReadyDeviceParksInResetting(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateReady})
	m := newManager(drv, st, 1)
	if _, rebuilt, err := m.SetIdentity(context.Background(), "3588-a-1", &Identity{Model: "X1", Brand: "ACME"}); err != nil || !rebuilt {
		t.Fatalf("rebuilt=%v err=%v", rebuilt, err)
	}
	if !st.sawState("3588-a-1", StateResetting) {
		t.Error("重建期间应处于 resetting，避免被 claim")
	}
	if d, _ := st.GetDevice("3588-a-1"); d.State != StateReady {
		t.Errorf("重建完应回到 ready，实际 %s", d.State)
	}
}

func TestSetIdentityUnchangedSkipsRebuild(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	id := Identity{Model: "X1", Brand: "ACME"}.Normalized()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateLeased, Identity: &id})
	m := newManager(drv, st, 1)
	if _, rebuilt, err := m.SetIdentity(context.Background(), "3588-a-1", &Identity{Model: "X1", Brand: "ACME"}); err != nil || rebuilt {
		t.Errorf("身份没变不该重建：rebuilt=%v err=%v", rebuilt, err)
	}
	if len(drv.created) != 0 {
		t.Errorf("不该重建容器: %v", drv.created)
	}
}

// 撤销覆盖 = 回到节点默认，而不是回到镜像原样。
func TestSetIdentityNilFallsBackToNodeDefault(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	own := Identity{Model: "X1", Brand: "Acme"}.Normalized()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateReady, Identity: &own})
	m := newManager(drv, st, 1)
	def := Identity{Model: "X1", Brand: "ACME"}.Normalized()
	m.DefaultIdentity = &def
	eff, rebuilt, err := m.SetIdentity(context.Background(), "3588-a-1", nil)
	if err != nil || !rebuilt {
		t.Fatalf("rebuilt=%v err=%v", rebuilt, err)
	}
	if eff == nil || eff.Model != "X1" {
		t.Errorf("撤销后应回到节点默认 X1，实际 %+v", eff)
	}
	if got := drv.identities["3588-a-1"]; got == nil || got.Model != "X1" {
		t.Errorf("重建应带节点默认身份: %+v", got)
	}
}

func TestSetIdentityRejectsInvalid(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateReady})
	m := newManager(drv, st, 1)
	if _, _, err := m.SetIdentity(context.Background(), "3588-a-1", &Identity{Model: "a/b", Brand: "x"}); err == nil {
		t.Error("非法身份应被拒")
	}
	if len(drv.created) != 0 {
		t.Error("被拒时不该重建")
	}
}

// 正在重建中的设备（creating/resetting/broken）只落库，等那次重建自己带上。
func TestSetIdentityOnResettingDeviceOnlyPersists(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateResetting})
	m := newManager(drv, st, 1)
	_, rebuilt, err := m.SetIdentity(context.Background(), "3588-a-1", &Identity{Model: "X1", Brand: "ACME"})
	if err != nil || rebuilt {
		t.Errorf("rebuilt=%v err=%v", rebuilt, err)
	}
	if d, _ := st.GetDevice("3588-a-1"); d.Identity == nil {
		t.Error("应落库")
	}
}

// Ensure 起设备时要带上节点默认身份，golden 与设备的 fingerprint 才一致。
func TestEnsureCreatesWithDefaultIdentity(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 1)
	def := Identity{Model: "X1", Brand: "ACME"}.Normalized()
	m.DefaultIdentity = &def
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := drv.identities["3588-a-1"]; got == nil || got.Model != "X1" {
		t.Errorf("Create 未带默认身份: %+v", got)
	}
}

func TestSetLocationPersistsAndApplies(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "d1", State: StateLeased})
	m := newManager(drv, st, 1)
	if err := m.SetLocation(context.Background(), "d1", "23.1291,113.2644"); err != nil {
		t.Fatal(err)
	}
	if drv.location["d1"] != "23.1291,113.2644" {
		t.Errorf("未下发到设备：%q", drv.location["d1"])
	}
	if d, _ := st.GetDevice("d1"); d.MockLocation != "23.1291,113.2644" {
		t.Errorf("未落库：%q", d.MockLocation)
	}
	// 撤销覆盖回到节点默认
	m.DefaultLocation = "31.23,121.47"
	if err := m.SetLocation(context.Background(), "d1", ""); err != nil {
		t.Fatal(err)
	}
	if drv.location["d1"] != "31.23,121.47" {
		t.Errorf("撤销后应下发节点默认，实际 %q", drv.location["d1"])
	}
}

// mock 定位不落盘，容器一重建就没了，复位后必须重放；没覆盖的用节点默认。
func TestResetReappliesLocation(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateReady, MockLocation: "23.1,113.2"})
	_ = st.UpsertDevice(&Device{ID: "3588-a-2", State: StateReady})
	m := newManager(drv, st, 2)
	m.DefaultLocation = "31.23,121.47"
	for _, id := range []string{"3588-a-1", "3588-a-2"} {
		if err := m.Reset(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if drv.location["3588-a-1"] != "23.1,113.2" {
		t.Errorf("设备自己的定位未重放: %q", drv.location["3588-a-1"])
	}
	if drv.location["3588-a-2"] != "31.23,121.47" {
		t.Errorf("节点默认定位未重放: %q", drv.location["3588-a-2"])
	}
}

// 没配定位时不该去碰设备——每次多一条 docker exec 白白拖慢复位。
func TestResetSkipsLocationWhenUnset(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateReady})
	m := newManager(drv, st, 1)
	if err := m.Reset(context.Background(), "3588-a-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := drv.location["3588-a-1"]; ok {
		t.Error("没配定位不该下发")
	}
}
