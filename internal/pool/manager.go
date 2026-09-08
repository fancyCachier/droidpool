package pool

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// NodeDriver 是 Manager 需要的节点能力子集（由 internal/node.Node 实现）。
type NodeDriver interface {
	// Create 起容器。ident 非 nil 时把硬件身份写进镜像的 build.prop 并设序列号，
	// 这些是开机就定死的 ro.* 属性，所以只能在这里给。
	Create(ctx context.Context, deviceID string, port int, overlayBase string, ident *Identity) error
	Remove(ctx context.Context, deviceID string) error
	// WipeData 清空设备数据目录。复位真正生效的一步——只重建容器的话，
	// 宿主上的数据目录还在，上一个 agent 的状态会留给下一个。
	WipeData(ctx context.Context, deviceID, overlayBase string) error
	WaitBoot(ctx context.Context, deviceID string, timeout time.Duration) error
	// FinishEgress 在设备起来之后把流量导进出口隧道。必须等 Android 的 netd
	// 装完它自己的路由规则，早了会被覆盖。未开出口时是空操作。
	FinishEgress(ctx context.Context, deviceID string) error
	// SetEgress 换这台设备的上游出口。只重建中继容器，设备不重启。
	SetEgress(ctx context.Context, deviceID, proxy string) error
	// SetCamera 换这台设备的摄像头画面源。只重建推流容器，设备不重启。
	SetCamera(ctx context.Context, deviceID, rtsp string) error
	// SetLocation 给运行中的设备设 mock 定位（"纬度,经度"），空串撤销。
	SetLocation(ctx context.Context, deviceID, location string) error
}

// DeviceStore 是 Manager 需要的存储能力子集。
type DeviceStore interface {
	UpsertDevice(d *Device) error
	GetDevice(id string) (*Device, error)
	ListDevices() ([]*Device, error)
	SetDeviceState(id string, to DeviceState) error
	SetDeviceEgress(id, proxy string) error
	SetDeviceCamera(id, rtsp string) error
	SetDeviceIdentity(id string, ident *Identity) error
	SetDeviceLocation(id, location string) error
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
	// DefaultIdentity 节点上所有设备的默认硬件身份，nil = 镜像原样。
	// 设备各自的 Identity 非 nil 时优先。
	DefaultIdentity *Identity
	// DefaultLocation 节点默认的 mock 定位，空 = 不 mock。设备各自的优先。
	DefaultLocation string
	Log             *slog.Logger
}

// effectiveIdentity 这台设备实际该用的身份：自己的优先，否则节点默认。
func (m *Manager) effectiveIdentity(d *Device) *Identity {
	if d != nil && d.Identity != nil {
		return d.Identity
	}
	return m.DefaultIdentity
}

// effectiveLocation 这台设备实际该用的 mock 定位：自己的优先，否则节点默认。
func (m *Manager) effectiveLocation(d *Device) string {
	if d != nil && d.MockLocation != "" {
		return d.MockLocation
	}
	return m.DefaultLocation
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
	// 库里可能已有这台设备的身份覆盖（broken 重建时），要带上
	if old, err := m.Store.GetDevice(id); err == nil {
		d.Identity = old.Identity
	}
	if err := m.Driver.Create(ctx, id, m.Port(i), m.OverlayBase, m.effectiveIdentity(d)); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	if err := m.Driver.WaitBoot(ctx, id, m.bootTimeout()); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	if err := m.applyDeviceSettings(ctx, id); err != nil {
		// 出口没接上不至于让设备不可用（此时是直连），但要留下痕迹，
		// 否则「以为走了代理其实没走」比直接失败更危险。
		m.log().Error("每设备设置未落上，该设备为直连且无摄像头", "device", id, "err", err)
	}
	d.State = StateReady
	d.LastHealthy = time.Now()
	if err := m.Store.UpsertDevice(d); err != nil {
		return err
	}
	m.log().Info("设备就绪", "device", id, "adb", d.ADBAddr)
	return nil
}

// applyDeviceSettings 把库里记着的每设备设置落到刚起来的设备上。
//
// 设备重建（复位、节点重启）后容器是全新的，出口与摄像头都不会自己回来，
// 必须重放。摄像头尤其要紧：推流容器是对着某个 /dev/videoN 灌的，设备换了
// 而流没换，画面就对着一个没人读的节点空转。
func (m *Manager) applyDeviceSettings(ctx context.Context, deviceID string) error {
	if err := m.Driver.FinishEgress(ctx, deviceID); err != nil {
		return err
	}
	d, err := m.Store.GetDevice(deviceID)
	if err != nil {
		return err
	}
	if d.EgressProxy != "" {
		if err := m.Driver.SetEgress(ctx, deviceID, d.EgressProxy); err != nil {
			return err
		}
	}
	if d.CameraRTSP != "" {
		if err := m.Driver.SetCamera(ctx, deviceID, d.CameraRTSP); err != nil {
			return err
		}
	}
	// mock 定位是运行态的（test provider 不落盘），容器一重建就没了，必须重放
	if loc := m.effectiveLocation(d); loc != "" {
		if err := m.Driver.SetLocation(ctx, deviceID, loc); err != nil {
			return err
		}
	}
	return nil
}

