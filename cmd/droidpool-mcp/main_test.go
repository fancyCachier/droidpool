package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpText = mcp.TextContent

// fakeAPI 记录设备接口收到的请求并按 reply 应答。
func fakeAPI(t *testing.T, wantPath string, reply any) (*client, *map[string]string) {
	t.Helper()
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != wantPath {
			t.Errorf("请求打错了地方: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("没带 token: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return &client{base: srv.URL, token: "tok", http: srv.Client()}, &got
}

func TestIdentitySendsFieldsAndReportsRebuild(t *testing.T) {
	c, got := fakeAPI(t, "/api/devices/d1/identity", map[string]any{
		"device": "d1", "rebuilt": true,
		"identity": map[string]string{"model": "X1", "brand": "ACME", "manufacturer": "ACME", "device": "x1", "name": "x1", "serial": "DPD1"},
	})
	res, out, err := c.identity(context.Background(), nil, identityIn{DeviceID: "d1", Model: "X1", Brand: "ACME"})
	if err != nil || res.IsError {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if (*got)["model"] != "X1" || (*got)["brand"] != "ACME" {
		t.Errorf("请求体不对: %v", *got)
	}
	if !out.Rebuilt || out.Identity["serial"] != "DPD1" {
		t.Errorf("结构化输出不对: %+v", out)
	}
	msg := res.Content[0].(*mcpText).Text
	for _, want := range []string{"ACME / X1 / ACME", "serial=DPD1", "已重建", "droidpool_run"} {
		if !strings.Contains(msg, want) {
			t.Errorf("提示缺少 %q: %s", want, msg)
		}
	}
}

func TestIdentityResetSendsEmptyBody(t *testing.T) {
	c, got := fakeAPI(t, "/api/devices/d1/identity", map[string]any{"device": "d1", "rebuilt": true, "identity": nil})
	res, out, _ := c.identity(context.Background(), nil, identityIn{DeviceID: "d1", Reset: true, Model: "ignored"})
	if res.IsError {
		t.Fatal(res.Content[0].(*mcpText).Text)
	}
	if len(*got) != 0 {
		t.Errorf("reset 应发空对象（控制面按 {} 撤销），实际 %v", *got)
	}
	if out.Identity != nil || !strings.Contains(res.Content[0].(*mcpText).Text, "镜像原样") {
		t.Errorf("撤销后应提示回到镜像原样: %+v / %s", out, res.Content[0].(*mcpText).Text)
	}
}

func TestIdentityRejectsMissingModelWithoutHTTP(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer srv.Close()
	c := &client{base: srv.URL, token: "tok", http: srv.Client()}
	for _, in := range []identityIn{{Model: "X1"}, {DeviceID: "d1"}} {
		if res, _, _ := c.identity(context.Background(), nil, in); !res.IsError {
			t.Errorf("%+v 应被拒", in)
		}
	}
	if called {
		t.Error("参数不全不该打到控制面")
	}
}

func TestIdentityNotRebuiltSaysSo(t *testing.T) {
	c, _ := fakeAPI(t, "/api/devices/d1/identity", map[string]any{"device": "d1", "rebuilt": false,
		"identity": map[string]string{"model": "X1", "brand": "ACME"}})
	res, _, _ := c.identity(context.Background(), nil, identityIn{DeviceID: "d1", Model: "X1", Brand: "ACME"})
	if msg := res.Content[0].(*mcpText).Text; !strings.Contains(msg, "本次未重建") || strings.Contains(msg, "已重建") {
		t.Errorf("没重建不能说重建了: %s", msg)
	}
}

func TestIdentityPassesServerMessageThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unavailable", "message": "本实例未接硬件身份管理"})
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "tok", http: srv.Client()}
	res, _, _ := c.identity(context.Background(), nil, identityIn{DeviceID: "d1", Model: "X1", Brand: "ACME"})
	if msg := res.Content[0].(*mcpText).Text; !res.IsError || !strings.Contains(msg, "未接硬件身份管理") || strings.Contains(msg, "内存不足") {
		t.Errorf("设备接口的 503 要照抄控制面原因，不是 claim 那句内存不足: %s", msg)
	}
}

func TestLocationSetAndOff(t *testing.T) {
	c, got := fakeAPI(t, "/api/devices/d1/location", map[string]any{"device": "d1", "mock_location": "23.1291,113.2644"})
	res, _, _ := c.location(context.Background(), nil, locationIn{DeviceID: "d1", Location: "23.1291,113.2644"})
	if res.IsError || (*got)["location"] != "23.1291,113.2644" {
		t.Fatalf("res=%s body=%v", res.Content[0].(*mcpText).Text, *got)
	}
	if msg := res.Content[0].(*mcpText).Text; !strings.Contains(msg, "23.1291,113.2644") || !strings.Contains(msg, "isMock") {
		t.Errorf("提示应带坐标与 isMock 说明: %s", msg)
	}

	c, got = fakeAPI(t, "/api/devices/d1/location", map[string]any{"device": "d1", "mock_location": ""})
	res, _, _ = c.location(context.Background(), nil, locationIn{DeviceID: "d1"})
	if v, ok := (*got)["location"]; !ok || v != "" {
		t.Errorf("撤销应显式发空串 location，实际 %v", *got)
	}
	if msg := res.Content[0].(*mcpText).Text; res.IsError || !strings.Contains(msg, "撤销") {
		t.Errorf("撤销提示不对: %s", msg)
	}
	if res, _, _ := c.location(context.Background(), nil, locationIn{Location: "1,2"}); !res.IsError {
		t.Error("没 device_id 应被拒")
	}
}

// run 写本地租约记录：已有 CLI 记录时只更新 adb 地址，不能把 lease_id 冲掉（否则之后 CLI release 还不了）
func TestWriteADBAddrKeepsExistingFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".droidpool")
	if err := writeADBAddr(p, "10.0.0.1:5555"); err != nil {
		t.Fatal(err)
	}
	// 已有 CLI 记录（同一台设备，或记录里还没写地址）：只补 / 写地址，lease_id 等字段保留
	if err := os.WriteFile(p, []byte(`{"lease_id":"L1","device_id":"n-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeADBAddr(p, "10.0.0.2:5555"); err != nil {
		t.Fatal(err)
	}
	if err := writeADBAddr(p, "10.0.0.2:5555"); err != nil {
		t.Fatalf("同一台设备再写一次不该报错：%v", err)
	}
	var rec map[string]string
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &rec); err != nil || rec["lease_id"] != "L1" || rec["device_id"] != "n-1" || rec["adb_addr"] != "10.0.0.2:5555" {
		t.Errorf("应保留已有字段、只换 adb 地址，得到 %s", b)
	}
}

func TestWriteADBAddrRefusesAnotherDeviceAndSurvivesNull(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".droidpool")
	if err := os.WriteFile(p, []byte(`{"lease_id":"L1","device_id":"n-1","adb_addr":"10.0.0.1:5555"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeADBAddr(p, "10.0.0.9:5555"); err == nil || !strings.Contains(err.Error(), "另一台设备") {
		t.Errorf("已有另一台设备的记录应拒绝（否则心跳打到 L1、装包打到另一台）：%v", err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), `"10.0.0.1:5555"`) {
		t.Errorf("拒绝时不能改动原记录：%s", b)
	}
	if err := os.WriteFile(p, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeADBAddr(p, "10.0.0.9:5555"); err != nil {
		t.Errorf("内容是 null 时应当作没有记录：%v", err)
	}
}

// run 写的记录要落在 CLI 会读的位置（worktree 顶层 + 会话键），从子目录调也一样
func TestRunWritesRecordWhereCLIReads(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init 不可用: %v\n%s", err, out)
	}
	top, _ := filepath.EvalSymlinks(repo)
	sub := filepath.Join(repo, "cashier-app")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "droidpool"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DROIDPOOL_SESSION", "s1")
	t.Chdir(sub)
	c := &client{}
	res, _, _ := c.run(context.Background(), nil, runIn{ADBAddr: "10.0.0.2:5555", APK: "app.apk"})
	if res.IsError {
		t.Fatalf("run 应成功：%+v", res.Content)
	}
	b, err := os.ReadFile(filepath.Join(top, ".droidpool.s1"))
	if err != nil || !strings.Contains(string(b), "10.0.0.2:5555") {
		t.Errorf("记录应写到 worktree 顶层的 .droidpool.s1：%s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(sub, ".droidpool")); err == nil {
		t.Error("不该再往当前目录写 .droidpool")
	}
}
