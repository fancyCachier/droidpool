package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeDriver 记录调用，不覆写业务逻辑。
type fakeDriver struct {
	mu          sync.Mutex
	created     []string
	removed     []string
	wiped       []string
	wipeErr     map[string]error
	overlayArgs []string
	ports       []int
	createErr   map[string]error
	bootErr     map[string]error
	egress      map[string]string
	camera      map[string]string
	location    map[string]string
	identities  map[string]*Identity // Create 时收到的身份，nil 也记
	finished    []string
}

func (f *fakeDriver) SetLocation(_ context.Context, id, loc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.location == nil {
		f.location = map[string]string{}
	}
	f.location[id] = loc
	return nil
}

func (f *fakeDriver) Create(_ context.Context, id string, port int, overlayBase string, ident *Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.createErr[id]; ok {
		return err
	}
	f.created = append(f.created, id)
	f.ports = append(f.ports, port)
	f.overlayArgs = append(f.overlayArgs, overlayBase)
	if f.identities == nil {
		f.identities = map[string]*Identity{}
	}
	f.identities[id] = ident
	return nil
}

func (f *fakeDriver) FinishEgress(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, id)
	return nil
}

func (f *fakeDriver) SetCamera(_ context.Context, id, rtsp string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.camera == nil {
		f.camera = map[string]string{}
	}
	f.camera[id] = rtsp
	return nil
}

func (f *fakeDriver) SetEgress(_ context.Context, id, proxy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.egress == nil {
		f.egress = map[string]string{}
	}
	f.egress[id] = proxy
	return nil
}

func (f *fakeDriver) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeDriver) WipeData(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.wipeErr[id]; ok {
		return err
	}
	f.wiped = append(f.wiped, id)
	return nil
}

func (f *fakeDriver) WaitBoot(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.bootErr[id]; ok {
		return err
	}
	return nil
}

// memStore 内存版 DeviceStore，走真实的状态机校验。
type memStore struct {
	mu      sync.Mutex
	m       map[string]*Device
	history []string // 状态写入的先后记录，"id:state"
}

func newMemStore() *memStore { return &memStore{m: map[string]*Device{}} }

func (s *memStore) UpsertDevice(d *Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *d
	// 与真库保持一致：SQL 的 ON CONFLICT 分支不更新 egress_proxy，
	// 否则每次健康检查回写都会把用户设的出口冲掉。假实现也必须这样，
	// 不然它会掩盖真实行为。
	if old, ok := s.m[d.ID]; ok {
		if c.EgressProxy == "" {
			c.EgressProxy = old.EgressProxy
		}
		if c.CameraRTSP == "" {
			c.CameraRTSP = old.CameraRTSP
		}
		// identity 与 mock_location 同理：健康检查回写不能把它们冲掉
		if c.Identity == nil {
			c.Identity = old.Identity
		}
		if c.MockLocation == "" {
			c.MockLocation = old.MockLocation
		}
	}
	s.m[d.ID] = &c
	return nil
}

func (s *memStore) SetDeviceIdentity(id string, ident *Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[id]
	if !ok {
		return errors.New("不存在")
	}
	d.Identity = ident
	return nil
}

func (s *memStore) SetDeviceLocation(id, loc string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[id]
	if !ok {
		return errors.New("不存在")
	}
	d.MockLocation = loc
	return nil
}

func (s *memStore) SetDeviceCamera(id, rtsp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[id]
	if !ok {
		return errors.New("不存在")
	}
	d.CameraRTSP = rtsp
	return nil
}

func (s *memStore) SetDeviceEgress(id, proxy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[id]
	if !ok {
		return errors.New("不存在")
	}
	d.EgressProxy = proxy
	return nil
}

func (s *memStore) GetDevice(id string) (*Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[id]
	if !ok {
		return nil, errors.New("不存在")
	}
	c := *d
	return &c, nil
}

func (s *memStore) ListDevices() ([]*Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Device
	for _, d := range s.m {
		c := *d
		out = append(out, &c)
	}
	return out, nil
}

func (s *memStore) SetDeviceState(id string, to DeviceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.m[id]
	if !ok {
		return errors.New("不存在")
	}
	if _, err := Transition(d.State, to); err != nil {
		return err
	}
	d.State = to
	s.history = append(s.history, id+":"+string(to))
	return nil
}

// sawState 报告某台设备是否经历过某个状态（经 SetDeviceState 或 UpsertDevice 写入）。
func (s *memStore) sawState(id string, st DeviceState) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.history {
		if h == id+":"+string(st) {
			return true
		}
	}
	return false
}

