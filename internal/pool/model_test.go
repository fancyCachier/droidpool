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
