// Package pool 定义设备池的领域模型与状态机。
package pool

import (
	"errors"
	"fmt"
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

	// HumanTakeover 为真时 agent 应停手，等操作人员交还（设备墙上的接管开关）。
	HumanTakeover bool     `json:"human_takeover"`
	HumanNote     string   `json:"human_note,omitempty"`
	EdgeMode      EdgeMode `json:"edge_mode"`
}

// Expired 报告租约在 now 时是否已过期。
func (l *Lease) Expired(now time.Time) bool {
	return !now.Before(l.ExpiresAt)
}

// IdempotencyKey 同一 host 上同一 worktree 重复 claim 时复用既有租约。
// 用长度前缀而非单纯的分隔符：裸分隔符可被字段内容绕过（"a"+"b|c" 与 "a|b"+"c" 会撞键）。
func (l *Lease) IdempotencyKey() string {
	return fmt.Sprintf("%d:%s|%d:%s", len(l.Host), l.Host, len(l.Worktree), l.Worktree)
}
