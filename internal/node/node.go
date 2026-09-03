// Package node 通过 docker over SSH 管理节点上的 redroid 容器。
// 控制面跑在 devopt，节点（.54）上除 docker 外不装任何东西——节点坏了重装成本最低。
package node

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner 执行一条命令并返回其标准输出。抽出接口是为了测试能替换掉真实的 docker/ssh 调用。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner 真实执行。
type ExecRunner struct{ Env []string }

func (r ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(cmd.Environ(), r.Env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Node 一个设备节点。
type Node struct {
	Name       string
	DockerHost string // ssh://sa@192.168.14.54
	ADBHost    string
	Image      string
	DataRoot   string
	BootArgs   string
	Runner     Runner
}

func (n *Node) docker(ctx context.Context, args ...string) (string, error) {
	r := n.Runner
	if r == nil {
		r = ExecRunner{}
	}
	if er, ok := r.(ExecRunner); ok {
		er.Env = append(er.Env, "DOCKER_HOST="+n.DockerHost)
		r = er
	}
	return r.Run(ctx, "docker", args...)
}

// ContainerName 设备 id 对应的容器名。
func ContainerName(deviceID string) string { return "droidpool-" + deviceID }

// Create 起一个容器。overlayBase 非空时用 redroid 原生 overlayfs 共享 data
// （/data-base 只读基底 + /data-diff 实例私有），复位即删 diff，零拷贝；
// 空则退回独立 /data 挂载。
func (n *Node) Create(ctx context.Context, deviceID string, port int, overlayBase string) error {
	name := ContainerName(deviceID)
	_, _ = n.docker(ctx, "rm", "-f", name) // 忽略「不存在」
	args := []string{"run", "-d", "--privileged", "--name", name,
		"-p", strconv.Itoa(port) + ":5555"}
	if overlayBase != "" {
		args = append(args,
			"-v", overlayBase+":/data-base",
			"-v", n.DataRoot+"/diff/"+deviceID+":/data-diff")
	} else {
		args = append(args, "-v", n.DataRoot+"/data/"+deviceID+":/data")
	}
	args = append(args, n.Image)
	args = append(args, strings.Fields(n.BootArgs)...)
	_, err := n.docker(ctx, args...)
	return err
}

// Remove 删除容器。
func (n *Node) Remove(ctx context.Context, deviceID string) error {
	_, err := n.docker(ctx, "rm", "-f", ContainerName(deviceID))
	return err
}

// WaitBoot 轮询到 sys.boot_completed=1。Phase 1 实测约 11~13 s。
func (n *Node) WaitBoot(ctx context.Context, deviceID string, timeout time.Duration) error {
	name := ContainerName(deviceID)
	deadline := time.Now().Add(timeout)
	for {
		out, err := n.docker(ctx, "exec", name, "getprop", "sys.boot_completed")
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("设备 %s 在 %s 内未启动完成", deviceID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Running 列出节点上正在运行的 droidpool 容器名。
func (n *Node) Running(ctx context.Context) ([]string, error) {
	out, err := n.docker(ctx, "ps", "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "droidpool-") {
			names = append(names, line)
		}
	}
	return names, nil
}

// Health 节点资源快照。
type Health struct {
	MemTotalMiB int
	MemAvailMiB int
	SwapUsedMiB int
	TempC       float64
	Load1       float64
}

// Health 读节点内存 / swap / 温度 / 负载。
// swap 是准入的关键指标：Phase 1 实测 swap 从 0 变正就是性能断崖（N=12 起
// 失败率 5.6%、p95 冲到 2.66×），而 CPU 早在 N=4 就饱和、温度全程 < 70 °C，
// 这两者都没有预警价值。
func (n *Node) Health(ctx context.Context) (*Health, error) {
	r := n.Runner
	if r == nil {
		r = ExecRunner{}
	}
	sshTarget := strings.TrimPrefix(n.DockerHost, "ssh://")
	out, err := r.Run(ctx, "ssh", "-o", "BatchMode=yes", sshTarget,
		`free -m | awk 'NR==2{print $2, $7} NR==3{print $3}'; `+
			`cat /sys/class/thermal/thermal_zone0/temp; cut -d" " -f1 /proc/loadavg`)
	if err != nil {
		return nil, err
	}
	return parseHealth(out)
}

// parseHealth 解析 Health 的四行输出：总内存+可用、swap 已用、温度毫度、1 分钟负载。
func parseHealth(out string) (*Health, error) {
	var fields []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields = append(fields, strings.Fields(line)...)
	}
	if len(fields) < 5 {
		return nil, fmt.Errorf("节点健康输出格式不符，得到 %q", out)
	}
	atoi := func(s string) int { v, _ := strconv.Atoi(s); return v }
	milli, _ := strconv.ParseFloat(fields[3], 64)
	load, _ := strconv.ParseFloat(fields[4], 64)
	return &Health{
		MemTotalMiB: atoi(fields[0]),
		MemAvailMiB: atoi(fields[1]),
		SwapUsedMiB: atoi(fields[2]),
		TempC:       milli / 1000,
		Load1:       load,
	}, nil
}

// UnderPressure 报告节点是否已进入换页状态，是则应拒绝新的 claim。
func (h *Health) UnderPressure(swapGuardMiB int) bool {
	return h.SwapUsedMiB > swapGuardMiB
}
