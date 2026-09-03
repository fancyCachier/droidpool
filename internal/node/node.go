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

// WipeData 清空设备的数据目录，这是「复位」真正生效的那一步。
//
// 只删容器再重建**不等于复位**：/data-diff（或 /data）挂在宿主目录上，
// 容器没了目录还在，上一个 agent 装的包与登录态会原样留给下一个人。
// 专项 C 实测该目录里大量文件属主是 root（Android 各 uid），
// 普通用户 rm 会一路 Permission denied 只删掉一部分——留下一个「看起来复位了
// 其实没干净」的状态，比不复位更危险。因此这里用一次性特权容器来删：
// 不依赖宿主的 sudo 配置，删除动作和创建动作走同一条 docker 权限通道。
func (n *Node) WipeData(ctx context.Context, deviceID, overlayBase string) error {
	dir := n.DataRoot + "/data/" + deviceID
	if overlayBase != "" {
		dir = n.DataRoot + "/diff/" + deviceID
	}
	// busybox 足够小，且 redroid 镜像已在本地时也可换成它；这里显式用 busybox
	// 保持与业务镜像解耦。挂父目录再删子目录，避免删除挂载点本身。
	_, err := n.docker(ctx, "run", "--rm",
		"-v", dir+":/wipe",
		"busybox:stable", "sh", "-c", "rm -rf /wipe/* /wipe/.[!.]* 2>/dev/null; true")
	if err != nil {
		return fmt.Errorf("清空 %s: %w", dir, err)
	}
	return nil
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

// RunningSet 返回节点上正在运行的 droidpool 容器名集合。
func (n *Node) RunningSet(ctx context.Context) (map[string]bool, error) {
	names, err := n.Running(ctx)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(names))
	for _, nm := range names {
		set[nm] = true
	}
	return set, nil
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

// UnderPressure 报告节点是否已没有余量再放一台设备进来。
//
// 判据是**可用内存**，不是 swap。Phase 1 里 swap 与失败率同时出现，一度让人
// 以为 swap 是准入指标，但 swap_used 是滞后且黏滞的症状：一旦换出，页面不会
// 自己换回来，压测结束几小时后读数依然很高，闸门会在盒子空着时一直关着
// （实测残留 426 MiB，而当时可用内存有 10.9 GB）。
// 真正的因果变量是「还装不装得下一台设备」——实测每台常驻约 1 GB，
// 因此 minAvailMiB 取一台的量再留些余量。swap 仍然值得显示，但不做闸。
func (h *Health) UnderPressure(minAvailMiB int) bool {
	if minAvailMiB <= 0 {
		return false
	}
	return h.MemAvailMiB < minAvailMiB
}

// MakeGolden 生成 overlay 共享基底 /data-base。
//
// 路线图 §5.5：以普通 -v 挂载起一台裸容器，做完系统级设置后停掉，该目录即 base。
// 之后所有设备以只读基底挂它，复位 = 删 diff，零拷贝。
// **base 不预装 app**：多宿主机 debug keystore 不同，预装会让 `install -r` 报签名冲突。
// 幂等：base 已有内容时直接返回，想重做先手动清空目录。
func (n *Node) MakeGolden(ctx context.Context, baseDir string, port int) error {
	// 已有内容就不重做：boot 过一次的 /data 里必然有 system/ 目录
	if out, err := n.sshRun(ctx, "test -d "+baseDir+"/system && echo yes || echo no"); err == nil && strings.TrimSpace(out) == "yes" {
		return nil
	}
	const name = "droidpool-golden"
	_, _ = n.docker(ctx, "rm", "-f", name)
	args := []string{"run", "-d", "--privileged", "--name", name,
		"-p", strconv.Itoa(port) + ":5555", "-v", baseDir + ":/data", n.Image}
	args = append(args, strings.Fields(n.BootArgs)...)
	// overlay 参数在造 base 时不能带：base 本身就是要写进去的普通 /data
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "androidboot.use_redroid_overlayfs") {
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}
	if _, err := n.docker(ctx, args...); err != nil {
		return fmt.Errorf("起 golden 容器: %w", err)
	}
	defer n.docker(context.Background(), "rm", "-f", name)

	if err := n.waitBootContainer(ctx, name, 90*time.Second); err != nil {
		return err
	}
	// 系统级设置：关动画（截图与驱动都更稳）、常亮、中文与时区、关验证器
	for _, cmd := range []string{
		"settings put global window_animation_scale 0",
		"settings put global transition_animation_scale 0",
		"settings put global animator_duration_scale 0",
		"svc power stayon true",
		"setprop persist.sys.locale zh-CN",
		"setprop persist.sys.timezone Asia/Shanghai",
		"settings put global package_verifier_enable 0",
	} {
		if _, err := n.docker(ctx, "exec", name, "sh", "-c", cmd); err != nil {
			return fmt.Errorf("golden 设置 %q: %w", cmd, err)
		}
	}
	// 让设置落盘再停
	_, _ = n.docker(ctx, "exec", name, "sync")
	if _, err := n.docker(ctx, "stop", "-t", "10", name); err != nil {
		return fmt.Errorf("停 golden 容器: %w", err)
	}
	return nil
}

// waitBootContainer 与 WaitBoot 相同但按容器名等待。
func (n *Node) waitBootContainer(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := n.docker(ctx, "exec", name, "getprop", "sys.boot_completed")
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("容器 %s 在 %s 内未启动完成", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// sshRun 在节点宿主上跑一条 shell（不进容器）。
func (n *Node) sshRun(ctx context.Context, cmd string) (string, error) {
	r := n.Runner
	if r == nil {
		r = ExecRunner{}
	}
	return r.Run(ctx, "ssh", "-o", "BatchMode=yes", strings.TrimPrefix(n.DockerHost, "ssh://"), cmd)
}

// Reconcile 清掉节点上不在 keep 里的 droidpool- 容器（守护进程重启后的孤儿），
// 以及**任何占用了池端口的非 droidpool 容器**——基线测试留下的 redroid-N 会让
// 端口冲突，整个池一台都起不来。
func (n *Node) Reconcile(ctx context.Context, keep map[string]bool, ports []int) (removed []string, err error) {
	out, err := n.docker(ctx, "ps", "-a", "--format", "{{.Names}}\t{{.Ports}}")
	if err != nil {
		return nil, err
	}
	portSet := map[string]bool{}
	for _, p := range ports {
		portSet[":"+strconv.Itoa(p)+"->"] = true
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if parts[0] == "" {
			continue
		}
		name := parts[0]
		portsCol := ""
		if len(parts) > 1 {
			portsCol = parts[1]
		}
		stale := strings.HasPrefix(name, "droidpool-") && !keep[name] && name != "droidpool-golden"
		hogging := false
		for p := range portSet {
			if strings.Contains(portsCol, p) && !keep[name] {
				hogging = true
			}
		}
		if stale || hogging {
			if _, err := n.docker(ctx, "rm", "-f", name); err == nil {
				removed = append(removed, name)
			}
		}
	}
	return removed, nil
}