func newManager(drv NodeDriver, st DeviceStore, max int) *Manager {
	return &Manager{
		NodeName: "3588-a", ADBHost: "192.168.14.54", Driver: drv, Store: st,
		MaxDevices: max, PortBase: 5560, OverlayBase: "/data/droidpool/base",
		BootTimeout: time.Second, Log: quietLogger(),
	}
}

func TestDeviceIDAndAddr(t *testing.T) {
	m := newManager(&fakeDriver{}, newMemStore(), 3)
	if got := m.DeviceID(1); got != "3588-a-1" {
		t.Errorf("DeviceID(1) = %q", got)
	}
	if got := m.Port(3); got != 5563 {
		t.Errorf("Port(3) = %d，期望 5563", got)
	}
	if got := m.ADBAddr(2); got != "192.168.14.54:5562" {
		t.Errorf("ADBAddr(2) = %q", got)
	}
}

func TestEnsureCreatesAllDevices(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 3)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(drv.created) != 3 {
		t.Fatalf("应创建 3 台，实际 %v", drv.created)
	}
	// 端口必须各不相同，否则容器起不来
	seen := map[int]bool{}
	for _, p := range drv.ports {
		if seen[p] {
			t.Errorf("端口 %d 被重复分配", p)
		}
		seen[p] = true
	}
	// 全部应落到 ready
	for i := 1; i <= 3; i++ {
		d, err := st.GetDevice(m.DeviceID(i))
		if err != nil {
			t.Fatal(err)
		}
		if d.State != StateReady {
			t.Errorf("设备 %s 状态应为 ready，得到 %s", d.ID, d.State)
		}
		if d.ADBAddr == "" {
			t.Errorf("设备 %s 缺 adb 地址", d.ID)
		}
	}
	// overlay 基底要透传下去（零拷贝复位的前提）
	for _, o := range drv.overlayArgs {
		if o != "/data/droidpool/base" {
			t.Errorf("overlayBase 未透传，得到 %q", o)
		}
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 2)
	m.Ensure(context.Background())
	first := len(drv.created)

	// 再调一次：已 ready 的设备不该被重建（否则重启守护进程会踢掉在用的设备）
	m.Ensure(context.Background())
	if len(drv.created) != first {
		t.Errorf("重复 Ensure 不应重建设备，创建次数 %d → %d", first, len(drv.created))
	}
}

func TestEnsureRebuildsBrokenOnly(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 2)
	m.Ensure(context.Background())
	drv.created = nil

	// 把 1 号标记为 broken
	d, _ := st.GetDevice("3588-a-1")
	d.State = StateBroken
	st.UpsertDevice(d)

	m.Ensure(context.Background())
	if len(drv.created) != 1 || drv.created[0] != "3588-a-1" {
		t.Errorf("只应重建 broken 的设备，得到 %v", drv.created)
	}
}

// 一台设备起不来不应阻断其余设备。
func TestEnsureContinuesAfterFailure(t *testing.T) {
	drv := &fakeDriver{createErr: map[string]error{"3588-a-2": errors.New("端口被占")}}
	st := newMemStore()
	m := newManager(drv, st, 3)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatalf("单台失败不应让 Ensure 报错: %v", err)
	}
	if len(drv.created) != 2 {
		t.Errorf("其余 2 台仍应创建成功，得到 %v", drv.created)
	}
	d, _ := st.GetDevice("3588-a-2")
	if d.State != StateBroken {
		t.Errorf("失败的设备应标记为 broken，得到 %s", d.State)
	}
}

func TestCreateBootTimeoutMarksBroken(t *testing.T) {
	drv := &fakeDriver{bootErr: map[string]error{"3588-a-1": errors.New("超时")}}
	st := newMemStore()
	m := newManager(drv, st, 1)
	m.Ensure(context.Background())

	d, _ := st.GetDevice("3588-a-1")
	if d.State != StateBroken {
		t.Errorf("boot 超时的设备应为 broken，得到 %s", d.State)
	}
}

func TestResetRecreatesContainer(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 1)
	m.Ensure(context.Background())
	drv.created = nil

	// 模拟租用后归还
	d, _ := st.GetDevice("3588-a-1")
	d.State = StateResetting
	d.HealthFails = 2
	st.UpsertDevice(d)

	if err := m.Reset(context.Background(), "3588-a-1"); err != nil {
		t.Fatal(err)
	}
	// 必须先删再建：不删的话 diff 还在，上一个 agent 的构建会留给下一个
	if len(drv.removed) != 1 || drv.removed[0] != "3588-a-1" {
		t.Errorf("复位应先删容器，得到 %v", drv.removed)
	}
	// 关键：数据目录必须真的被清空。只删容器不清数据 = 假复位。
	if len(drv.wiped) != 1 || drv.wiped[0] != "3588-a-1" {
		t.Errorf("复位必须清空数据目录，得到 %v", drv.wiped)
	}
	if len(drv.created) != 1 {
		t.Errorf("复位应重建容器，得到 %v", drv.created)
	}
	if drv.ports[len(drv.ports)-1] != 5561 {
		t.Errorf("复位后端口应保持 5561，得到 %d", drv.ports[len(drv.ports)-1])
	}
	d, _ = st.GetDevice("3588-a-1")
	if d.State != StateReady {
		t.Errorf("复位后应回到 ready，得到 %s", d.State)
	}
	if d.HealthFails != 0 {
		t.Errorf("复位后失败计数应清零，得到 %d", d.HealthFails)
	}
}

