// Package uiagent 驱动设备上的常驻 UI dump agent。
//
// 为什么不用 `uiautomator dump`：它每次调用都要新起一个 ART 进程再加载框架 jar、
// 连接 AccessibilityService。2026-09-07 在 3588-a-8 实测，热设备上一次 dump 要
// 321~622 ms（中位数约 380 ms），而吐出来的树只有 27 个节点 / 6.8 KB——耗时几乎
// 全在启动上，遍历本身可以忽略。一个 login_flow 要十几次 dump，这是 agent 驱动
// 设备的主要开销。
//
// 这里把进程和 UiAutomation 连接留住：同样的设备上实测降到 14~43 ms（中位数约
// 25 ms），约 15 倍。设备侧代码在 device/uiagent/，用法与 scrcpy-server 一致，
// adb push 上去再由 app_process 拉起。
package uiagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// devicePath dex 推到设备上的位置。独立文件名，不和别人手动放的东西撞。
const devicePath = "/data/local/tmp/droidpool-uiagent.dex"

// mainClass 与 device/uiagent 里的包名一致，改一处两边都要改。
const mainClass = "com.daboshi.droidpool.UiAgent"

// devicePort agent 在设备上监听的端口，绑 loopback，靠 adb forward 进去。
const devicePort = 27400

// deviceLog agent 脱离启动后的输出落在这里，排查「起来了但连不上」时看它。
const deviceLog = "/data/local/tmp/droidpool-uiagent.log"

// maxDumpBytes 一次 dump 的上限。协议是一行一条响应，没有长度前缀，
// 加个上限免得设备侧出问题时把控制面的内存吃光。
const maxDumpBytes = 8 << 20

type Options struct {
	Serial    string // adb 设备序列号，如 node-host:5561
	ADBPath   string // 默认 "adb"
	DexPath   string // 本机 uiagent.dex 路径
	LocalPort int    // adb forward 用的本机端口
}

// Session 一个已就绪的 agent 连接通道。
type Session struct {
	opt    Options
	reused bool // 连上的是设备上已在跑的 agent，本次没有启动它
}

// Reused 报告本次是否复用了已在跑的 agent（没有付启动代价）。
func (s *Session) Reused() bool { return s.reused }

func (o *Options) adb() string {
	if o.ADBPath != "" {
		return o.ADBPath
	}
	return "adb"
}

func (s *Session) adbCmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, s.opt.adb(), append([]string{"-s", s.opt.Serial}, args...)...)
}

// Start 让设备上的 agent 就绪并建好通道，返回后即可 Dump。
//
// 先建 forward 再 PING：agent 会脱离 adb shell 常驻在设备上（实测 nohup
// 启动后 adb shell 退出，agent 仍在监听并正常应答），所以能复用就不重启。
// 复用时整个 Start 只有一次 forward 加一次 PING，几十毫秒；冷启动要推 dex、
// 拉起 ART 进程、等 UiAutomation 连上，约 1~2 s。login_flow 那种连着取十几次
// 的用法，差别全在这里。
func Start(ctx context.Context, opt Options) (*Session, error) {
	if opt.Serial == "" || opt.DexPath == "" || opt.LocalPort == 0 {
		return nil, errors.New("Serial / DexPath / LocalPort 都必填")
	}
	s := &Session{opt: opt}
	if err := s.forward(ctx); err != nil {
		return nil, err
	}
	if s.pingOK() {
		s.reused = true
		return s, nil
	}

	// 没人应答：可能是没起过，也可能是上一个卡死了还占着端口——后者会让新的
	// 绑定失败，症状是「起来了但连不上」，所以无论如何先清一遍。pkill 的退出码
	// 不可信（它的模式会匹配到自己），只管发不管结果。
	_ = s.adbCmd(ctx, "shell", "pkill", "-f", mainClass).Run()

	if out, err := s.adbCmd(ctx, "push", "--sync", opt.DexPath, devicePath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("推送 uiagent.dex: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// nohup 脱离：agent 要活过这条 adb shell，下次调用才能复用。
	start := fmt.Sprintf("CLASSPATH=%s nohup app_process / %s port=%d > %s 2>&1 &",
		devicePath, mainClass, devicePort, deviceLog)
	if out, err := s.adbCmd(ctx, "shell", start).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("启动 uiagent: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := s.waitReady(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// forward 建 adb forward，幂等。
func (s *Session) forward(ctx context.Context) error {
	port := strconv.Itoa(s.opt.LocalPort)
	_ = s.adbCmd(ctx, "forward", "--remove", "tcp:"+port).Run()
	if out, err := s.adbCmd(ctx, "forward", "tcp:"+port, "tcp:"+strconv.Itoa(devicePort)).CombinedOutput(); err != nil {
		return fmt.Errorf("建立 adb forward: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Session) pingOK() bool {
	out, err := s.request("PING")
	return err == nil && strings.TrimSpace(out) == "PONG"
}

// waitReady 轮询到 PING 有应答。设备侧要建 UiAutomation 连接，不是瞬时的。
func (s *Session) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(15 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		out, err := s.request("PING")
		if err == nil && strings.TrimSpace(out) == "PONG" {
			return nil
		}
		if err != nil {
			last = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("uiagent 15 s 内未就绪（设备上看 %s）: %w", deviceLog, last)
}

// Dump 取一次界面层级 XML。
func (s *Session) Dump() (string, error) {
	out, err := s.request("DUMP")
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(out, "ERR ") {
		return "", errors.New("设备侧 dump 失败: " + strings.TrimPrefix(out, "ERR "))
	}
	return out, nil
}

// request 发一条指令读一行响应。每次新建连接：dump 是毫秒级，
// 复用长连接省不下什么，却要处理半开连接与超时状态，不划算。
func (s *Session) request(cmd string) (string, error) {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(s.opt.LocalPort), 3*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write([]byte(cmd + "\n")); err != nil {
		return "", err
	}
	return readLimitedLine(bufio.NewReaderSize(c, 64<<10), maxDumpBytes)
}

// readLimitedLine 读一行，超过 limit 就报错而不是一直吃内存。
func readLimitedLine(r *bufio.Reader, limit int) (string, error) {
	var sb strings.Builder
	for {
		chunk, more, err := r.ReadLine()
		if err != nil {
			return "", err
		}
		if sb.Len()+len(chunk) > limit {
			return "", fmt.Errorf("响应超过 %d 字节，疑似流错位", limit)
		}
		sb.Write(chunk)
		if !more {
			return sb.String(), nil
		}
	}
}

// Close 只释放本机这端的 forward，**不动设备上的 agent**——留着它，
// 下次 Start 直接复用，省掉 1~2 s 冷启动。设备复位会重建容器，自然清干净。
func (s *Session) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.adbCmd(ctx, "forward", "--remove", "tcp:"+strconv.Itoa(s.opt.LocalPort)).Run()
	return nil
}

// Stop 连设备上的 agent 一起收掉。给「确实要腾干净」的场合用。
func (s *Session) Stop(ctx context.Context) error {
	_ = s.adbCmd(ctx, "shell", "pkill", "-f", mainClass).Run()
	return s.Close()
}
