package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeLeaseStore 只实现回收器需要的两个方法，不覆写业务逻辑。
type fakeLeaseStore struct {
	mu       sync.Mutex
	active   []*Lease
	released []string
	failOn   map[string]error
}

func (f *fakeLeaseStore) ActiveLeases() ([]*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, nil
}

func (f *fakeLeaseStore) Release(id string, _ time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[id]; ok {
		return "", err
	}
	f.released = append(f.released, id)
	return "dev-" + id, nil
}

type fakeResetter struct {
	mu     sync.Mutex
	reset  []string
	failOn map[string]error
}

func (f *fakeResetter) Reset(_ context.Context, deviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[deviceID]; ok {
		return err
	}
	f.reset = append(f.reset, deviceID)
	return nil
}

// expiredLease 造一条 TTL 已过期的租约（回收器应当收走它）。
func expiredLease(id string) *Lease {
	past := time.Now().Add(-time.Hour)
	return &Lease{ID: id, Owner: "dev@mac", Worktree: "wt-" + id,
		CreatedAt: past, ExpiresAt: past.Add(time.Minute), LastSeenAt: past}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReapOnceReleasesAndResets(t *testing.T) {
	fs := &fakeLeaseStore{active: []*Lease{
		expiredLease("L1"), expiredLease("L2"),
	}}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}

	n, err := r.ReapOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("应回收 2 条，得到 %d", n)
	}
	if len(fs.released) != 2 {
		t.Errorf("两条租约都应被 release，得到 %v", fs.released)
	}
	// 回收后必须复位设备，否则脏设备直接回池给下一个 agent
	if len(fr.reset) != 2 || fr.reset[0] != "dev-L1" {
		t.Errorf("两台设备都应被复位，得到 %v", fr.reset)
	}
}

func TestReapOnceNothingExpired(t *testing.T) {
	fs := &fakeLeaseStore{}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}
	n, err := r.ReapOnce(context.Background())
	if err != nil || n != 0 {
		t.Errorf("无过期租约时应回收 0 条无错，得到 n=%d err=%v", n, err)
	}
	if len(fr.reset) != 0 {
		t.Errorf("不应触发任何复位，得到 %v", fr.reset)
	}
}

// 一条租约回收失败不能让整轮停摆——否则一台卡住的设备会拖垮整个池子。
func TestReapOnceContinuesAfterReleaseFailure(t *testing.T) {
	boom := errors.New("库锁住了")
	fs := &fakeLeaseStore{
		active: []*Lease{expiredLease("L1"), expiredLease("L2"), expiredLease("L3")},
		failOn: map[string]error{"L2": boom},
	}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}

	n, err := r.ReapOnce(context.Background())
	if err != nil {
		t.Fatalf("单条失败不应让整轮报错: %v", err)
	}
	if n != 2 {
		t.Errorf("应成功回收 2 条（跳过失败的 L2），得到 %d", n)
	}
	if len(fs.released) != 2 || fs.released[0] != "L1" || fs.released[1] != "L3" {
		t.Errorf("L1 与 L3 应被回收，得到 %v", fs.released)
	}
}

// 复位失败同样不能中断后续回收。
func TestReapOnceContinuesAfterResetFailure(t *testing.T) {
	fs := &fakeLeaseStore{active: []*Lease{expiredLease("L1"), expiredLease("L2")}}
	fr := &fakeResetter{failOn: map[string]error{"dev-L1": errors.New("容器起不来")}}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}

	n, err := r.ReapOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("复位失败不影响回收计数，应为 2，得到 %d", n)
	}
	if len(fr.reset) != 1 || fr.reset[0] != "dev-L2" {
		t.Errorf("L2 的设备仍应被复位，得到 %v", fr.reset)
	}
}

