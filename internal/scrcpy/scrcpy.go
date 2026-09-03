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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
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
	Serial    string // adb 设备序列号，如 192.168.14.54:5561
	ADBPath   string // 默认 "adb"
	ServerJar string // 本机 scrcpy-server jar 路径
	LocalPort int    // adb forward 用的本机端口
	MaxFPS    int    // 0 = 不限
	BitRate   int    // 0 = 服务端默认
	MaxSize   int    // 0 = 原始分辨率
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

// Start 推服务端、建隧道、完成握手。返回后即可 ReadFrame。
func Start(ctx context.Context, opt Options) (*Session, error) {
	if opt.Serial == "" || opt.ServerJar == "" || opt.LocalPort == 0 {
		return nil, errors.New("Serial / ServerJar / LocalPort 都必填")
	}
	s := &Session{opt: opt, scid: newSCID()}

	if out, err := s.adbCmd(ctx, "push", opt.ServerJar, devicePath).CombinedOutput(); err != nil {
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
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	if s.forwards {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.adbCmd(ctx, "forward", "--remove", "tcp:"+strconv.Itoa(s.opt.LocalPort)).Run()
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
