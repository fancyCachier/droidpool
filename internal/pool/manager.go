package pool

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// NodeDriver 是 Manager 需要的节点能力子集（由 internal/node.Node 实现）。
type NodeDriver interface {
	Create(ctx context.Context, deviceID string, port int, overlayBase string) error
	Remove(ctx context.Context, deviceID string) error
	// WipeData 清空设备数据目录。复位真正生效的一步——只重建容器的话，
	// 宿主上的数据目录还在，上一个 agent 的状态会留给下一个。
	WipeData(ctx context.Context, deviceID, overlayBase string) error
	WaitBoot(ctx context.Context, deviceID string, timeout time.Duration) error
}

// DeviceStore 是 Manager 需要的存储能力子集。
type DeviceStore interface {
	UpsertDevice(d *Device) error
	GetDevice(id string) (*Device, error)
	ListDevices() ([]*Device, error)
	SetDeviceState(id string, to DeviceState) error
}

// Manager 负责把设备拉起来、复位、以及节点上的容器与库内记录对账。
type Manager struct {
	NodeName    string
	ADBHost     string
	Driver      NodeDriver
	Store       DeviceStore
	MaxDevices  int
	PortBase    int
	OverlayBase string // 非空则用 overlayfs 共享 data（零拷贝复位）
	BootTimeout time.Duration
	Log         *slog.Logger
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

func (m *Manager) bootTimeout() time.Duration {
	if m.BootTimeout > 0 {
		return m.BootTimeout
	}
	return 90 * time.Second
}

// DeviceID 第 i 台（从 1 起）的设备 id。
func (m *Manager) DeviceID(i int) string { return fmt.Sprintf("%s-%d", m.NodeName, i) }

// Port 第 i 台的宿主端口。
func (m *Manager) Port(i int) int { return m.PortBase + i }

// ADBAddr 第 i 台的 adb 地址。
func (m *Manager) ADBAddr(i int) string { return fmt.Sprintf("%s:%d", m.ADBHost, m.Port(i)) }

// Ensure 把池补齐到 MaxDevices 台 ready 设备。已存在且非 broken 的不动。
// 幂等：重启 droidpoold 后再调一次不会重建正常设备。
func (m *Manager) Ensure(ctx context.Context) error {
	for i := 1; i <= m.MaxDevices; i++ {
		id := m.DeviceID(i)
		d, err := m.Store.GetDevice(id)
		if err == nil && d.State != StateBroken {
			continue
		}
		if err := m.createOne(ctx, i); err != nil {
			m.log().Error("创建设备失败", "device", id, "err", err)
			// 一台起不来不该阻断其余设备
			continue
		}
	}
	return nil
}

func (m *Manager) createOne(ctx context.Context, i int) error {
	id := m.DeviceID(i)
	d := &Device{
		ID: id, Node: m.NodeName, Container: "droidpool-" + id,
		ADBAddr: m.ADBAddr(i), State: StateCreating, CreatedAt: time.Now(),
	}
	if err := m.Store.UpsertDevice(d); err != nil {
		return err
	}
	if err := m.Driver.Create(ctx, id, m.Port(i), m.OverlayBase); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	if err := m.Driver.WaitBoot(ctx, id, m.bootTimeout()); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	d.State = StateReady
	d.LastHealthy = time.Now()
	if err := m.Store.UpsertDevice(d); err != nil {
		return err
	}
	m.log().Info("设备就绪", "device", id, "adb", d.ADBAddr)
	return nil
}

// ReconcileStore 让库与节点对齐，在 Ensure 之前跑。
//
// 守护进程重启、上一版的 bug、有人手动 docker rm——都会让库里的状态和节点实况脱节。
// 两种脱节各有后果：
//   - 库说 ready/leased 但节点没容器：会一直把死设备分给人（health 循环 90 s 后才纠正）
//   - 库说 resetting 但没人在复位（上一版 release 后无人接手）：永远卡住
//
// running 是节点上实际在跑的容器名集合。
func (m *Manager) ReconcileStore(ctx context.Context, running map[string]bool) {
	devices, err := m.Store.ListDevices()
	if err != nil {
		m.log().Error("对账读库失败", "err", err)
		return
	}
	for _, d := range devices {
		switch d.State {
		case StateReady, StateLeased:
			if !running[d.Container] {
				m.log().Warn("库里活跃但节点无容器，标 broken", "device", d.ID, "was", d.State)
				d.State = StateBroken
				_ = m.Store.UpsertDevice(d)
			}
		case StateResetting, StateCreating:
			// 没人在做这件事了（进程都重启了），直接重来
			m.log().Warn("库里卡在中间态，重新复位", "device", d.ID, "was", d.State)
			if err := m.Reset(ctx, d.ID); err != nil {
				m.log().Error("对账复位失败", "device", d.ID, "err", err)
			}
		}
	}
}

// Reset 复位一台设备：重建容器（overlay 模式下等价于丢弃 diff）→ 等 boot → 置 ready。
// 实现 Reaper 的 Resetter 接口。
func (m *Manager) Reset(ctx context.Context, deviceID string) error {
	d, err := m.Store.GetDevice(deviceID)
	if err != nil {
		return err
	}
	i, err := m.indexOf(deviceID)
	if err != nil {
		return err
	}
	if err := m.Driver.Remove(ctx, deviceID); err != nil {
		m.log().Warn("删除容器失败，仍尝试重建", "device", deviceID, "err", err)
	}
	// 必须真的清空数据目录：删容器不删数据 = 假复位，脏状态原样留给下一个 agent。
	// 清不干净就宁可把设备标 broken，也不能把「看起来干净」的设备放回池子。
	if err := m.Driver.WipeData(ctx, deviceID, m.OverlayBase); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return fmt.Errorf("清空设备数据失败，已标记 broken: %w", err)
	}
	if err := m.Driver.Create(ctx, deviceID, m.Port(i), m.OverlayBase); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	if err := m.Driver.WaitBoot(ctx, deviceID, m.bootTimeout()); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	d.State = StateReady
	d.LastHealthy = time.Now()
	d.HealthFails = 0
	if err := m.Store.UpsertDevice(d); err != nil {
		return err
	}
	m.log().Info("设备已复位", "device", deviceID)
	return nil
}

// indexOf 从设备 id 反解出序号。
func (m *Manager) indexOf(deviceID string) (int, error) {
	var i int
	if _, err := fmt.Sscanf(deviceID, m.NodeName+"-%d", &i); err != nil || i <= 0 {
		return 0, fmt.Errorf("无法从设备 id %q 解析序号", deviceID)
	}
	return i, nil
}
