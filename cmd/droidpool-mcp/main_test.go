package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
