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
}

func (f *fakeDriver) Create(_ context.Context, id string, port int, overlayBase string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.createErr[id]; ok {
		return err
	}
	f.created = append(f.created, id)
	f.ports = append(f.ports, port)
	f.overlayArgs = append(f.overlayArgs, overlayBase)
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
	mu sync.Mutex
	m  map[string]*Device
}

func newMemStore() *memStore { return &memStore{m: map[string]*Device{}} }

func (s *memStore) UpsertDevice(d *Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *d
	s.m[d.ID] = &c
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
	return nil
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
