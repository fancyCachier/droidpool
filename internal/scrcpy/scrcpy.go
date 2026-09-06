// Package scrcpy 是 scrcpy 服务端的最小客户端：把设备画面以 H.264 取回来。
//
// 为什么不用 screencap：`adb exec-out screencap -p` 每帧要 350 ms，其中约 270 ms
// 是容器内 SurfaceFlinger 的 CPU 回读（软件渲染下尤其贵），剩下才是编码。
// scrcpy 让 MediaCodec 以 Surface 为输入，画面直接合成进编码器，CPU 完全不碰像素，
// 同一台 redroid 上实测 10.4 fps（screencap 路线只有 3 fps）。
//
// 协议是对 scrcpy 4.1 实测抓包确认的（见 docs/2026-09-03-远程操作方案对比.md）：
//
//	握手   dummy 字节 0x00 → 设备名 64 字节 → 视频头 16 字节
//	视频头 codec id 4 字节("h264") + 标志 4 字节 + 宽 4 字节 + 高 4 字节
//	每帧   帧头 12 字节（8 字节 pts+标志，4 字节负载长度）+ Annex-B 负载
package scrcpy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ServerVersion 必须与 ServerJar 指向的 jar 版本一致，服务端会校验。
const ServerVersion = "4.1"

// devicePath 是 jar 推到设备上的位置。用独立文件名，不和别人手动跑的 scrcpy 抢。
const devicePath = "/data/local/tmp/droidpool-scrcpy-server.jar"

// Frame 一个 H.264 访问单元（Annex-B）。
type Frame struct {
	// PTS 来自设备时钟。**不要拿它当播放时间戳**：实测高位会带上未公开的标志位，
	// 且我们是「收到即显示」的低延迟场景，时间戳由接收端自己生成更可靠。
	PTS      uint64
	Config   bool // 参数集（SPS/PPS），必须最先喂给解码器
	KeyFrame bool
	Data     []byte
}

// Options 会话参数。
type Options struct {
	Serial    string // adb 设备序列号，如 node-host:5561
	ADBPath   string // 默认 "adb"
	ServerJar string // 本机 scrcpy-server jar 路径
	LocalPort int    // adb forward 用的本机端口
	MaxFPS    int    // 0 = 不限
	BitRate   int    // 0 = 服务端默认
	MaxSize   int    // 0 = 原始分辨率
	// Log 非空时把设备端服务端的输出转记到这里，便于排查「连上了但没有帧」。
	Log *slog.Logger
}

// Session 一次投屏会话。
type Session struct {
	opt      Options
	scid     string
	cmd      *exec.Cmd
	video    net.Conn
	control  net.Conn
	Width    int
	Height   int
	Device   string
	closed   atomic.Bool
	forwards bool
}

func (o *Options) adb() string {
	if o.ADBPath != "" {
		return o.ADBPath
	}
	return "adb"
}

func (s *Session) adbCmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, s.opt.adb(), append([]string{"-s", s.opt.Serial}, args...)...)
}

// newSCID 生成会话 id。
// 服务端用 Integer.parseInt(scid, 16) 解析，超过 0x7fffffff 会抛 NumberFormatException
// 并直接退出——踩过这个坑，所以这里显式掩掉最高位。
func newSCID() string {
	return fmt.Sprintf("%08x", time.Now().UnixNano()&0x7fffffff)
}

// NewSession 用已建立的视频与控制连接构造会话，跳过推 jar、forward 与握手。
// 给测试和「连接由别处建立」的场景用；没有设备侧进程可探测，Alive 只看是否已 Close。
func NewSession(video, control net.Conn, width, height int) *Session {
	return &Session{video: video, control: control, Width: width, Height: height}
}