func TestReapOnceWithoutResetter(t *testing.T) {
	fs := &fakeLeaseStore{active: []*Lease{expiredLease("L1")}}
	r := &Reaper{Store: fs, Log: quietLogger()} // Resetter 为 nil
	n, err := r.ReapOnce(context.Background())
	if err != nil || n != 1 {
		t.Errorf("无 Resetter 时也应正常回收，得到 n=%d err=%v", n, err)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	fs := &fakeLeaseStore{}
	r := &Reaper{Store: fs, Interval: time.Millisecond, Log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 应立即退出")
	}
}

func TestRunReapsOnTick(t *testing.T) {
	fs := &fakeLeaseStore{active: []*Lease{expiredLease("L1")}}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Interval: 5 * time.Millisecond, Log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		fs.mu.Lock()
		n := len(fs.released)
		fs.mu.Unlock()
		if n > 0 {
			return // 定时器确实触发了回收
		}
		select {
		case <-deadline:
			t.Fatal("Run 在 2s 内没有触发任何回收")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// watchdog 的核心场景：agent 僵死（进程还在但不再干活），TTL 远未到期，
// 空闲闸必须把设备收回来，否则机器被白占到 TTL 结束。
func TestReaperIdleGateCatchesZombieAgent(t *testing.T) {
	now := time.Now()
	zombie := &Lease{
		ID: "Z1", Owner: "dev@mac", Worktree: "wt-zombie",
		CreatedAt:  now.Add(-90 * time.Minute),
		ExpiresAt:  now.Add(2 * time.Hour), // TTL 还剩 2 小时
		LastSeenAt: now.Add(-45 * time.Minute),
	}
	busy := &Lease{
		ID: "B1", Owner: "dev@linux", Worktree: "wt-busy",
		CreatedAt:  now.Add(-90 * time.Minute),
		ExpiresAt:  now.Add(2 * time.Hour),
		LastSeenAt: now.Add(-1 * time.Minute), // 一分钟前还在干活
	}
	fs := &fakeLeaseStore{active: []*Lease{zombie, busy}}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, IdleTimeout: 30 * time.Minute, Log: quietLogger()}

	n, err := r.ReapOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应只回收僵死的那条，得到 %d", n)
	}
	if len(fs.released) != 1 || fs.released[0] != "Z1" {
		t.Errorf("应回收 Z1（僵死），保留 B1（在忙），得到 %v", fs.released)
	}
	if len(fr.reset) != 1 || fr.reset[0] != "dev-Z1" {
		t.Errorf("僵死租约的设备应被复位，得到 %v", fr.reset)
	}
}

// 空闲闸关闭时，僵死 agent 只能等 TTL——这正是改造前的行为，用它守住回归。
func TestReaperWithoutIdleGateKeepsZombie(t *testing.T) {
	now := time.Now()
	zombie := &Lease{
		ID: "Z1", CreatedAt: now.Add(-90 * time.Minute),
		ExpiresAt: now.Add(2 * time.Hour), LastSeenAt: now.Add(-89 * time.Minute),
	}
	fs := &fakeLeaseStore{active: []*Lease{zombie}}
	r := &Reaper{Store: fs, Log: quietLogger()} // IdleTimeout 为 0

	n, _ := r.ReapOnce(context.Background())
	if n != 0 {
		t.Errorf("空闲闸关闭时不应回收未到期的租约，得到 %d", n)
	}
}

// 硬上限兜底：agent 一直心跳、TTL 也一直被续，仍不能永久占着机器。
func TestReaperMaxLifetimeGate(t *testing.T) {
	now := time.Now()
	clingy := &Lease{
		ID: "C1", CreatedAt: now.Add(-25 * time.Hour),
		ExpiresAt: now.Add(4 * time.Hour), LastSeenAt: now, // 心跳很新鲜
	}
	fs := &fakeLeaseStore{active: []*Lease{clingy}}
	r := &Reaper{Store: fs, IdleTimeout: 30 * time.Minute, MaxLifetime: 24 * time.Hour, Log: quietLogger()}

	n, _ := r.ReapOnce(context.Background())
	if n != 1 || len(fs.released) != 1 {
		t.Errorf("持有超过 24h 应被硬上限回收，得到 n=%d released=%v", n, fs.released)
	}
}

// watchdog 收走设备是 agent 看不到的变更（它已经僵死了），必须能推给设备墙。
func TestReapOnceNotifiesWithReason(t *testing.T) {
	now := time.Now()
	fs := &fakeLeaseStore{active: []*Lease{
		expiredLease("L1"),
		{ID: "Z1", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), LastSeenAt: now.Add(-time.Hour)},
	}}
	type note struct {
		lease, dev string
		reason     ReapReason
	}
	var got []note
	r := &Reaper{
		Store: fs, IdleTimeout: 30 * time.Minute, Log: quietLogger(),
		OnReap: func(l, d string, reason ReapReason) { got = append(got, note{l, d, reason}) },
	}
	if _, err := r.ReapOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("两条都该通知，得到 %+v", got)
	}
	// 原因要能区分：TTL 到期是正常收尾，空闲超时说明 agent 僵死了
	if got[0].reason != ReapExpired {
		t.Errorf("L1 应报 ttl_expired，得到 %q", got[0].reason)
	}
	if got[1].reason != ReapIdle {
		t.Errorf("Z1 应报 idle_timeout，得到 %q", got[1].reason)
	}
	if got[0].dev != "dev-L1" {
		t.Errorf("应带上设备 id，得到 %q", got[0].dev)
	}
}
