// Package pool 定义设备池的领域模型与状态机。
package pool

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DeviceState 设备状态。状态机见 docs/2026-09-03-roadmap.md §5.2：
//
//	creating ──► ready ──► leased ──► resetting ──► ready
//	                │                     ▲
//	                └──(健康检查连续失败)──┴──► broken ──► creating
type DeviceState string

const (
	StateCreating  DeviceState = "creating"
	StateReady     DeviceState = "ready"
	StateLeased    DeviceState = "leased"
	StateResetting DeviceState = "resetting"
	StateBroken    DeviceState = "broken"
)

// validTransitions 列出每个状态允许转移到的目标状态。
var validTransitions = map[DeviceState][]DeviceState{
	StateCreating:  {StateReady, StateBroken},
	StateReady:     {StateLeased, StateResetting, StateBroken},
	StateLeased:    {StateResetting, StateBroken},
	StateResetting: {StateReady, StateBroken},
	StateBroken:    {StateCreating},
}

var ErrBadTransition = errors.New("非法状态转移")

// CanTransition 报告 from → to 是否为合法转移。
func CanTransition(from, to DeviceState) bool {
	for _, s := range validTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Transition 校验并返回目标状态，非法转移返回 ErrBadTransition。
func Transition(from, to DeviceState) (DeviceState, error) {
	if !CanTransition(from, to) {
		return from, fmt.Errorf("%w: %s → %s", ErrBadTransition, from, to)
	}
	return to, nil
}

// Device 一台 redroid 容器对应的设备。
type Device struct {
	ID          string      `json:"id"`
	Node        string      `json:"node"`
	Container   string      `json:"container"`
	ADBAddr     string      `json:"adb_addr"`
	State       DeviceState `json:"state"`
	CreatedAt   time.Time   `json:"created_at"`
	LastHealthy time.Time   `json:"last_health_at"`
	// HealthFails 连续健康检查失败次数，达到 HealthFailThreshold 即转 broken。
	HealthFails int `json:"health_fails"`
	// EgressProxy 这台设备公网出口用的上游 socks5（如 socks5://host:1080），
	// 空 = 直连。每台设备一份，运行中可改，见 node/egress.go。
	EgressProxy string `json:"egress_proxy"`
	// CameraRTSP 这台设备摄像头画面的来源，空 = 不推流（相机报 0 个设备）。
	// 每台设备一份，运行中可改，见 node/camera.go。
	CameraRTSP string `json:"camera_rtsp"`
	// Identity 这台设备对外报的硬件身份（Build.MODEL 等），nil = 用节点默认。
	// 属性是开机时定死的（ro.*），改它要重建容器，见 Manager.SetIdentity。
	Identity *Identity `json:"identity,omitempty"`
	// MockLocation 这台设备的 mock 定位 "纬度,经度"，空 = 用节点默认。
	// 运行中可改、即时生效；容器重建后要重放，见 node/identity.go。
	MockLocation string `json:"mock_location"`
}

// Identity 设备对外报的硬件身份，对应 Android 的 Build.MODEL / BRAND /
// MANUFACTURER / DEVICE / PRODUCT 与 Build.getSerial()。
//
// 这几项来自镜像里四个分区的 build.prop（ro.product.<分区>.model 等，
// 取值顺序 product → odm → vendor → system_ext → system，所以四份都得覆盖），
// 序列号则来自启动参数 androidboot.serialno。ro.build.fingerprint 由
// brand/name/device 派生，会自动跟着变。ro.hardware=redroid 改不了——
// init 靠它选 rc 与 HAL。
type Identity struct {
	Model        string `json:"model" toml:"model"`
	Brand        string `json:"brand" toml:"brand"`
	Manufacturer string `json:"manufacturer" toml:"manufacturer"`
	Device       string `json:"device" toml:"device"`
	Name         string `json:"name" toml:"name"`
	// Serial 留空时按设备 id 派生，保证每台不同。
	Serial string `json:"serial" toml:"serial"`
}

// IsZero 报告是否一个字段都没填。
func (id Identity) IsZero() bool { return id == Identity{} }

// Normalized 补齐没填的字段：品牌与厂商互相兜底，device/name 从型号派生。
// 用户只给 --model 与 --brand 就够用。
func (id Identity) Normalized() Identity {
	if id.Brand == "" {
		id.Brand = id.Manufacturer
	}
	if id.Manufacturer == "" {
		id.Manufacturer = id.Brand
	}
	if id.Device == "" {
		id.Device = slug(id.Model)
	}
	if id.Name == "" {
		id.Name = id.Device
	}
	return id
}

// slug 把型号压成 device 名的形状：小写、只留字母数字。
func slug(s string) string {
	var b []byte
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b = append(b, byte(r))
		case r >= 'A' && r <= 'Z':
			b = append(b, byte(r+'a'-'A'))
		}
	}
	return string(b)
}

