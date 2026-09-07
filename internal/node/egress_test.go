package node

import (
	"context"
	"strings"
	"testing"
)

func egressNode(r Runner) *Node {
	n := testNode(r)
	n.Egress = true
	n.EgressDNS = "223.5.5.5"
	return n
}

// 网关查询要有返回值，否则 ensureEgressNetwork 会报「没有网关地址」。
func egressReplies() []reply {
	return []reply{{match: "network inspect", out: "172.18.0.1\n"}}
}

func TestCreateWithEgressWiresSidecars(t *testing.T) {
	f := &fakeRunner{replies: egressReplies()}
	if err := egressNode(f).Create(context.Background(), "d1", 5561, ""); err != nil {
		t.Fatal(err)
	}

	relay := strings.Join(f.lastMatching("--name droidpool-egress-d1"), " ")
	if !strings.Contains(relay, "socks5://:1080?udp=true") {
		// 没有 UDP ASSOCIATE 就没有 DNS，容器里所有域名都解析不了
		t.Errorf("中继必须开 UDP：%s", relay)
	}
	tun := strings.Join(f.lastMatching("--name droidpool-tun-d1"), " ")
	for _, want := range []string{
		"-p 5561:5555", // 端口只能发布在边车上
		"PROXY=socks5://droidpool-egress-d1:1080", // 用容器名，中继换 IP 也不断
		"MTU=1500",        // 默认 9000 会让数据传输卡死
		"--dns 223.5.5.5", // --dns 与 --network container: 互斥，只能设这里
		"--device /dev/net/tun",
	} {
		if !strings.Contains(tun, want) {
			t.Errorf("边车缺少 %q\n实际: %s", want, tun)
		}
	}

	redroid := strings.Join(f.lastMatching("--name droidpool-d1"), " ")
	if !strings.Contains(redroid, "--network container:droidpool-tun-d1") {
		t.Errorf("redroid 未加入边车 netns：%s", redroid)
	}
	// 共享 netns 的容器不允许自己发布端口，带上 -p 会直接起不来
	if strings.Contains(redroid, "-p 5561") {
		t.Errorf("redroid 不该带 -p：%s", redroid)
	}
}

func TestCreateWithoutEgressUnchanged(t *testing.T) {
	f := &fakeRunner{}
	if err := testNode(f).Create(context.Background(), "d1", 5561, ""); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.lastMatching("--name droidpool-d1"), " ")
	if !strings.Contains(joined, "-p 5561:5555") {
		t.Errorf("未开出口时应照旧自己发布端口：%s", joined)
	}
	if strings.Contains(joined, "--network container:") {
		t.Errorf("未开出口时不该加入别人的 netns：%s", joined)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "droidpool-tun-") {
			t.Errorf("未开出口时不该起边车：%v", c)
		}
	}
}

// FinishEgress 的两条规则顺序不能反：代理自身要先拿到真实网卡，
// 否则它的上行会掉进 Android netd 的 `32000: unreachable` 里，整条隧道自锁。
func TestFinishEgressRuleOrderAndLANSplit(t *testing.T) {
	f := &fakeRunner{replies: egressReplies()}
	if err := egressNode(f).FinishEgress(context.Background(), "d1"); err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "ip rule add") {
			rules = append(rules, j)
		}
	}
	if len(rules) != 2 {
		t.Fatalf("应当只加两条规则，实际 %d 条：%v", len(rules), rules)
	}
	if !strings.Contains(rules[0], "pref 17000 fwmark 0x22b lookup main") {
		t.Errorf("第一条必须放行代理自身上行，实际：%s", rules[0])
	}
	if !strings.Contains(rules[1], "pref 17001 not fwmark 0x22b lookup 555") {
		t.Errorf("第二条才是把其余流量导入隧道，实际：%s", rules[1])
	}

	// 内网必须直连，否则 adb 回包进隧道，设备直接失联
	for _, cidr := range []string{"192.168.0.0/16", "172.16.0.0/12", "10.0.0.0/8"} {
		if f.lastMatching("ip route replace "+cidr+" via 172.18.0.1") == nil {
			t.Errorf("缺少内网直连路由 %s", cidr)
		}
	}
}

