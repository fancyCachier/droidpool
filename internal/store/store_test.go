package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/fancyCachier/droidpool/internal/pool"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedReady 造 n 台 ready 设备。
func seedReady(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		d := &pool.Device{
			ID:        string(rune('a'+i-1)) + "-dev",
			Node:      "3588-a",
			Container: "redroid-x",
			ADBAddr:   "192.168.14.54:556" + string(rune('0'+i)),
			State:     pool.StateReady,
			CreatedAt: time.Now().Truncate(time.Second),
		}
		if err := s.UpsertDevice(d); err != nil {
			t.Fatalf("造设备失败: %v", err)
		}
	}
}

func newLease(id, host, wt string, ttl time.Duration) *pool.Lease {
	now := time.Now().Truncate(time.Second)
	return &pool.Lease{
		ID: id, Owner: "dev@" + host, Host: host, Worktree: wt,
		Branch: "fix/x", HeadSHA: "abc1234",
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
}

func TestDeviceRoundTrip(t *testing.T) {
	s := open(t)
	d := &pool.Device{
		ID: "d1", Node: "3588-a", Container: "redroid-1",
		ADBAddr: "192.168.14.54:5561", State: pool.StateCreating,
		CreatedAt: time.Now().Truncate(time.Second),
	}
	if err := s.UpsertDevice(d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDevice("d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ADBAddr != d.ADBAddr || got.State != pool.StateCreating || got.Node != "3588-a" {
		t.Errorf("读回的设备与写入不符: %+v", got)
	}
	if !got.CreatedAt.Equal(d.CreatedAt) {
		t.Errorf("CreatedAt 不符: %v vs %v", got.CreatedAt, d.CreatedAt)
	}
	// 零值时间要读回零值，不能变成 1970
	if !got.LastHealthy.IsZero() {
		t.Errorf("未做过健康检查时 LastHealthy 应为零值，得到 %v", got.LastHealthy)
	}

	if _, err := s.GetDevice("不存在"); !errors.Is(err, ErrNotFound) {
		t.Errorf("查不存在的设备应返回 ErrNotFound，得到 %v", err)
	}
}

func TestSetDeviceStateEnforcesStateMachine(t *testing.T) {
	s := open(t)
	seedReady(t, s, 1)
	id := "a-dev"

	if err := s.SetDeviceState(id, pool.StateLeased); err != nil {
		t.Fatalf("ready→leased 应允许: %v", err)
	}
	// leased 不能直接回 ready：必须先复位，否则上一个 agent 的构建会留给下一个
	if err := s.SetDeviceState(id, pool.StateReady); !errors.Is(err, pool.ErrBadTransition) {
		t.Errorf("leased→ready 应被拒，得到 %v", err)
	}
	got, _ := s.GetDevice(id)
	if got.State != pool.StateLeased {
		t.Errorf("非法转移后状态被改坏: %s", got.State)
	}
}

func TestClaimAllocatesDistinctDevices(t *testing.T) {
	s := open(t)
	seedReady(t, s, 2)

	l1, reused, err := s.Claim(newLease("L1", "mac", "wt-a", time.Hour), time.Now())
	if err != nil || reused {
		t.Fatalf("首次 claim 失败: err=%v reused=%v", err, reused)
	}
	l2, _, err := s.Claim(newLease("L2", "linux", "wt-b", time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if l1.DeviceID == l2.DeviceID {
		t.Errorf("两个租约拿到同一台设备 %s —— 独占被破坏", l1.DeviceID)
	}
	for _, id := range []string{l1.DeviceID, l2.DeviceID} {
		d, _ := s.GetDevice(id)
		if d.State != pool.StateLeased {
			t.Errorf("设备 %s 被租后状态应为 leased，得到 %s", id, d.State)
		}
	}
}

func TestClaimIsIdempotentPerWorktree(t *testing.T) {
	s := open(t)
	seedReady(t, s, 2)

	first, reused, err := s.Claim(newLease("L1", "mac", "wt-a", time.Hour), time.Now())
	if err != nil || reused {
		t.Fatalf("首次 claim: err=%v reused=%v", err, reused)
	}
	// 同一 worktree 再 claim（agent 重启、CLI 重复调用）应复用，不占第二台
	again, reused, err := s.Claim(newLease("L2", "mac", "wt-a", time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Error("同一 (host, worktree) 重复 claim 应标记为复用")
	}
	if again.ID != first.ID || again.DeviceID != first.DeviceID {
		t.Errorf("复用应返回原租约 %s/%s，得到 %s/%s", first.ID, first.DeviceID, again.ID, again.DeviceID)
	}
	leases, _ := s.ListLeases()
	if len(leases) != 1 {
		t.Errorf("重复 claim 不应产生第二条租约，现有 %d 条", len(leases))
	}
}

func TestClaimExhausted(t *testing.T) {
	s := open(t)
	seedReady(t, s, 1)

	if _, _, err := s.Claim(newLease("L1", "mac", "wt-a", time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Claim(newLease("L2", "mac", "wt-b", time.Hour), time.Now())
	if !errors.Is(err, ErrNoFreeDevice) {
		t.Errorf("池满时应返回 ErrNoFreeDevice，得到 %v", err)
	}
}

func TestReleaseFreesDeviceForReset(t *testing.T) {
	s := open(t)
	seedReady(t, s, 1)
	l, _, _ := s.Claim(newLease("L1", "mac", "wt-a", time.Hour), time.Now())

	devID, err := s.Release("L1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if devID != l.DeviceID {
		t.Errorf("Release 应返回设备 %s，得到 %s", l.DeviceID, devID)
	}
	d, _ := s.GetDevice(devID)
	// 归还后必须进 resetting 而不是直接 ready：脏设备不能给下一个 agent
	if d.State != pool.StateResetting {
		t.Errorf("归还后设备状态应为 resetting，得到 %s", d.State)
	}
	if _, err := s.GetLease("L1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("已归还的租约不应还能查到，得到 %v", err)
	}
	// 归还后同一 worktree 可以重新 claim（唯一索引只约束活跃行）
	if err := s.SetDeviceState(devID, pool.StateReady); err != nil {
		t.Fatal(err)
	}
	if _, reused, err := s.Claim(newLease("L2", "mac", "wt-a", time.Hour), time.Now()); err != nil || reused {
		t.Errorf("归还后重新 claim 应成功且非复用: err=%v reused=%v", err, reused)
	}

	if _, err := s.Release("不存在", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("归还不存在的租约应返回 ErrNotFound，得到 %v", err)
	}
}

func TestRenewAndExpiry(t *testing.T) {
	s := open(t)
	seedReady(t, s, 1)
	now := time.Now().Truncate(time.Second)
	s.Claim(newLease("L1", "mac", "wt-a", time.Minute), now)

	// 到期前不在过期列表里
	if got, _ := s.ExpiredLeases(now); len(got) != 0 {
		t.Errorf("未到期不应被列为过期，得到 %d 条", len(got))
	}
	// 边界：恰好到期的那一刻就应被回收，不能卡在临界点上多活一个巡检周期
	exact := now.Add(time.Minute)
	if got, _ := s.ExpiredLeases(exact); len(got) != 1 {
		t.Errorf("恰好到期时刻应被判为过期，得到 %d 条", len(got))
	}
	if got, _ := s.ExpiredLeases(exact.Add(-time.Second)); len(got) != 0 {
		t.Errorf("到期前 1s 不应被判为过期，得到 %d 条", len(got))
	}

	// 过期后应被捞出来（TTL 强制回收是唯一可靠的回收路径）
	late := now.Add(2 * time.Minute)
	got, err := s.ExpiredLeases(late)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "L1" {
		t.Fatalf("过期租约应被捞出，得到 %+v", got)
	}
	// 续约后不再过期
	if err := s.Renew("L1", late.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ExpiredLeases(late); len(got) != 0 {
		t.Errorf("续约后不应被列为过期，得到 %d 条", len(got))
	}
	if err := s.Renew("不存在", late); !errors.Is(err, ErrNotFound) {
		t.Errorf("续不存在的租约应返回 ErrNotFound，得到 %v", err)
	}
}

func TestHumanTakeover(t *testing.T) {
	s := open(t)
	seedReady(t, s, 1)
	s.Claim(newLease("L1", "mac", "wt-a", time.Hour), time.Now())

	l, _ := s.GetLease("L1")
	if l.HumanTakeover {
		t.Error("新租约不应处于接管状态")
	}
	if err := s.SetHumanTakeover("L1", true, "要人工扫码"); err != nil {
		t.Fatal(err)
	}
	l, _ = s.GetLease("L1")
	if !l.HumanTakeover || l.HumanNote != "要人工扫码" {
		t.Errorf("接管标志未生效: takeover=%v note=%q", l.HumanTakeover, l.HumanNote)
	}
	// 交还
	if err := s.SetHumanTakeover("L1", false, ""); err != nil {
		t.Fatal(err)
	}
	l, _ = s.GetLease("L1")
	if l.HumanTakeover {
		t.Error("交还后接管标志应清除")
	}
	if err := s.SetHumanTakeover("不存在", true, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("对不存在的租约设接管应返回 ErrNotFound，得到 %v", err)
	}
}

func TestListDevicesAndLeases(t *testing.T) {
	s := open(t)
	seedReady(t, s, 3)
	if ds, _ := s.ListDevices(); len(ds) != 3 {
		t.Errorf("应有 3 台设备，得到 %d", len(ds))
	}
	s.Claim(newLease("L1", "mac", "wt-a", time.Hour), time.Now())
	s.Claim(newLease("L2", "mac", "wt-b", time.Hour), time.Now())
	ls, err := s.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("应有 2 条活跃租约，得到 %d", len(ls))
	}
	s.Release("L1", time.Now())
	if ls, _ := s.ListLeases(); len(ls) != 1 {
		t.Errorf("归还一条后应剩 1 条活跃租约，得到 %d", len(ls))
	}
}

// Claim 走事务 + 唯一索引，这里验证并发下不会把同一台设备发给两个人。
func TestClaimConcurrentNoDoubleAllocation(t *testing.T) {
	s := open(t)
	const devices = 4
	seedReady(t, s, devices)

	const workers = 12
	type res struct {
		devID string
		err   error
	}
	ch := make(chan res, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func(i int) {
			<-start
			l, _, err := s.Claim(newLease(
				"L"+string(rune('A'+i)), "h"+string(rune('A'+i)), "wt"+string(rune('A'+i)), time.Hour), time.Now())
			if err != nil {
				ch <- res{err: err}
				return
			}
			ch <- res{devID: l.DeviceID}
		}(i)
	}
	close(start)

	seen := map[string]int{}
	var ok, exhausted int
	for i := 0; i < workers; i++ {
		r := <-ch
		switch {
		case r.err == nil:
			ok++
			seen[r.devID]++
		case errors.Is(r.err, ErrNoFreeDevice):
			exhausted++
		default:
			t.Errorf("非预期错误: %v", r.err)
		}
	}
	if ok != devices {
		t.Errorf("应恰好 %d 个 claim 成功，得到 %d", devices, ok)
	}
	if exhausted != workers-devices {
		t.Errorf("应有 %d 个收到池满，得到 %d", workers-devices, exhausted)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("设备 %s 被分配了 %d 次 —— 独占被破坏", id, n)
		}
	}
}

func TestTouchUpdatesLastSeen(t *testing.T) {
	s := open(t)
	seedReady(t, s, 1)
	t0 := time.Now().Truncate(time.Second)
	s.Claim(newLease("L1", "mac", "wt-a", time.Hour), t0)

	l, _ := s.GetLease("L1")
	// claim 本身算一次活动，否则新租约一上来就像僵死的
	if !l.LastSeenAt.Equal(t0.UTC()) {
		t.Errorf("claim 应把 LastSeenAt 设为 claim 时刻 %v，得到 %v", t0.UTC(), l.LastSeenAt)
	}

	t1 := t0.Add(10 * time.Minute)
	if err := s.Touch("L1", t1); err != nil {
		t.Fatal(err)
	}
	l, _ = s.GetLease("L1")
	if !l.LastSeenAt.Equal(t1.UTC()) {
		t.Errorf("Touch 后 LastSeenAt 应为 %v，得到 %v", t1.UTC(), l.LastSeenAt)
	}

	if err := s.Touch("不存在", t1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Touch 不存在的租约应返回 ErrNotFound，得到 %v", err)
	}
	// 已归还的租约不该还能被心跳续命
	s.Release("L1", t1)
	if err := s.Touch("L1", t1); !errors.Is(err, ErrNotFound) {
		t.Errorf("已归还的租约不应能 Touch，得到 %v", err)
	}
}

func TestActiveLeasesExcludesReleased(t *testing.T) {
	s := open(t)
	seedReady(t, s, 2)
	now := time.Now()
	s.Claim(newLease("L1", "mac", "wt-a", time.Hour), now)
	s.Claim(newLease("L2", "mac", "wt-b", time.Hour), now)

	ls, err := s.ActiveLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("应有 2 条活跃租约，得到 %d", len(ls))
	}
	s.Release("L1", now)
	ls, _ = s.ActiveLeases()
	if len(ls) != 1 || ls[0].ID != "L2" {
		t.Errorf("归还的租约不应出现在活跃列表，得到 %+v", ls)
	}
}