func TestResetUnknownDevice(t *testing.T) {
	m := newManager(&fakeDriver{}, newMemStore(), 1)
	if err := m.Reset(context.Background(), "不存在的设备"); err == nil {
		t.Error("复位不存在的设备应报错")
	}
}

func TestIndexOf(t *testing.T) {
	m := newManager(&fakeDriver{}, newMemStore(), 1)
	if i, err := m.indexOf("3588-a-7"); err != nil || i != 7 {
		t.Errorf("indexOf = %d, %v，期望 7, nil", i, err)
	}
	for _, bad := range []string{"别的节点-1", "3588-a-", "3588-a-0", "垃圾"} {
		if _, err := m.indexOf(bad); err == nil {
			t.Errorf("indexOf(%q) 应报错", bad)
		}
	}
}

// 数据清不干净时宁可把设备标 broken，也不能把「看起来干净」的设备放回池子——
// 那正是本项目要消灭的失效：下一个 agent 上去看到的是别人的构建。
func TestResetMarksBrokenWhenWipeFails(t *testing.T) {
	drv := &fakeDriver{wipeErr: map[string]error{"3588-a-1": errors.New("目录被占用")}}
	st := newMemStore()
	m := newManager(drv, st, 1)
	m.Ensure(context.Background())
	drv.created = nil

	d, _ := st.GetDevice("3588-a-1")
	d.State = StateResetting
	st.UpsertDevice(d)

	err := m.Reset(context.Background(), "3588-a-1")
	if err == nil {
		t.Fatal("清空失败时 Reset 应报错")
	}
	d, _ = st.GetDevice("3588-a-1")
	if d.State != StateBroken {
		t.Errorf("清空失败的设备应标 broken，得到 %s", d.State)
	}
	// 绝不能重建成一台「可用」的脏设备
	if len(drv.created) != 0 {
		t.Errorf("清空失败后不应继续重建容器，得到 %v", drv.created)
	}
}

// 库与节点脱节是守护进程重启后的常态。两种脱节各有后果：
//   - 库说 ready 但节点没容器：会一直把死设备分给人
//   - 库说 resetting 但没人在复位：永远卡住（首次部署真踩到了）
func TestReconcileStoreMarksMissingContainersBroken(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 3)
	m.Ensure(context.Background())
	// 模拟节点上 2 号容器没了（有人 docker rm、或宿主重启）
	running := map[string]bool{"droidpool-3588-a-1": true, "droidpool-3588-a-3": true}
	m.ReconcileStore(context.Background(), running)

	d2, _ := st.GetDevice("3588-a-2")
	if d2.State != StateBroken {
		t.Errorf("节点无容器的设备应标 broken，得到 %s", d2.State)
	}
	for _, id := range []string{"3588-a-1", "3588-a-3"} {
		d, _ := st.GetDevice(id)
		if d.State != StateReady {
			t.Errorf("%s 容器还在，不应被动，得到 %s", id, d.State)
		}
	}
}

func TestReconcileStoreResetsStuckIntermediateStates(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 2)
	m.Ensure(context.Background())
	drv.created, drv.wiped = nil, nil
	// 上一版 release 后无人复位，卡在 resetting；另一台卡在 creating（进程中途重启）
	d1, _ := st.GetDevice("3588-a-1")
	d1.State = StateResetting
	st.UpsertDevice(d1)
	d2, _ := st.GetDevice("3588-a-2")
	d2.State = StateCreating
	st.UpsertDevice(d2)

	running := map[string]bool{"droidpool-3588-a-1": true, "droidpool-3588-a-2": true}
	m.ReconcileStore(context.Background(), running)

	for _, id := range []string{"3588-a-1", "3588-a-2"} {
		d, _ := st.GetDevice(id)
		if d.State != StateReady {
			t.Errorf("%s 卡在中间态应被重新复位到 ready，得到 %s", id, d.State)
		}
	}
	if len(drv.wiped) != 2 {
		t.Errorf("两台都应真正复位（清数据），得到 %v", drv.wiped)
	}
}

