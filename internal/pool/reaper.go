package pool

import (
	"context"
	"log/slog"
	"time"
)

// LeaseStore 是回收器需要的存储能力子集。
type LeaseStore interface {
	ActiveLeases() ([]*Lease, error)
	Release(id string, now time.Time) (deviceID string, err error)
}

// Resetter 把设备复位到干净状态（删 diff → 重建容器 → 等 boot → 置 ready）。
type Resetter interface {
	Reset(ctx context.Context, deviceID string) error
}

// Reaper 是设备池的 watchdog：定期把该收的租约收回来，防止 agent 僵死后
// 一直占着机器。
//
// 强制回收不问 agent —— agent 异常退出、卡死、被 kill 都是常态，不能指望它
// 自己调 release。三道闸各管一种失效（见 Lease.ShouldReap）：
// TTL 到期、空闲超时（抓僵死的主力）、生命周期上限（兜底）。
// 回收后设备进 resetting，由 Resetter 洗干净再放回池子。
type Reaper struct {
	Store    LeaseStore
	Resetter Resetter
	Interval time.Duration
	// IdleTimeout 多久没有 agent 活动就判定僵死并回收。0 表示不启用。
	IdleTimeout time.Duration
	// MaxLifetime 单个租约允许持有的总时长上限，防止「一直心跳但其实卡死」。0 表示不启用。
	MaxLifetime time.Duration
	Now         func() time.Time
	Log         *slog.Logger
	// OnReap 回收成功后回调，用于把「设备被 watchdog 收走了」推给设备墙。
	// 这是 agent 看不到的变更：它已经僵死了，不会自己来问。
	OnReap func(leaseID, deviceID string, reason ReapReason)
}

func (r *Reaper) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reaper) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// ReapOnce 回收一轮，返回被回收的租约数。
// 单个租约处理失败不影响其余租约——一台设备卡住不该让整个池子停摆。
func (r *Reaper) ReapOnce(ctx context.Context) (int, error) {
	active, err := r.Store.ActiveLeases()
	if err != nil {
		return 0, err
	}
	now := r.now()
	n := 0
	for _, l := range active {
		reason := l.ShouldReap(now, r.IdleTimeout, r.MaxLifetime)
		if reason == "" {
			continue
		}
		devID, err := r.Store.Release(l.ID, now)
		if err != nil {
			r.log().Error("回收租约失败", "lease", l.ID, "reason", reason, "err", err)
			continue
		}
		n++
		r.log().Info("watchdog 回收租约", "lease", l.ID, "reason", reason,
			"device", devID, "owner", l.Owner, "worktree", l.Worktree,
			"idle", now.Sub(l.LastSeenAt).Round(time.Second),
			"held", now.Sub(l.CreatedAt).Round(time.Second))
		if r.OnReap != nil {
			r.OnReap(l.ID, devID, reason)
		}
		if r.Resetter != nil {
			if err := r.Resetter.Reset(ctx, devID); err != nil {
				r.log().Error("复位设备失败", "device", devID, "err", err)
			}
		}
	}
	return n, nil
}

// Run 阻塞循环，直到 ctx 取消。
func (r *Reaper) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.ReapOnce(ctx); err != nil {
				r.log().Error("回收轮次失败", "err", err)
			}
		}
	}
}