// SetLocation 改一台设备的 mock 定位并落库。运行中即时生效，设备与租约都不动。
// location 为空表示撤销覆盖、回到节点默认（节点默认也为空时就是不 mock）。
func (m *Manager) SetLocation(ctx context.Context, deviceID, location string) error {
	d, err := m.Store.GetDevice(deviceID)
	if err != nil {
		return err
	}
	if err := m.Store.SetDeviceLocation(deviceID, location); err != nil {
		return err
	}
	d.MockLocation = location
	// 设备没在跑就只落库，等它起来时 applyDeviceSettings 会重放
	if d.State != StateReady && d.State != StateLeased {
		return nil
	}
	return m.Driver.SetLocation(ctx, deviceID, m.effectiveLocation(d))
}

// SetIdentity 改一台设备的硬件身份并落库，然后**重建**这台设备让它生效。
//
// 身份是开机定死的 ro.* 属性，没有运行中改的办法（改 build.prop 要重启，
// setprop 对 ro.* 无效）。重建等价于复位：数据清空、容器重开，约 20~40 s。
// 租约保留——agent 拿到手先设身份再装包，代价就只是等这几十秒；
// adb 端口不变，重连即可。ident 为 nil 表示撤销覆盖、回到节点默认。
//
// 返回实际生效的身份，以及这次有没有真的重建：身份没变、或设备正处于
// creating/resetting/broken（已经有人在重建它）时只落库不重建。
func (m *Manager) SetIdentity(ctx context.Context, deviceID string, ident *Identity) (effective *Identity, rebuilt bool, err error) {
	d, err := m.Store.GetDevice(deviceID)
	if err != nil {
		return nil, false, err
	}
	if ident != nil {
		n := ident.Normalized()
		if err := n.Validate(); err != nil {
			return nil, false, err
		}
		ident = &n
	}
	before := m.effectiveIdentity(d)
	if err := m.Store.SetDeviceIdentity(deviceID, ident); err != nil {
		return nil, false, err
	}
	d.Identity = ident
	effective = m.effectiveIdentity(d)
	if sameIdentity(before, effective) {
		return effective, false, nil
	}
	if d.State != StateReady && d.State != StateLeased {
		return effective, false, nil
	}
	if d.State == StateReady {
		// 先摘出池子，免得重建到一半被 claim 走：agent 拿到的是一台还在开机的设备，
		// 而重建结束时的回写又会把 leased 冲成 ready。
		if err := m.Store.SetDeviceState(deviceID, StateResetting); err != nil {
			return effective, false, err
		}
	}
	if err := m.rebuild(ctx, d); err != nil {
		return effective, false, err
	}
	// 重建期间状态可能变了（leased 设备被归还），以库里的为准，不用 d 上的旧值
	cur, err := m.Store.GetDevice(deviceID)
	if err != nil {
		return effective, true, err
	}
	if cur.State == StateResetting {
		cur.State = StateReady
	}
	cur.LastHealthy = time.Now()
	cur.HealthFails = 0
	if err := m.Store.UpsertDevice(cur); err != nil {
		return effective, true, err
	}
	// 撤销覆盖且节点没配默认身份时 effective 为 nil（回到镜像原样），不能直接取 Model
	model := "(镜像原样)"
	if effective != nil {
		model = effective.Model
	}
	m.log().Info("设备已按新身份重建", "device", deviceID, "model", model)
	return effective, true, nil
}

// sameIdentity 比较两份身份（nil 安全）。
func sameIdentity(a, b *Identity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SetCamera 改一台设备的摄像头画面源并落库。推流容器重建不影响设备与租约。
//
// 按需：默认空串（不推流）。一路 720p@15 的转码在这台节点上实测约占一个核的
// 71%（其中 MJPEG 编码约 59%，是大头，而这个 ffmpeg 构建没有 mjpeg_rkmpp
// 硬件编码器——硬件与 MPP 库都支持，只是没编进去）。8 台全开会吃掉近 6 个核，
// 所以只给真正需要摄像头的设备开。
func (m *Manager) SetCamera(ctx context.Context, deviceID, rtsp string) error {
	if _, err := m.Store.GetDevice(deviceID); err != nil {
		return err
	}
	if err := m.Driver.SetCamera(ctx, deviceID, rtsp); err != nil {
		return err
	}
	return m.Store.SetDeviceCamera(deviceID, rtsp)
}

// SetEgress 改一台设备的上游出口并落库。中继重建不影响设备与租约。
func (m *Manager) SetEgress(ctx context.Context, deviceID, proxy string) error {
	if _, err := m.Store.GetDevice(deviceID); err != nil {
		return err
	}
	if err := m.Driver.SetEgress(ctx, deviceID, proxy); err != nil {
		return err
	}
	return m.Store.SetDeviceEgress(deviceID, proxy)
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
	if err := m.rebuild(ctx, d); err != nil {
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

// rebuild 删容器 → 清数据 → 按当前身份重建 → 等 boot → 重放每设备设置。
// 不改库里的状态字段，由调用方决定重建完算 ready 还是保持 leased。
// 失败时标 broken。
func (m *Manager) rebuild(ctx context.Context, d *Device) error {
	deviceID := d.ID
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
	if err := m.Driver.Create(ctx, deviceID, m.Port(i), m.OverlayBase, m.effectiveIdentity(d)); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	if err := m.Driver.WaitBoot(ctx, deviceID, m.bootTimeout()); err != nil {
		d.State = StateBroken
		_ = m.Store.UpsertDevice(d)
		return err
	}
	if err := m.applyDeviceSettings(ctx, deviceID); err != nil {
		// 同 createOne：出口没接上时设备是直连，可用但不是预期状态，必须留痕
		m.log().Error("每设备设置未落上，该设备为直连且无摄像头", "device", deviceID, "err", err)
	}
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
