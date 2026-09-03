package pool

import (
	"errors"
	"testing"
	"time"
)

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to DeviceState
		want     bool
	}{
		// 正常生命周期
		{StateCreating, StateReady, true},
		{StateReady, StateLeased, true},
		{StateLeased, StateResetting, true},
		{StateResetting, StateReady, true},
		// 健康检查失败可从任一活动态转 broken
		{StateCreating, StateBroken, true},
		{StateReady, StateBroken, true},
		{StateLeased, StateBroken, true},
		{StateResetting, StateBroken, true},
		// broken 只能重建
		{StateBroken, StateCreating, true},
		{StateBroken, StateReady, false},
		{StateBroken, StateLeased, false},
		// 不能跳过复位直接再租：租完必须洗干净
		{StateLeased, StateReady, false},
		// 不能从 ready 直接进 creating
		{StateReady, StateCreating, false},
		// 未知状态
		{DeviceState("bogus"), StateReady, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%s, %s) = %v, 期望 %v", c.from, c.to, got, c.want)
		}
	}
}

func TestTransition(t *testing.T) {
	got, err := Transition(StateReady, StateLeased)
	if err != nil {
		t.Fatalf("合法转移不应报错: %v", err)
	}
	if got != StateLeased {
		t.Errorf("得到 %s，期望 %s", got, StateLeased)
	}

	got, err = Transition(StateLeased, StateReady)
	if !errors.Is(err, ErrBadTransition) {
		t.Errorf("非法转移应返回 ErrBadTransition，得到 %v", err)
	}
	if got != StateLeased {
		t.Errorf("非法转移应保持原状态 %s，得到 %s", StateLeased, got)
	}
}

func TestLeaseExpired(t *testing.T) {
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	l := &Lease{ExpiresAt: now.Add(time.Hour)}

	if l.Expired(now) {
		t.Error("未到期不应判为过期")
	}
	if !l.Expired(now.Add(2 * time.Hour)) {
		t.Error("已过期应判为过期")
	}
	// 边界：正好到期算过期，避免卡在临界点上反复续命
	if !l.Expired(l.ExpiresAt) {
		t.Error("恰好到期时刻应判为过期")
	}
	if l.Expired(l.ExpiresAt.Add(-time.Nanosecond)) {
		t.Error("到期前 1ns 不应判为过期")
	}
}

func TestLeaseIdempotencyKey(t *testing.T) {
	a := &Lease{Host: "mac-1", Worktree: "fix-3482"}
	b := &Lease{Host: "mac-1", Worktree: "fix-3482", Branch: "别的分支"}
	c := &Lease{Host: "linux-2", Worktree: "fix-3482"}
	d := &Lease{Host: "mac-1", Worktree: "feat-x"}

	if a.IdempotencyKey() != b.IdempotencyKey() {
		t.Error("同 host 同 worktree 应视为同一个租约，与分支无关")
	}
	if a.IdempotencyKey() == c.IdempotencyKey() {
		t.Error("不同 host 不应撞键")
	}
	if a.IdempotencyKey() == d.IdempotencyKey() {
		t.Error("不同 worktree 不应撞键")
	}
	// 键必须无歧义：字段内容不能通过塞入分隔符伪造出另一组 (host, worktree)
	e := &Lease{Host: "a", Worktree: "b|c"}
	f := &Lease{Host: "a|b", Worktree: "c"}
	if e.IdempotencyKey() == f.IdempotencyKey() {
		t.Error("分隔符可被绕过，键设计有歧义")
	}
}

