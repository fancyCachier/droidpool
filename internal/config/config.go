// Package config 读取 droidpoold 的 TOML 配置。
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// 默认值来自 Phase 1 实测（docs/2026-09-03-phase1-baseline.md）：
//   - 活跃并发上限实测 10 台，取 8 留余量给设备墙截图与 scrcpy
//   - profile 1366×768 @ 160 dpi = 收银机实机 Sunmi D1s 2nd 实测值
//   - use_memfd 不可省（.54 无 ashmem）
const (
	DefaultMaxDevices = 8
	DefaultImage      = "redroid/redroid:14.0.0_64only-latest"
	DefaultBootArgs   = "androidboot.use_memfd=true androidboot.use_redroid_overlayfs=1 " +
		"androidboot.redroid_width=1366 androidboot.redroid_height=768 " +
		"androidboot.redroid_dpi=160 androidboot.redroid_gpu_mode=guest"
)

type Config struct {
	Listen     string   `toml:"listen"`
	Token      string   `toml:"token"`
	DBPath     string   `toml:"db_path"`
	DefaultTTL Duration `toml:"default_ttl"`
	MaxTTL     Duration `toml:"max_ttl"`
	// IdleTimeout 租约多久没有 agent 活动就判定僵死并回收（watchdog 抓僵死的主力闸）。
	IdleTimeout Duration `toml:"idle_timeout"`
	// MaxLifetime 单个租约持有总时长的硬上限，兜住「一直心跳但其实卡死」的情况。
	MaxLifetime Duration `toml:"max_lifetime"`
	// ReapInterval watchdog 巡检间隔。
	ReapInterval Duration `toml:"reap_interval"`
	WarmPool     int      `toml:"warm_pool"`
	// SwapGuardMiB 节点 swap 超过此值即拒绝新 claim。Phase 1 实测 swap 从 0 变正
	// 就是性能断崖（N=12 起失败率 5.6%），比 CPU / 温度更有预警价值。
	SwapGuardMiB int        `toml:"swap_guard_mib"`
	EdgeDefault  EdgeTarget `toml:"edge_default"`
	Nodes        []Node     `toml:"nodes"`
}

type EdgeTarget struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
}

type Node struct {
	Name       string `toml:"name"`
	DockerHost string `toml:"docker_host"` // ssh://sa@192.168.14.54
	ADBHost    string `toml:"adb_host"`
	PortRange  [2]int `toml:"port_range"`
	MaxDevices int    `toml:"max_devices"`
	Image      string `toml:"image"`
	DataRoot   string `toml:"data_root"`
	BootArgs   string `toml:"boot_args"`
}

// Duration 让 TOML 里能写 "4h" 这样的字符串。
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Load 读取并校验配置，同时填入默认值。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8600"
	}
	if c.DBPath == "" {
		c.DBPath = "droidpool.db"
	}
	if c.DefaultTTL.Duration == 0 {
		c.DefaultTTL.Duration = 4 * time.Hour
	}
	if c.MaxTTL.Duration == 0 {
		c.MaxTTL.Duration = 24 * time.Hour
	}
	if c.IdleTimeout.Duration == 0 {
		c.IdleTimeout.Duration = 30 * time.Minute
	}
	if c.MaxLifetime.Duration == 0 {
		c.MaxLifetime.Duration = 24 * time.Hour
	}
	if c.ReapInterval.Duration == 0 {
		c.ReapInterval.Duration = time.Minute
	}
	if c.SwapGuardMiB == 0 {
		c.SwapGuardMiB = 256
	}
	if c.EdgeDefault.Port == 0 {
		c.EdgeDefault.Port = 8090
	}
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.MaxDevices == 0 {
			n.MaxDevices = DefaultMaxDevices
		}
		if n.Image == "" {
			n.Image = DefaultImage
		}
		if n.BootArgs == "" {
			n.BootArgs = DefaultBootArgs
		}
		if n.DataRoot == "" {
			n.DataRoot = "/data/droidpool"
		}
		if n.ADBHost == "" {
			n.ADBHost = hostFromDockerHost(n.DockerHost)
		}
	}
}

// hostFromDockerHost 从 ssh://user@host 提取 host，取不到返回空串。
func hostFromDockerHost(s string) string {
	const p = "ssh://"
	if len(s) <= len(p) || s[:len(p)] != p {
		return ""
	}
	rest := s[len(p):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '@' {
			rest = rest[i+1:]
			i = -1
		}
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' || rest[i] == '/' {
			return rest[:i]
		}
	}
	return rest
}

func (c *Config) validate() error {
	if c.Token == "" {
		return fmt.Errorf("token 不能为空：agent 与设备墙共用它鉴权")
	}
	if len(c.Nodes) == 0 {
		return fmt.Errorf("至少要配一个 [[nodes]]")
	}
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		if n.Name == "" {
			return fmt.Errorf("[[nodes]] 缺 name")
		}
		if seen[n.Name] {
			return fmt.Errorf("节点名重复: %s", n.Name)
		}
		seen[n.Name] = true
		if n.DockerHost == "" {
			return fmt.Errorf("节点 %s 缺 docker_host", n.Name)
		}
		if n.ADBHost == "" {
			return fmt.Errorf("节点 %s 缺 adb_host 且无法从 docker_host 推导", n.Name)
		}
		lo, hi := n.PortRange[0], n.PortRange[1]
		if lo <= 0 || hi < lo {
			return fmt.Errorf("节点 %s 的 port_range 非法: %v", n.Name, n.PortRange)
		}
		if hi-lo+1 < n.MaxDevices {
			return fmt.Errorf("节点 %s 端口区间 %d~%d 容纳不下 max_devices=%d", n.Name, lo, hi, n.MaxDevices)
		}
	}
	return nil
}
