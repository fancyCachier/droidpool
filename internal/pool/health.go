package pool

import (
	"context"
	"log/slog"
	"time"
)

// Prober 探测一台设备是否还应答（由 adb.Client.Alive 实现）。
type Prober interface {
	Alive(ctx context.Context, serial string) bool
}

// HealthChecker 定期探活，连续失败达到阈值就把设备标 broken 并交给 Resetter 重建。
//
// 没有它，一台容器挂了池子不知道，会一直把它分给人；agent 拿到手才发现 adb 连不上，
// 白白浪费一次 claim 加排查时间。
type HealthChecker struct {
	Store    DeviceStore
	Prober   Prober
	Resetter Resetter
	Interval time.Duration
	// Threshold 连续失败几次判死。默认 HealthFailThreshold（3）：
	// 单次失败可能只是 adb 抖动，三次连败才算真挂。
	Threshold int
	Log       *slog.Logger
}

func (h *HealthChecker) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *HealthChecker) threshold() int {
	if h.Threshold > 0 {
		return h.Threshold
	}
	return HealthFailThreshold
}

// CheckOnce 探一轮。返回本轮被标为 broken 的设备 id。
//
// 只探 ready 与 leased 两态：creating/resetting 本来就没准备好，broken 已经死了。
// 一台设备探测失败不影响其余——逐台处理，不中断循环。
func (h *HealthChecker) CheckOnce(ctx context.Context) (broken []string) {
	devices, err := h.Store.ListDevices()
	if err != nil {
		h.log().Error("健康检查读设备列表失败", "err", err)
		return nil
	}
	now := time.Now()
	for _, d := range devices {
		if d.State != StateReady && d.State != StateLeased {
			continue
		}
		if h.Prober.Alive(ctx, d.ADBAddr) {
			if d.HealthFails != 0 || d.LastHealthy.IsZero() {
				d.HealthFails = 0
				d.LastHealthy = now
				_ = h.Store.UpsertDevice(d)
			} else {
				d.LastHealthy = now
				_ = h.Store.UpsertDevice(d)
			}
			continue
		}
		d.HealthFails++
		if d.HealthFails < h.threshold() {
			h.log().Warn("设备探活失败", "device", d.ID, "fails", d.HealthFails, "threshold", h.threshold())
			_ = h.Store.UpsertDevice(d)
			continue
		}
		// 连续失败到阈值：标 broken。leased 态的设备也一样标——
		// 持有它的 agent 下次 status 会看到 broken，比让它对着死设备干等强。
		prev := d.State
		d.State = StateBroken
		if err := h.Store.UpsertDevice(d); err != nil {
			h.log().Error("标记 broken 失败", "device", d.ID, "err", err)
			continue
		}
		broken = append(broken, d.ID)
		h.log().Error("设备判死，标记 broken", "device", d.ID, "prev_state", prev, "fails", d.HealthFails)
		if h.Resetter != nil {
			// 重建放后台：一台重建要十几秒，不能卡住整轮巡检
			go func(id string) {
				rctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				if err := h.Resetter.Reset(rctx, id); err != nil {
					h.log().Error("重建 broken 设备失败", "device", id, "err", err)
				}
			}(d.ID)
		}
	}
	return broken
}

// Run 阻塞循环，直到 ctx 取消。
func (h *HealthChecker) Run(ctx context.Context) {
	interval := h.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.CheckOnce(ctx)
		}
	}
}