func TestShouldReapThreeGates(t *testing.T) {
	base := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	// 一条正常租约：刚建、4h TTL、刚刚活动过
	fresh := func() *Lease {
		return &Lease{CreatedAt: base, ExpiresAt: base.Add(4 * time.Hour), LastSeenAt: base}
	}
	const idle = 30 * time.Minute
	const maxLife = 24 * time.Hour

	t.Run("健康租约不回收", func(t *testing.T) {
		l := fresh()
		l.LastSeenAt = base.Add(10 * time.Minute) // 10 分钟前还在活动
		if r := l.ShouldReap(base.Add(20*time.Minute), idle, maxLife); r != "" {
			t.Errorf("活跃租约不应回收，得到 %q", r)
		}
	})

	t.Run("空闲超时抓僵死", func(t *testing.T) {
		l := fresh() // 最后活动在 base，TTL 还有 4h
		now := base.Add(31 * time.Minute)
		if r := l.ShouldReap(now, idle, maxLife); r != ReapIdle {
			t.Errorf("久未活动应判 idle_timeout，得到 %q", r)
		}
		// 这正是 watchdog 的价值：TTL 还远没到，但设备已经被白占了半小时
		if l.Expired(now) {
			t.Fatal("测试前提错了：此时 TTL 不应到期")
		}
	})

	t.Run("空闲边界", func(t *testing.T) {
		l := fresh()
		if r := l.ShouldReap(base.Add(idle), idle, maxLife); r != ReapIdle {
			t.Errorf("恰好到空闲阈值应回收，得到 %q", r)
		}
		if r := l.ShouldReap(base.Add(idle-time.Second), idle, maxLife); r != "" {
			t.Errorf("差 1s 到阈值不应回收，得到 %q", r)
		}
	})

	t.Run("TTL 到期", func(t *testing.T) {
		l := fresh()
		now := base.Add(4 * time.Hour)
		l.LastSeenAt = now // 一直在活动，但约定时间到了
		if r := l.ShouldReap(now, idle, maxLife); r != ReapExpired {
			t.Errorf("TTL 到期应回收，得到 %q", r)
		}
	})

	t.Run("生命周期上限兜底", func(t *testing.T) {
		// 恶劣情形：agent 卡在循环里一直心跳、TTL 也被不断续，两道闸都拦不住
		l := &Lease{CreatedAt: base, ExpiresAt: base.Add(100 * time.Hour), LastSeenAt: base.Add(25 * time.Hour)}
		now := base.Add(25 * time.Hour)
		if r := l.ShouldReap(now, idle, maxLife); r != ReapTooLong {
			t.Errorf("超过生命周期上限应回收，得到 %q", r)
		}
	})

	t.Run("人工接管豁免空闲闸", func(t *testing.T) {
		// 人在设备墙上操作时本来就没有 agent 活动，不能因此把设备收走
		l := fresh()
		l.HumanTakeover = true
		if r := l.ShouldReap(base.Add(2*time.Hour), idle, maxLife); r != "" {
			t.Errorf("人工接管中不应因空闲被回收，得到 %q", r)
		}
		// 但 TTL 到期仍然回收，接管不是免死金牌
		l2 := fresh()
		l2.HumanTakeover = true
		if r := l2.ShouldReap(base.Add(5*time.Hour), idle, maxLife); r != ReapExpired {
			t.Errorf("接管中 TTL 到期仍应回收，得到 %q", r)
		}
	})

	t.Run("闸可单独关闭", func(t *testing.T) {
		l := fresh()
		// idleTimeout=0 关掉空闲闸，久未活动也不收
		if r := l.ShouldReap(base.Add(3*time.Hour), 0, maxLife); r != "" {
			t.Errorf("空闲闸关闭时不应因空闲回收，得到 %q", r)
		}
		// maxLifetime=0 关掉硬上限
		old := &Lease{CreatedAt: base, ExpiresAt: base.Add(1000 * time.Hour), LastSeenAt: base.Add(99 * time.Hour)}
		if r := old.ShouldReap(base.Add(99*time.Hour), 0, 0); r != "" {
			t.Errorf("两闸皆关时不应回收，得到 %q", r)
		}
	})

	t.Run("从未活动过的租约不误判", func(t *testing.T) {
		// LastSeenAt 为零值（老数据迁移上来）时不该被空闲闸秒杀
		l := &Lease{CreatedAt: base, ExpiresAt: base.Add(4 * time.Hour)}
		if r := l.ShouldReap(base.Add(time.Hour), idle, maxLife); r != "" {
			t.Errorf("LastSeenAt 为零值不应触发空闲回收，得到 %q", r)
		}
	})
}