func TestFinishEgressNoopWhenDisabled(t *testing.T) {
	f := &fakeRunner{}
	if err := testNode(f).FinishEgress(context.Background(), "d1"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("未开出口时 FinishEgress 应当什么都不做，实际：%v", f.calls)
	}
}

// SetEgress 只该动中继——边车持有 netns，重建它等于把设备网络拔掉。
func TestSetEgressOnlyTouchesRelay(t *testing.T) {
	f := &fakeRunner{replies: egressReplies()}
	if err := egressNode(f).SetEgress(context.Background(), "d1", "socks5://up:1080"); err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(f.lastMatching("--name droidpool-egress-d1"), " "); !strings.Contains(j, "-F socks5://up:1080") {
		t.Errorf("中继未接上上游：%s", j)
	}
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "rm -f droidpool-tun-d1") || strings.Contains(j, "rm -f droidpool-d1") {
			t.Errorf("换出口不该动边车或设备：%v", c)
		}
	}
}

func TestRemoveCleansEgressChain(t *testing.T) {
	f := &fakeRunner{replies: egressReplies()}
	if err := egressNode(f).Remove(context.Background(), "d1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rm -f droidpool-d1", "rm -f droidpool-tun-d1", "rm -f droidpool-egress-d1"} {
		if f.lastMatching(want) == nil {
			t.Errorf("Remove 漏了 %q，残留容器会占着端口让下次 Create 失败", want)
		}
	}
}

func TestSidecarDeviceID(t *testing.T) {
	cases := map[string]string{
		"droidpool-tun-3588-a-1":    "3588-a-1",
		"droidpool-egress-3588-a-1": "3588-a-1",
		"droidpool-3588-a-1":        "", // 设备本身不是边车
		"droidpool-golden":          "",
		"droidpool-tun-":            "", // 空 id 不算
		"unrelated":                 "",
	}
	for in, want := range cases {
		got, ok := SidecarDeviceID(in)
		if want == "" {
			if ok {
				t.Errorf("SidecarDeviceID(%q) 不该判定为边车，得到 %q", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("SidecarDeviceID(%q) = %q,%v，期望 %q,true", in, got, ok, want)
		}
	}
}

// 对账绝不能删掉在用设备的边车：tun 边车持有 netns，删了那台设备网络就断了，
// 而且它占着 adb 端口会同时命中「抢占端口」那条判定。
func TestReconcileKeepsSidecarsOfLiveDevices(t *testing.T) {
	f := &fakeRunner{replies: []reply{{
		match: "ps -a",
		out: "droidpool-3588-a-1\t\n" +
			"droidpool-tun-3588-a-1\t0.0.0.0:5561->5555/tcp\n" +
			"droidpool-egress-3588-a-1\t\n",
	}}}
	keep := map[string]bool{"droidpool-3588-a-1": true}
	removed, err := egressNode(f).Reconcile(context.Background(), keep, []int{5561})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Errorf("在用设备的边车被删了：%v", removed)
	}
}

// 设备已经不在了，它的边车要跟着清掉，否则残留会占着 adb 端口让下次 Create 失败。
func TestReconcileRemovesOrphanedSidecars(t *testing.T) {
	f := &fakeRunner{replies: []reply{{
		match: "ps -a",
		out: "droidpool-tun-3588-a-9\t0.0.0.0:5569->5555/tcp\n" +
			"droidpool-egress-3588-a-9\t\n",
	}}}
	removed, err := egressNode(f).Reconcile(context.Background(), map[string]bool{}, []int{5569})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("孤儿边车应当被清掉，实际删了 %v", removed)
	}
}

// 边车不是设备，不该混进 Running 的设备清单——那会让 ReconcileStore 误判。
func TestRunningExcludesSidecars(t *testing.T) {
	f := &fakeRunner{replies: []reply{{
		match: "ps --format",
		out:   "droidpool-3588-a-1\ndroidpool-tun-3588-a-1\ndroidpool-egress-3588-a-1\n",
	}}}
	names, err := egressNode(f).Running(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "droidpool-3588-a-1" {
		t.Errorf("Running 应当只返回设备，实际 %v", names)
	}
}
