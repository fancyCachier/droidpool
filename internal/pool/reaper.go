package pool

import (
	"context"
	"log/slog"
	"time"
)

// LeaseStore 是回收器需要的存储能力子集。
type LeaseStore interface {
	ExpiredLeases(now time.Time) ([]*Lease, error)
	Release(id string, now time.Time) (deviceID string, err error)
}

// Resetter 把设备复位到干净状态（删 diff → 重建容器 → 等 boot → 置 ready）。
type Resetter interface {
	Reset(ctx context.Context, deviceID string) error
}

// Reaper 定期回收过期租约。
//
// TTL 到期强制回收是唯一可靠的回收路径：agent 异常退出是常态，不能指望它
// 自己调 release。回收后设备进 resetting，由 Resetter 洗干净再放回池子。
type Reaper struct {
	Store    LeaseStore
	Resetter Resetter
	Interval time.Duration
	Now      func() time.Time
	Log      *slog.Logger
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
	expired, err := r.Store.ExpiredLeases(r.now())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range expired {
		devID, err := r.Store.Release(l.ID, r.now())
		if err != nil {
			r.log().Error("回收过期租约失败", "lease", l.ID, "err", err)
			continue
		}
		n++
		r.log().Info("回收过期租约",
			"lease", l.ID, "device", devID, "owner", l.Owner, "worktree", l.Worktree)
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