// leased 也一样：容器没了就是没了，持有它的 agent 下次 status 看到 broken 比对着死设备干等强。
func TestReconcileStoreHandlesLeasedWithoutContainer(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 1)
	m.Ensure(context.Background())
	d, _ := st.GetDevice("3588-a-1")
	d.State = StateLeased
	st.UpsertDevice(d)

	m.ReconcileStore(context.Background(), map[string]bool{})
	d, _ = st.GetDevice("3588-a-1")
	if d.State != StateBroken {
		t.Errorf("leased 但节点无容器应标 broken，得到 %s", d.State)
	}
}

// 设备重建后容器是全新的，库里记着的出口必须被重放，否则「以为走代理其实直连」。
func TestCreateReplaysStoredEgress(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	// broken 才会被 Ensure 重建，正好模拟「设备重建后出口要回来」
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateBroken, EgressProxy: "socks5://up:1080"})
	m := newManager(drv, st, 1)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(drv.finished) == 0 {
		t.Error("未调用 FinishEgress，流量不会进隧道")
	}
	if got := drv.egress["3588-a-1"]; got != "socks5://up:1080" {
		t.Errorf("未重放库里的出口设置，实际 %q", got)
	}
}

func TestCreateSkipsEgressWhenUnset(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 1)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := drv.egress["3588-a-1"]; ok {
		t.Error("没设过出口就不该调 SetEgress")
	}
}

func TestSetEgressPersistsAndApplies(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "d1"})
	m := newManager(drv, st, 1)
	if err := m.SetEgress(context.Background(), "d1", "socks5://x:1080"); err != nil {
		t.Fatal(err)
	}
	if drv.egress["d1"] != "socks5://x:1080" {
		t.Errorf("未下发到节点：%q", drv.egress["d1"])
	}
	d, _ := st.GetDevice("d1")
	if d.EgressProxy != "socks5://x:1080" {
		t.Errorf("未落库：%q", d.EgressProxy)
	}
	// 设备不存在时不该去动节点
	drv.egress = map[string]string{}
	if err := m.SetEgress(context.Background(), "nope", "socks5://y:1080"); err == nil {
		t.Error("对不存在的设备应当报错")
	}
	if len(drv.egress) != 0 {
		t.Errorf("设备不存在却动了节点：%v", drv.egress)
	}
}

// 复位走的是 Reset 而不是 createOne，出口同样要重新落上去。
// 漏了这条的后果很隐蔽：设备照常可用，只是悄悄变成直连——
// 2026-09-07 生产上开 egress 后第一台重建的设备就是这样，边车起来了但规则没加。
func TestResetReappliesEgress(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateReady, EgressProxy: "socks5://up:1080"})
	m := newManager(drv, st, 1)
	if err := m.Reset(context.Background(), "3588-a-1"); err != nil {
		t.Fatal(err)
	}
	if len(drv.finished) == 0 {
		t.Error("Reset 后未调用 FinishEgress，流量不会进隧道")
	}
	if got := drv.egress["3588-a-1"]; got != "socks5://up:1080" {
		t.Errorf("Reset 后未重放出口设置，实际 %q", got)
	}
}

// 复位后摄像头也要重放：推流容器是对着某个 /dev/videoN 灌的，设备重建了
// 而流没跟上，画面就对着一个没人读的节点空转。
func TestResetReappliesCamera(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "3588-a-1", State: StateReady, CameraRTSP: "rtsp://cam/live"})
	m := newManager(drv, st, 1)
	if err := m.Reset(context.Background(), "3588-a-1"); err != nil {
		t.Fatal(err)
	}
	if got := drv.camera["3588-a-1"]; got != "rtsp://cam/live" {
		t.Errorf("Reset 后未重放画面源，实际 %q", got)
	}
}

func TestSetCameraPersistsAndApplies(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	_ = st.UpsertDevice(&Device{ID: "d1"})
	m := newManager(drv, st, 1)
	if err := m.SetCamera(context.Background(), "d1", "rtsp://x/live"); err != nil {
		t.Fatal(err)
	}
	if drv.camera["d1"] != "rtsp://x/live" {
		t.Errorf("未下发到节点：%q", drv.camera["d1"])
	}
	if d, _ := st.GetDevice("d1"); d.CameraRTSP != "rtsp://x/live" {
		t.Errorf("未落库：%q", d.CameraRTSP)
	}
}

// 没设过摄像头的设备不该被起推流容器——转码一路吃掉一个核的七成。
func TestCreateSkipsCameraWhenUnset(t *testing.T) {
	drv, st := &fakeDriver{}, newMemStore()
	m := newManager(drv, st, 1)
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := drv.camera["3588-a-1"]; ok {
		t.Error("没设过画面源就不该起推流容器")
	}
}