// Validate 校验各字段。值会原样写进 build.prop 与启动参数（androidboot.serialno），
// 所以只放行字母数字与少数标点，其余一律拒掉——比逐个转义可靠。
func (id Identity) Validate() error {
	if id.Model == "" {
		return errors.New("identity 至少要有 model")
	}
	if id.Brand == "" && id.Manufacturer == "" {
		return errors.New("identity 要有 brand 或 manufacturer")
	}
	for name, v := range map[string]string{
		"model": id.Model, "brand": id.Brand, "manufacturer": id.Manufacturer,
		"device": id.Device, "name": id.Name,
	} {
		if v == "" {
			continue
		}
		if len(v) > 64 || !identityValue.MatchString(v) {
			return fmt.Errorf("identity.%s %q 非法：只能是字母数字与空格 . _ + -，且不超过 64 字符", name, v)
		}
	}
	if id.Serial != "" && !serialValue.MatchString(id.Serial) {
		return fmt.Errorf("identity.serial %q 非法：只能是字母数字，1~32 位", id.Serial)
	}
	return nil
}

var (
	identityValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._+-]*$`)
	serialValue   = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)
)

// ParseLocation 解析 "纬度,经度"。空串合法，表示不 mock。
func ParseLocation(s string) (lat, lng float64, err error) {
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("定位要写成 纬度,经度（如 23.1291,113.2644），得到 %q", s)
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lng, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return 0, 0, fmt.Errorf("定位 %q 不是合法经纬度：纬度 -90~90，经度 -180~180", s)
	}
	return lat, lng, nil
}

// FormatLocation 把经纬度写回 "纬度,经度"，去掉多余的空白与位数。
func FormatLocation(lat, lng float64) string {
	return strconv.FormatFloat(lat, 'f', -1, 64) + "," + strconv.FormatFloat(lng, 'f', -1, 64)
}

// HealthFailThreshold 连续失败多少次判定设备损坏。
const HealthFailThreshold = 3

// EdgeMode 租约使用的 Edge 形态。dedicated 为 Phase 4 预留。
type EdgeMode string

const (
	EdgeShared    EdgeMode = "shared"
	EdgeDedicated EdgeMode = "dedicated"
)

// Lease 一次设备租用。
type Lease struct {
	ID       string `json:"id"`
	DeviceID string `json:"device_id"`
	// Owner 形如 user@host，Host 与 Worktree 组成幂等键。
	Owner    string `json:"owner"`
	Host     string `json:"host"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	HeadSHA  string `json:"head_sha"`

	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// LastSeenAt 最后一次被 agent 碰到的时刻（watchdog 的活跃度信号）。
	// 僵死的 agent 不再刷新它，空闲超时到了就被回收，不必等满整个 TTL。
	LastSeenAt time.Time `json:"last_seen_at"`

	// HumanTakeover 为真时 agent 应停手，等操作人员交还（设备墙上的接管开关）。
	HumanTakeover bool     `json:"human_takeover"`
	HumanNote     string   `json:"human_note,omitempty"`
	EdgeMode      EdgeMode `json:"edge_mode"`
}

// Expired 报告租约在 now 时是否已过期。
func (l *Lease) Expired(now time.Time) bool {
	return !now.Before(l.ExpiresAt)
}

// ReapReason 说明一条租约为何该被回收，空串表示不该回收。
type ReapReason string

const (
	ReapExpired ReapReason = "ttl_expired"  // 到了约定的到期时刻
	ReapIdle    ReapReason = "idle_timeout" // 久未活动，判定 agent 僵死
	ReapTooLong ReapReason = "max_lifetime" // 一直有心跳但持有过久，硬上限兜底
)

// ShouldReap 判断租约是否该被 watchdog 回收，并给出原因。
//
// 三道闸各管一种失效：
//   - TTL 到期：约定时间到了。
//   - 空闲超时：agent 僵死（进程还在但不再干活），它不会再刷新 LastSeenAt。
//     这是抓僵死的主力闸，比 TTL 快得多。
//   - 生命周期上限：agent 卡在循环里一直心跳，TTL 与空闲闸都拦不住它，
//     用持有总时长兜底。
//
// idleTimeout / maxLifetime 传 0 表示不启用该闸。
// 人工接管中的租约豁免空闲闸——那时本来就该没有 agent 活动。
func (l *Lease) ShouldReap(now time.Time, idleTimeout, maxLifetime time.Duration) ReapReason {
	if maxLifetime > 0 && !l.CreatedAt.IsZero() && now.Sub(l.CreatedAt) >= maxLifetime {
		return ReapTooLong
	}
	if l.Expired(now) {
		return ReapExpired
	}
	if idleTimeout > 0 && !l.HumanTakeover && !l.LastSeenAt.IsZero() && now.Sub(l.LastSeenAt) >= idleTimeout {
		return ReapIdle
	}
	return ""
}

// IdempotencyKey 同一 host 上同一 worktree 重复 claim 时复用既有租约。
// 用长度前缀而非单纯的分隔符：裸分隔符可被字段内容绕过（"a"+"b|c" 与 "a|b"+"c" 会撞键）。
func (l *Lease) IdempotencyKey() string {
	return fmt.Sprintf("%d:%s|%d:%s", len(l.Host), l.Host, len(l.Worktree), l.Worktree)
}
