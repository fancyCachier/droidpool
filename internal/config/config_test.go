package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write 把内容写进临时文件并返回路径。
func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimal = `
token = "secret"
[[nodes]]
name = "3588-a"
docker_host = "ssh://sa@192.168.14.54"
port_range = [5561, 5576]
`

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if c.Listen != "0.0.0.0:8600" {
		t.Errorf("Listen 默认值 = %q", c.Listen)
	}
	if c.DefaultTTL.Duration != 4*time.Hour {
		t.Errorf("DefaultTTL 默认值 = %v", c.DefaultTTL.Duration)
	}
	if c.MinAvailMiB != 2048 {
		t.Errorf("MinAvailMiB 默认值 = %d", c.MinAvailMiB)
	}
	if c.EdgeDefault.Port != 8090 {
		t.Errorf("Edge 默认端口 = %d", c.EdgeDefault.Port)
	}
	n := c.Nodes[0]
	if n.MaxDevices != DefaultMaxDevices {
		t.Errorf("MaxDevices 默认值 = %d，期望 %d", n.MaxDevices, DefaultMaxDevices)
	}
	if n.Image != DefaultImage {
		t.Errorf("Image 默认值 = %q", n.Image)
	}
	// boot_args 必须带 use_memfd：.54 无 ashmem，漏了容器起不来
	if !strings.Contains(n.BootArgs, "use_memfd=true") {
		t.Errorf("默认 BootArgs 缺 use_memfd: %q", n.BootArgs)
	}
	// 分辨率必须是收银机实机实测值
	if !strings.Contains(n.BootArgs, "redroid_width=1366") || !strings.Contains(n.BootArgs, "redroid_dpi=160") {
		t.Errorf("默认 BootArgs 不是实机 profile: %q", n.BootArgs)
	}
	if n.ADBHost != "192.168.14.54" {
		t.Errorf("ADBHost 应从 docker_host 推导出 192.168.14.54，得到 %q", n.ADBHost)
	}
}

func TestLoadParsesDuration(t *testing.T) {
	// 注意 TOML 语义：键必须放在 [[nodes]] 之前，否则会落进那张表里
	c, err := Load(write(t, "default_ttl = \"90m\"\n"+minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultTTL.Duration != 90*time.Minute {
		t.Errorf("default_ttl = %v，期望 90m", c.DefaultTTL.Duration)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name, content, wantSubstr string
	}{
		{
			"缺 token",
			"[[nodes]]\nname=\"a\"\ndocker_host=\"ssh://x@h\"\nport_range=[1,10]\n",
			"token",
		},
		{
			"没有节点",
			"token = \"s\"\n",
			"nodes",
		},
		{
			"节点缺 name",
			"token=\"s\"\n[[nodes]]\ndocker_host=\"ssh://x@h\"\nport_range=[1,10]\n",
			"name",
		},
		{
			"节点名重复",
			"token=\"s\"\n[[nodes]]\nname=\"a\"\ndocker_host=\"ssh://x@h\"\nport_range=[1,10]\n[[nodes]]\nname=\"a\"\ndocker_host=\"ssh://y@h2\"\nport_range=[1,10]\n",
			"重复",
		},
		{
			"端口区间倒置",
			"token=\"s\"\n[[nodes]]\nname=\"a\"\ndocker_host=\"ssh://x@h\"\nport_range=[100,10]\n",
			"port_range",
		},
		{
			// 端口不够放下 max_devices 台，跑起来才发现就晚了
			"端口区间容纳不下 max_devices",
			"token=\"s\"\n[[nodes]]\nname=\"a\"\ndocker_host=\"ssh://x@h\"\nport_range=[5561,5563]\nmax_devices=8\n",
			"容纳不下",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(write(t, c.content))
			if err == nil {
				t.Fatalf("非法配置应报错")
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Errorf("错误信息 %q 未提到 %q", err.Error(), c.wantSubstr)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "不存在.toml")); err == nil {
		t.Error("读不存在的文件应报错")
	}
}

func TestHostFromDockerHost(t *testing.T) {
	cases := map[string]string{
		"ssh://sa@192.168.14.54":    "192.168.14.54",
		"ssh://sa@192.168.14.54:22": "192.168.14.54",
		"ssh://host-only":           "host-only",
		"ssh://user@host/some/path": "host",
		"tcp://192.168.1.1:2375":    "", // 非 ssh 前缀不推导
		"":                          "",
		"ssh://":                    "",
	}
	for in, want := range cases {
		if got := hostFromDockerHost(in); got != want {
			t.Errorf("hostFromDockerHost(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// token 放 EnvironmentFile 由 systemd 注入，配置文件只留 ${DROIDPOOL_TOKEN} 占位。
func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("DROIDPOOL_TOKEN", "from-env-xyz")
	c, err := Load(write(t, "token = \"${DROIDPOOL_TOKEN}\"\n"+minimal[len("\ntoken = \"secret\"\n"):]))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "from-env-xyz" {
		t.Errorf("应从环境变量展开 token，得到 %q", c.Token)
	}
}

func TestLoadRejectsUnsetEnvToken(t *testing.T) {
	t.Setenv("DROIDPOOL_TOKEN", "")
	// 占位符未设置时展开为空，必须被拦下而不是以空 token 启动
	_, err := Load(write(t, "token = \"${DROIDPOOL_TOKEN}\"\n"+minimal[len("\ntoken = \"secret\"\n"):]))
	if err == nil {
		t.Fatal("环境变量未设置时 token 为空，应报错")
	}
}
