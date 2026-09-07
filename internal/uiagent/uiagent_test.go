package uiagent

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDevice 在本机起一个说同样协议的服务端，冒充经 adb forward 过来的设备侧 agent。
// 这样能真跑通拨号、发指令、读一行这条完整路径，而不用碰真设备。
func fakeDevice(t *testing.T, dumpReply string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				for sc.Scan() {
					switch strings.TrimSpace(sc.Text()) {
					case "PING":
						c.Write([]byte("PONG\n"))
					case "DUMP":
						c.Write([]byte(dumpReply + "\n"))
					default:
						c.Write([]byte("ERR 未知指令\n"))
					}
				}
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// fakeADB 造一个只会成功返回的假 adb，挡住 push / forward / app_process。
func fakeADB(t *testing.T) (adbPath, dexPath string) {
	t.Helper()
	dir := t.TempDir()
	adbPath = filepath.Join(dir, "adb")
	// 记下每次调用，好断言「复用时没去推 dex / 启动进程」
	logPath := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(adbPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UIAGENT_TEST_ADBLOG", logPath)
	dexPath = filepath.Join(dir, "uiagent.dex")
	if err := os.WriteFile(dexPath, []byte("dex"), 0o644); err != nil {
		t.Fatal(err)
	}
	return adbPath, dexPath
}

func TestStartDumpAndClose(t *testing.T) {
	const xml = `<?xml version='1.0'?><hierarchy><node class="X"/></hierarchy>`
	port := fakeDevice(t, xml)
	adbPath, dexPath := fakeADB(t)

	s, err := Start(context.Background(), Options{
		Serial: "host:5561", ADBPath: adbPath, DexPath: dexPath, LocalPort: port,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	got, err := s.Dump()
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if got != xml {
		t.Errorf("Dump = %q，期望 %q", got, xml)
	}
}

func TestStartRequiresFields(t *testing.T) {
	for _, o := range []Options{
		{DexPath: "d", LocalPort: 1},
		{Serial: "s", LocalPort: 1},
		{Serial: "s", DexPath: "d"},
	} {
		if _, err := Start(context.Background(), o); err == nil {
			t.Errorf("%+v 应当报参数缺失", o)
		}
	}
}

// 设备侧报错要原样带出来，不能吞掉当成正常 XML —— 吞掉的话调用方会拿着
// "ERR ..." 当界面树去解析，错得很隐蔽。
func TestDumpSurfacesDeviceError(t *testing.T) {
	// 让假设备对 DUMP 直接回 ERR，走的就是 Dump() 自己的判断分支
	port := fakeDevice(t, "ERR UiAutomation 连接已断")
	adbPath, dexPath := fakeADB(t)
	s, err := Start(context.Background(), Options{
		Serial: "s", ADBPath: adbPath, DexPath: dexPath, LocalPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.Dump()
	if err == nil {
		t.Fatalf("设备回 ERR 时 Dump 必须报错，实际返回了 %q", got)
	}
	if !strings.Contains(err.Error(), "UiAutomation 连接已断") {
		t.Errorf("错误里应当带上设备侧原因，实际: %v", err)
	}
}

// 一行响应可能远超 bufio 默认缓冲，长树不能被截断。
func TestDumpHandlesLongLine(t *testing.T) {
	long := "<hierarchy>" + strings.Repeat("<node class=\"a\"/>", 40000) + "</hierarchy>"
	port := fakeDevice(t, long)
	adbPath, dexPath := fakeADB(t)
	s, err := Start(context.Background(), Options{
		Serial: "s", ADBPath: adbPath, DexPath: dexPath, LocalPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.Dump()
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if len(got) != len(long) {
		t.Errorf("长响应被截断：拿到 %d 字节，期望 %d", len(got), len(long))
	}
}

func TestReadLimitedLineRejectsOversize(t *testing.T) {
	c1, c2 := net.Pipe()
	go func() {
		c2.Write([]byte(strings.Repeat("x", 1024)))
		c2.Close()
	}()
	defer c1.Close()
	if _, err := readLimitedLine(bufio.NewReaderSize(c1, 64), 100); err == nil {
		t.Error("超过上限应当报错，而不是一直吃内存")
	}
}

func TestStartFailsWhenDeviceNeverReady(t *testing.T) {
	adbPath, dexPath := fakeADB(t)
	// 指向一个没人监听的端口
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消，免得测试真等 15 秒
	_, err := Start(ctx, Options{Serial: "s", ADBPath: adbPath, DexPath: dexPath, LocalPort: port})
	if err == nil {
		t.Fatal("连不上时 Start 必须报错，不能返回一个用不了的会话")
	}
}

func adbCalls(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("UIAGENT_TEST_ADBLOG"))
	if err != nil {
		return ""
	}
	return string(b)
}

// 设备上已有 agent 在应答时，Start 不该再推 dex 或拉进程——那正是省下
// 1~2 s 冷启动的地方。复用失效的话性能悄悄退回去，测试之外看不出来。
func TestStartReusesRunningAgent(t *testing.T) {
	port := fakeDevice(t, "<hierarchy/>")
	adbPath, dexPath := fakeADB(t)

	s, err := Start(context.Background(), Options{
		Serial: "s", ADBPath: adbPath, DexPath: dexPath, LocalPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if !s.Reused() {
		t.Error("假设备一直在应答 PING，应当判定为复用")
	}
	calls := adbCalls(t)
	if strings.Contains(calls, "push") {
		t.Errorf("复用时不该推 dex：\n%s", calls)
	}
	if strings.Contains(calls, "app_process") {
		t.Errorf("复用时不该再拉起进程：\n%s", calls)
	}
	if !strings.Contains(calls, "forward") {
		t.Errorf("仍然要建 forward：\n%s", calls)
	}
}

// Close 不能杀设备侧 agent，否则下次调用又要付冷启动。
func TestCloseLeavesDeviceAgentRunning(t *testing.T) {
	port := fakeDevice(t, "<hierarchy/>")
	adbPath, dexPath := fakeADB(t)
	s, err := Start(context.Background(), Options{
		Serial: "s", ADBPath: adbPath, DexPath: dexPath, LocalPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := adbCalls(t)
	_ = s.Close()
	added := strings.TrimPrefix(adbCalls(t), before)
	if strings.Contains(added, "pkill") {
		t.Errorf("Close 不该杀设备侧 agent：%s", added)
	}
	if !strings.Contains(added, "forward --remove") {
		t.Errorf("Close 应当撤掉 forward：%s", added)
	}
}

func TestStopKillsDeviceAgent(t *testing.T) {
	port := fakeDevice(t, "<hierarchy/>")
	adbPath, dexPath := fakeADB(t)
	s, err := Start(context.Background(), Options{
		Serial: "s", ADBPath: adbPath, DexPath: dexPath, LocalPort: port,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := adbCalls(t)
	_ = s.Stop(context.Background())
	added := strings.TrimPrefix(adbCalls(t), before)
	if !strings.Contains(added, "pkill") {
		t.Errorf("Stop 应当收掉设备侧 agent：%s", added)
	}
}
