package scrcpy

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// pipeSession 造一个只带 video 连接的会话，用内存管道喂协议字节，
// 不启动真实设备也能测解析逻辑。
func pipeSession(t *testing.T) (*Session, net.Conn) {
	t.Helper()
	client, server := net.Pipe()
	s := &Session{video: client}
	t.Cleanup(func() { client.Close(); server.Close() })
	return s, server
}

func TestNewSCIDFitsSignedInt32(t *testing.T) {
	// 服务端用 Integer.parseInt(scid, 16)：超过 0x7fffffff 会抛
	// NumberFormatException 并直接退出，表现为「socket 连上就 EOF」，极难定位。
	for i := 0; i < 200; i++ {
		id := newSCID()
		if len(id) != 8 {
			t.Fatalf("scid 应为 8 位十六进制，得到 %q", id)
		}
		v, err := strconv.ParseInt(id, 16, 64)
		if err != nil {
			t.Fatalf("scid %q 不是合法十六进制: %v", id, err)
		}
		if v > 0x7fffffff || v < 0 {
			t.Fatalf("scid %q = %d 超出 int32 正范围，服务端会拒绝", id, v)
		}
		time.Sleep(time.Microsecond)
	}
}

func TestHandshakeParsesNameAndSize(t *testing.T) {
	s, srv := pipeSession(t)
	go func() {
		name := make([]byte, 64)
		copy(name, "redroid14_arm64_only")
		srv.Write(name)
		head := make([]byte, 16)
		copy(head[0:4], "h264")
		binary.BigEndian.PutUint32(head[4:8], 0x80000000)
		binary.BigEndian.PutUint32(head[8:12], 1366)
		binary.BigEndian.PutUint32(head[12:16], 768)
		srv.Write(head)
	}()
	if err := s.handshake(); err != nil {
		t.Fatal(err)
	}
	if s.Device != "redroid14_arm64_only" {
		t.Errorf("设备名 = %q", s.Device)
	}
	// 宽高在视频头的 8~16 字节，不是紧跟 codec id —— 这是实测确认的布局，
	// 按 12 字节头解析会把 0x80000000 当成宽度
	if s.Width != 1366 || s.Height != 768 {
		t.Errorf("分辨率 = %dx%d，期望 1366x768", s.Width, s.Height)
	}
}

func TestHandshakeRejectsNonH264(t *testing.T) {
	s, srv := pipeSession(t)
	go func() {
		srv.Write(make([]byte, 64))
		head := make([]byte, 16)
		copy(head[0:4], "av1 ")
		binary.BigEndian.PutUint32(head[8:12], 1366)
		binary.BigEndian.PutUint32(head[12:16], 768)
		srv.Write(head)
	}()
	if err := s.handshake(); err == nil {
		t.Error("非 h264 应报错——浏览器侧只接了 h264 解码器")
	}
}

func TestHandshakeRejectsBogusSize(t *testing.T) {
	s, srv := pipeSession(t)
	go func() {
		srv.Write(make([]byte, 64))
		head := make([]byte, 16)
		copy(head[0:4], "h264") // 宽高全 0
		srv.Write(head)
	}()
	if err := s.handshake(); err == nil {
		t.Error("分辨率为 0 应报错，否则后续会按 0 尺寸建解码器")
	}
}

func writeFrame(w io.Writer, pts uint64, config, key bool, payload []byte) {
	var h [12]byte
	pf := pts
	if config {
		pf |= flagConfig
	}
	if key {
		pf |= flagKey
	}
	binary.BigEndian.PutUint64(h[0:8], pf)
	binary.BigEndian.PutUint32(h[8:12], uint32(len(payload)))
	w.Write(h[:])
	w.Write(payload)
}

func TestReadFrameFlagsAndPayload(t *testing.T) {
	s, srv := pipeSession(t)
	sps := []byte{0, 0, 0, 1, 0x67, 0x42, 0xc0}
	idr := []byte{0, 0, 0, 1, 0x65, 0xb8}
	go func() {
		writeFrame(srv, 1366, true, false, sps)
		writeFrame(srv, 999, false, true, idr)
		writeFrame(srv, 1200, false, false, []byte{0, 0, 0, 1, 0x41})
	}()

	f1, err := s.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if !f1.Config {
		t.Error("第一帧应标记为参数集：解码器必须先吃到 SPS/PPS 才能解后续帧")
	}
	if f1.KeyFrame {
		t.Error("参数集帧不该同时被标为关键帧")
	}
	if f1.PTS != 1366 {
		t.Errorf("PTS = %d，标志位应已从 PTS 中剥离", f1.PTS)
	}
	if string(f1.Data) != string(sps) {
		t.Errorf("负载不符: % x", f1.Data)
	}

	f2, _ := s.ReadFrame()
	if !f2.KeyFrame || f2.Config {
		t.Errorf("第二帧应是关键帧而非参数集: key=%v config=%v", f2.KeyFrame, f2.Config)
	}
	f3, _ := s.ReadFrame()
	if f3.KeyFrame || f3.Config {
		t.Errorf("第三帧应是普通帧: key=%v config=%v", f3.KeyFrame, f3.Config)
	}
}

// 流一旦错位，长度字段会变成天文数字。不设上限会直接申请几个 GB 内存把进程打死，
// 而这在生产上表现为「控制面莫名其妙被 OOM」，极难归因。
func TestReadFrameRejectsAbsurdLength(t *testing.T) {
	s, srv := pipeSession(t)
	go func() {
		var h [12]byte
		binary.BigEndian.PutUint32(h[8:12], 0xFFFFFFF0)
		srv.Write(h[:])
	}()
	if _, err := s.ReadFrame(); err == nil {
		t.Fatal("超大帧长应报错而不是照单全收")
	}
}

func TestReadFrameRejectsZeroLength(t *testing.T) {
	s, srv := pipeSession(t)
	go func() {
		var h [12]byte // 长度 0
		srv.Write(h[:])
	}()
	if _, err := s.ReadFrame(); err == nil {
		t.Error("零长帧说明解析已错位，应报错")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := &Session{}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 重复 Close 不能 panic（HTTP handler 的 defer 与显式关闭会同时发生）
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartValidatesOptions(t *testing.T) {
	cases := []Options{
		{},
		{Serial: "x"},
		{Serial: "x", ServerJar: "/tmp/j"},
	}
	for _, o := range cases {
		if _, err := Start(t.Context(), o); err == nil {
			t.Errorf("缺必填项应报错: %+v", o)
		}
	}
}

func TestTrimNUL(t *testing.T) {
	b := make([]byte, 64)
	copy(b, "abc")
	if got := trimNUL(b); got != "abc" {
		t.Errorf("trimNUL = %q", got)
	}
	if got := trimNUL([]byte("nonul")); got != "nonul" {
		t.Errorf("无 NUL 时应返回全部内容，得到 %q", got)
	}
}

// 实测 scrcpy 4.1 会用到第 61 位标志。不掩掉它，PTS 会变成 2.3e18 这种荒谬值。
func TestReadFrameMasksUnknownHighFlag(t *testing.T) {
	s, srv := pipeSession(t)
	const realPTS = 18969707301
	go func() {
		var h [12]byte
		binary.BigEndian.PutUint64(h[0:8], realPTS|flagUnknown61)
		binary.BigEndian.PutUint32(h[8:12], 3)
		srv.Write(h[:])
		srv.Write([]byte{1, 2, 3})
	}()
	f, err := s.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if f.PTS != realPTS {
		t.Errorf("PTS = %d，未知标志位应被掩掉，期望 %d", f.PTS, realPTS)
	}
}
