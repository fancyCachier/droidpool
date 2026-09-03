package pool

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeProber struct {
	mu    sync.Mutex
	alive map[string]bool
}

func (f *fakeProber) Alive(_ context.Context, serial string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive[serial]
}

func seedDevices(t *testing.T, st *memStore, states map[string]DeviceState) {
	t.Helper()
	for id, s := range states {
		st.UpsertDevice(&Device{ID: id, Node: "n", Container: "c-" + id, ADBAddr: id + ":5555", State: s, CreatedAt: time.Now()})
	}
}

// 单次失败可能只是 adb 抖动，必须连续三次才判死；否则网络一颤池子就把好设备全标 broken。
func TestHealthNeedsConsecutiveFailures(t *testing.T) {
	st := newMemStore()
	seedDevices(t, st, map[string]DeviceState{"d1": StateReady})
	pr := &fakeProber{alive: map[string]bool{}}
	fr := &fakeResetter{}
	h := &HealthChecker{Store: st, Prober: pr, Resetter: fr, Log: quietLogger()}

	for i := 1; i <= 2; i++ {
		if broken := h.CheckOnce(context.Background()); len(broken) != 0 {
			t.Fatalf("第 %d 次失败就判死，太激进: %v", i, broken)
		}
		d, _ := st.GetDevice("d1")
		if d.HealthFails != i {
			t.Errorf("第 %d 次后 HealthFails = %d", i, d.HealthFails)
		}
		if d.State != StateReady {
			t.Errorf("未到阈值不应改状态，得到 %s", d.State)
		}
	}
	broken := h.CheckOnce(context.Background())
	if len(broken) != 1 || broken[0] != "d1" {
		t.Fatalf("第 3 次失败应判死，得到 %v", broken)
	}
	d, _ := st.GetDevice("d1")
	if d.State != StateBroken {
		t.Errorf("应标 broken，得到 %s", d.State)
	}
	// 判死后要触发重建（异步），等一下
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fr.mu.Lock()
		n := len(fr.reset)
		fr.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("判死后应触发 Reset 重建")
}

// 中途恢复要清零计数，否则「偶尔抖一下」累积到三次也会被误杀。
func TestHealthRecoveryResetsCounter(t *testing.T) {
	st := newMemStore()
	seedDevices(t, st, map[string]DeviceState{"d1": StateReady})
	pr := &fakeProber{alive: map[string]bool{}}
	h := &HealthChecker{Store: st, Prober: pr, Log: quietLogger()}

	h.CheckOnce(context.Background()) // 失败 1
	h.CheckOnce(context.Background()) // 失败 2
	pr.mu.Lock()
	pr.alive["d1:5555"] = true
	pr.mu.Unlock()
	h.CheckOnce(context.Background()) // 恢复
	d, _ := st.GetDevice("d1")
	if d.HealthFails != 0 {
		t.Errorf("恢复后计数应清零，得到 %d", d.HealthFails)
	}
	if d.LastHealthy.IsZero() {
		t.Error("恢复后应记录 LastHealthy")
	}
	pr.mu.Lock()
	pr.alive["d1:5555"] = false
	pr.mu.Unlock()
	if broken := h.CheckOnce(context.Background()); len(broken) != 0 {
		t.Errorf("恢复后再失败 1 次不应判死（计数已清零），得到 %v", broken)
	}
}

// 只探 ready/leased：creating/resetting 本来就没准备好，broken 已经死了，
// 探它们只会制造噪音和误判。
func TestHealthSkipsNonActiveStates(t *testing.T) {
	st := newMemStore()
	seedDevices(t, st, map[string]DeviceState{
		"creating": StateCreating, "resetting": StateResetting, "broken": StateBroken,
		"ready": StateReady, "leased": StateLeased,
	})
	pr := &fakeProber{alive: map[string]bool{}} // 全部不应答
	h := &HealthChecker{Store: st, Prober: pr, Threshold: 1, Log: quietLogger()}
	broken := h.CheckOnce(context.Background())
	got := map[string]bool{}
	for _, b := range broken {
		got[b] = true
	}
	if !got["ready"] || !got["leased"] {
		t.Errorf("ready 与 leased 不应答应被判死，得到 %v", broken)
	}
	if got["creating"] || got["resetting"] || got["broken"] {
		t.Errorf("非活动态不应被探测/判死，得到 %v", broken)
	}
	for _, id := range []string{"creating", "resetting"} {
		d, _ := st.GetDevice(id)
		if d.HealthFails != 0 {
			t.Errorf("%s 不应累积失败计数，得到 %d", id, d.HealthFails)
		}
	}
}

// 一台设备探测失败不能让整轮中断，其余设备照常检查。
func TestHealthContinuesPastFailures(t *testing.T) {
	st := newMemStore()
	seedDevices(t, st, map[string]DeviceState{"d1": StateReady, "d2": StateReady, "d3": StateReady})
	pr := &fakeProber{alive: map[string]bool{"d2:5555": true}} // 只有 d2 活着
	h := &HealthChecker{Store: st, Prober: pr, Threshold: 1, Log: quietLogger()}
	broken := h.CheckOnce(context.Background())
	if len(broken) != 2 {
		t.Errorf("d1 与 d3 都应被判死，得到 %v", broken)
	}
	d2, _ := st.GetDevice("d2")
	if d2.State != StateReady || d2.HealthFails != 0 {
		t.Errorf("d2 应保持健康，得到 state=%s fails=%d", d2.State, d2.HealthFails)
	}
}

func TestHealthRunStopsOnCancel(t *testing.T) {
	st := newMemStore()
	h := &HealthChecker{Store: st, Prober: &fakeProber{}, Interval: time.Millisecond, Log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 应退出")
	}
}
