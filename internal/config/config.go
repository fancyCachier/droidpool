// Package config 读取 droidpoold 的 TOML 配置。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/fancyCachier/droidpool/internal/pool"
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
	// MinAvailMiB 节点可用内存低于此值即拒绝新 claim。
	// 实测每台设备常驻约 1 GB，默认 2048 留一台的量加余量。
	// 不用 swap 做闸——它是滞后且黏滞的症状，见 node.Health.UnderPressure。
	MinAvailMiB int        `toml:"min_avail_mib"`
	EdgeDefault EdgeTarget `toml:"edge_default"`
	// TLS 设备墙的 HTTPS 入口。WebCodecs 只在安全上下文（HTTPS / localhost）里存在，
	// 用 http://内网IP 打开设备墙只能拿到 3 fps 截图流。证书由别处（acme.sh）签好
	// 推到 cert / key 路径，文件换新后自动生效，不用重启。
	TLS   TLS    `toml:"tls"`
	Nodes []Node `toml:"nodes"`
}

type TLS struct {
	Listen string `toml:"listen"` // 如 "0.0.0.0:443"；空 = 不开 HTTPS
	Cert   string `toml:"cert"`   // fullchain PEM
	Key    string `toml:"key"`    // 私钥 PEM
	// WallURL 设了之后，在 http 监听上打开设备墙页面会 302 到这里；API 不受影响，
	// agent CLI 与 MCP 仍走 http。
	WallURL string `toml:"wall_url"`
}

// Enabled 报告是否要开 HTTPS 监听。
func (t TLS) Enabled() bool { return t.Listen != "" }

type EdgeTarget struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
}

type Node struct {
	Name       string `toml:"name"`
	DockerHost string `toml:"docker_host"` // 如 ssh://user@node-host
	ADBHost    string `toml:"adb_host"`
	PortRange  [2]int `toml:"port_range"`
	MaxDevices int    `toml:"max_devices"`
	Image      string `toml:"image"`
	DataRoot   string `toml:"data_root"`
	BootArgs   string `toml:"boot_args"`
	// Egress 打开后每台设备多起两个辅助容器（tun 边车 + socks5 中继），
	// 公网流量经中继落地，内网仍直连；具体出口地址每台设备各自在设备墙上设。
	// 节点需要加载 tun 模块（modprobe tun）。
	Egress bool `toml:"egress"`
	// EgressDNS 隧道内用的解析器，如 223.5.5.5。留空则沿用 docker 默认，
	// 但那通常是宿主的 resolver，在隧道里不可达，域名会全部解析不了。
	EgressDNS string `toml:"egress_dns"`
	// CameraVideoBase 非 0 时，每台设备透传 /dev/video<base+序号>，
	// 配合自建镜像里的外接摄像头 HAL。画面由宿主侧的 rtsp-camera.sh 灌进去。
	// 需要节点跑过 deploy/node/setup-node.sh（v4l2loopback）。
	CameraVideoBase int `toml:"camera_video_base"`
	// Identity 节点上所有设备默认报的硬件身份（Build.MODEL 等），不填 = 镜像原样
	// （redroid14_arm64_only）。每台设备可经 API / CLI 单独覆盖。
	Identity pool.Identity `toml:"identity"`
	// MockLocation 默认 mock 定位 "纬度,经度"，不填 = 不 mock。每台设备可单独覆盖。
	MockLocation string `toml:"mock_location"`
}

// DefaultIdentity 节点默认身份，没配时为 nil。已补齐派生字段。
func (n Node) DefaultIdentity() *pool.Identity {
	if n.Identity.IsZero() {
		return nil
	}
	id := n.Identity.Normalized()
	return &id
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
	// 先展开 ${VAR}：token 这类秘密不该进仓库，放 EnvironmentFile 里由 systemd 注入，
	// 配置文件只留占位。未设置的变量展开为空串，随后会被 validate 拦下。
	b = []byte(os.ExpandEnv(string(b)))
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
	if c.MinAvailMiB == 0 {
		c.MinAvailMiB = 2048
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
	if c.TLS.Enabled() && (c.TLS.Cert == "" || c.TLS.Key == "") {
		return fmt.Errorf("[tls] 开了 listen 就必须同时给 cert 与 key")
	}
	if u := c.TLS.WallURL; u != "" {
		if !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("[tls] wall_url 必须以 https:// 开头，得到 %q", u)
		}
		c.TLS.WallURL = strings.TrimRight(u, "/")
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
		if id := n.DefaultIdentity(); id != nil {
			if err := id.Validate(); err != nil {
				return fmt.Errorf("节点 %s 的 identity: %w", n.Name, err)
			}
		}
		if _, _, err := pool.ParseLocation(n.MockLocation); err != nil {
			return fmt.Errorf("节点 %s 的 mock_location: %w", n.Name, err)
		}
	}
	return nil
}