// Start 推服务端、建隧道、完成握手。返回后即可 ReadFrame。
func Start(ctx context.Context, opt Options) (*Session, error) {
	if opt.Serial == "" || opt.ServerJar == "" || opt.LocalPort == 0 {
		return nil, errors.New("Serial / ServerJar / LocalPort 都必填")
	}
	s := &Session{opt: opt, scid: newSCID()}

	// 起新会话前先清掉设备上残留的服务端。
	//
	// 一台设备同时只该有一个 scrcpy 服务端：它独占显示编码器，残留的那个会让新会话
	// 连上却永远收不到帧，页面就一直停在「等待首帧」的黑屏，而且重开多少次都一样。
	// 残留是常态而不是意外——droidpoold 一重启（每次部署都会），正在跑的 WebSocket
	// 连接是被 hijack 出去的，http.Server.Shutdown 不等它们，进程直接退出，
	// 设备侧的 app_process 就留在那里了。所以这里不做「假设上次清干净了」的假设。
	//
	// pkill 的退出码不能信：`pkill -f <pat>` 的自身命令行也含有该模式，会把自己
	// 一起杀掉（实测退出码 143）。只要发出去就行，杀没杀到看后续能否收到帧。
	_ = s.adbCmd(ctx, "shell", "pkill", "-f", "com.genymobile.scrcpy.Server").Run()

	// --sync：设备上已有同样新的 jar 就不传了，每次开放大页省掉 700 KB 的传输。
	// 设备复位会清掉 /data/local/tmp，届时自然会重传。
	if out, err := s.adbCmd(ctx, "push", "--sync", opt.ServerJar, devicePath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("推送 scrcpy-server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	port := strconv.Itoa(opt.LocalPort)
	_ = s.adbCmd(ctx, "forward", "--remove", "tcp:"+port).Run() // 清理上次残留
	if out, err := s.adbCmd(ctx, "forward", "tcp:"+port, "localabstract:scrcpy_"+s.scid).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("建立 adb forward: %w: %s", err, strings.TrimSpace(string(out)))
	}
	s.forwards = true

	args := []string{"shell", "CLASSPATH=" + devicePath, "app_process", "/",
		"com.genymobile.scrcpy.Server", ServerVersion,
		"scid=" + s.scid, "log_level=info", "audio=false", "tunnel_forward=true"}
	if opt.MaxFPS > 0 {
		args = append(args, "max_fps="+strconv.Itoa(opt.MaxFPS))
	}
	if opt.BitRate > 0 {
		args = append(args, "video_bit_rate="+strconv.Itoa(opt.BitRate))
	}
	if opt.MaxSize > 0 {
		args = append(args, "max_size="+strconv.Itoa(opt.MaxSize))
	}
	s.cmd = s.adbCmd(ctx, args...)
	// 设备端服务端自己的日志（log_level=info）走这条 adb shell 的 stdout/stderr。
	// 之前直接丢掉，结果它报错时我们这边只看到「连上了但没有帧」，无从归因。
	if opt.Log != nil {
		if out, err := s.cmd.StdoutPipe(); err == nil {
			go logLines(opt.Log, s.opt.Serial, "stdout", out)
		}
		if errp, err := s.cmd.StderrPipe(); err == nil {
			go logLines(opt.Log, s.opt.Serial, "stderr", errp)
		}
	}
	if err := s.cmd.Start(); err != nil {
		s.Close()
		return nil, fmt.Errorf("启动 scrcpy-server: %w", err)
	}

	if err := s.connect(ctx, port); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.handshake(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// connect 连视频与控制两个 socket。
//
// adb forward 在设备侧 localabstract 还没就绪时**也会接受 TCP 连接然后立刻关闭**，
// 所以必须重试到真的读到 dummy 字节为止——只判断 Dial 成功会拿到一个已死的连接。
// scrcpy 本身也是这么做的（它重试 100 次）。
func (s *Session) connect(ctx context.Context, port string) error {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		c, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err != nil {
			continue
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		if _, err := io.ReadFull(c, b[:]); err != nil {
			c.Close()
			continue
		}
		_ = c.SetReadDeadline(time.Time{})
		s.video = c
		break
	}
	if s.video == nil {
		return errors.New("视频 socket 一直连不上（服务端可能启动失败）")
	}
	// 控制 socket 是紧接着的第二个连接
	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		return fmt.Errorf("连接控制 socket: %w", err)
	}
	s.control = c
	return nil
}

func (s *Session) handshake() error {
	name := make([]byte, 64)
	if _, err := io.ReadFull(s.video, name); err != nil {
		return fmt.Errorf("读设备名: %w", err)
	}
	s.Device = trimNUL(name)

	head := make([]byte, 16)
	if _, err := io.ReadFull(s.video, head); err != nil {
		return fmt.Errorf("读视频头: %w", err)
	}
	if codec := string(head[0:4]); codec != "h264" {
		return fmt.Errorf("只支持 h264，设备给的是 %q", codec)
	}
	s.Width = int(binary.BigEndian.Uint32(head[8:12]))
	s.Height = int(binary.BigEndian.Uint32(head[12:16]))
	if s.Width <= 0 || s.Height <= 0 {
		return fmt.Errorf("视频头里的分辨率不合理: %dx%d", s.Width, s.Height)
	}
	return nil
}

const (
	flagConfig = uint64(1) << 63
	flagKey    = uint64(1) << 62
	// 实测 scrcpy 4.1 还会用到第 61 位（含义未公开），不掩掉的话它会泄进 PTS，
	// 让时间戳跳到 2.3e18 这种荒谬值。PTS 本身我们并不依赖（见 Frame.PTS 注释），
	// 但留着脏值会让排查时看到莫名其妙的数字。
	flagUnknown61 = uint64(1) << 61
	ptsMask       = ^(flagConfig | flagKey | flagUnknown61)
	// maxFrameBytes 防御性上限：解析错位时长度字段会变成天文数字，
	// 不设限会直接申请几个 GB 内存把进程打死。
	maxFrameBytes = 16 << 20
)

// ReadFrame 读下一帧。画面静止时 scrcpy 不出帧，这里会一直阻塞，
// 调用方需要靠 ctx 或 SetReadDeadline 控制。
func (s *Session) ReadFrame() (*Frame, error) {
	var h [12]byte
	if _, err := io.ReadFull(s.video, h[:]); err != nil {
		return nil, err
	}
	pf := binary.BigEndian.Uint64(h[0:8])
	size := binary.BigEndian.Uint32(h[8:12])
	if size == 0 || size > maxFrameBytes {
		return nil, fmt.Errorf("帧长 %d 不合理，流已错位", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(s.video, data); err != nil {
		return nil, err
	}
	return &Frame{
		PTS:      pf & ptsMask,
		Config:   pf&flagConfig != 0,
		KeyFrame: pf&flagKey != 0,
		Data:     data,
	}, nil
}

// Alive 报告设备侧 scrcpy 进程是否还在。
// 进程退出后 socket 未必立刻报错（adb 隧道会吞掉一段时间），靠它来兜底。
func (s *Session) Alive() bool {
	if s.closed.Load() {
		return false
	}
	if s.cmd == nil || s.cmd.Process == nil {
		// 不是 Start 拉起的进程（NewSession 传入的现成连接），没有进程可探，
		// 只要没 Close 就当活着
		return true
	}
	// ProcessState 只在 Wait 过之后才有；这里用信号 0 探测
	return s.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// SetReadDeadline 给读操作设超时，用于「画面静止时不要永远挂着」。
func (s *Session) SetReadDeadline(t time.Time) error {
	if s.video == nil {
		return errors.New("会话未连接")
	}
	return s.video.SetReadDeadline(t)
}

func (s *Session) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if s.video != nil {
		s.video.Close()
	}
	if s.control != nil {
		s.control.Close()
	}
	// 清理必须用独立的 ctx：调用方的 ctx 往往已经取消（HTTP 请求结束、
	// 被新会话接管），exec.CommandContext 在取消的 ctx 下根本不会执行，
	// 结果就是 forward 与设备侧进程全部泄漏——实测一次 reload 留下 3 个僵尸。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// 杀本机的 adb shell 只是断了管道，设备上的 app_process 不一定跟着退。
	// 用 scid 精确定位设备侧进程再杀，不误伤别的会话或别人手动跑的 scrcpy。
	if s.cmd != nil {
		_ = exec.CommandContext(ctx, s.opt.adb(), "-s", s.opt.Serial, "shell",
			"pkill", "-f", "scid="+s.scid).Run()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	if s.forwards {
		_ = exec.CommandContext(ctx, s.opt.adb(), "-s", s.opt.Serial,
			"forward", "--remove", "tcp:"+strconv.Itoa(s.opt.LocalPort)).Run()
	}
	return nil
}

func trimNUL(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// DeviceMessageType 是服务端经控制 socket 主动推来的消息类型。
type DeviceMessageType byte

const (
	DeviceMsgClipboard    DeviceMessageType = 0 // 设备剪贴板内容（GET_CLIPBOARD 的回应，或设备侧复制时自动推送）
	DeviceMsgAckClipboard DeviceMessageType = 1 // SET_CLIPBOARD 带 sequence 时的回执
	DeviceMsgUHIDOutput   DeviceMessageType = 2 // UHID 设备输出，我们不用
)

// DeviceMessage 一条设备消息。
type DeviceMessage struct {
	Type     DeviceMessageType
	Text     string // Clipboard
	Sequence uint64 // AckClipboard
}

// maxDeviceMessageBytes 与服务端 MESSAGE_MAX_SIZE 一致，超过即视为流错位。
const maxDeviceMessageBytes = 1 << 18

// ReadDeviceMessage 读一条设备消息，没有就阻塞。
//
// 控制 socket 是双向的：Controller 往里写输入，这里读服务端推回来的东西。
// 必须有人读——scrcpy 默认 clipboard_autosync=true，设备剪贴板一变就往 socket 里塞，
// 没人读缓冲区终会堵死，到时设备侧的发送线程会卡住。
func (s *Session) ReadDeviceMessage() (*DeviceMessage, error) {
	if s.control == nil {
		return nil, errors.New("控制 socket 未连接")
	}
	var t [1]byte
	if _, err := io.ReadFull(s.control, t[:]); err != nil {
		return nil, err
	}
	m := &DeviceMessage{Type: DeviceMessageType(t[0])}
	switch m.Type {
	case DeviceMsgClipboard:
		var l [4]byte
		if _, err := io.ReadFull(s.control, l[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint32(l[:])
		if n > maxDeviceMessageBytes {
			return nil, fmt.Errorf("剪贴板消息长 %d 不合理，控制流已错位", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(s.control, buf); err != nil {
			return nil, err
		}
		m.Text = string(buf)
	case DeviceMsgAckClipboard:
		var b [8]byte
		if _, err := io.ReadFull(s.control, b[:]); err != nil {
			return nil, err
		}
		m.Sequence = binary.BigEndian.Uint64(b[:])
	case DeviceMsgUHIDOutput:
		var h [4]byte // id 2 字节 + 长度 2 字节
		if _, err := io.ReadFull(s.control, h[:]); err != nil {
			return nil, err
		}
		if _, err := io.CopyN(io.Discard, s.control, int64(binary.BigEndian.Uint16(h[2:4]))); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("未知设备消息类型 %d，控制流已错位", t[0])
	}
	return m, nil
}

// logLines 把设备端服务端的输出按行转记，丢掉 scrcpy 每次都打的横幅噪声。
func logLines(log *slog.Logger, serial, stream string, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "[server] INFO: Device:") {
			continue
		}
		log.Info("scrcpy 服务端", "device", serial, "流", stream, "行", line)
	}
}
