package node

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// 出口链路：redroid 共享 tun 边车的网络命名空间，边车把公网流量交给中继，
// 中继再转给用户配置的上游 socks5。
//
//	droidpool-egress-<id>   gost 中继，-F 指向上游；换出口只重建它
//	droidpool-tun-<id>      tun2socks 边车，adb 端口发布在它身上
//	droidpool-<id>          redroid，--network container:droidpool-tun-<id>
//
// 中继单独一层是为了**换出口不动设备**：边车持有 netns，重启它等于把 redroid
// 的网络连根拔掉；中继不持有，重建它设备无感。实测 tun2socks 每次连接都会重新
// 解析代理主机名（中继重建后 IP 从 172.18.0.2 变成 172.18.0.4，流量照常），
// 所以边车用固定容器名指向中继，靠自定义网络的 docker DNS 解析。
const (
	// EgressNetwork 自定义网络：默认 bridge 不解析容器名，必须用自定义网络。
	EgressNetwork = "droidpool-egress"
	// egressFwmark 与 tun2socks 镜像内置的 fwmark / 路由表号一致（0x22b = 555）。
	egressFwmark = "0x22b"
	egressTable  = "555"
	relayImage   = "gogost/gost"
	tunImage     = "xjasonlyu/tun2socks"
)

// RelayName / TunName 出口链路上两个辅助容器的名字。
func RelayName(deviceID string) string { return "droidpool-egress-" + deviceID }
func TunName(deviceID string) string   { return "droidpool-tun-" + deviceID }

// SidecarDeviceID 把设备的辅助容器名映射回它服务的设备 id。
// 出口链路两个 + 摄像头推流一个，都跟着设备走。
//
// 这两个容器名同样以 droidpool- 开头，而对账逻辑（Running / Reconcile）是按
// 这个前缀认设备的。不把它们摘出来的话，对账会认定它们「不在设备表里」而
// `docker rm -f` 掉——tun 边车持有 netns，删掉等于把每台设备的网络连根拔掉，
// 而且它还占着 adb 端口，会同时命中「抢占端口」那条判定。
func SidecarDeviceID(container string) (deviceID string, ok bool) {
	for _, p := range []string{"droidpool-tun-", "droidpool-egress-", "droidpool-cam-"} {
		if id, found := strings.CutPrefix(container, p); found && id != "" {
			return id, true
		}
	}
	return "", false
}

// lanRoutes 走真实网卡而不进隧道的网段。
//
// 不分流 adb 就会断：docker 的 DNAT 保留源 IP，adbd 的回包目标是局域网地址，
// 一旦被规则送进 tun 就再也回不去（实测 adb connect 直接失败）。而且 Edge、
// 控制面本来就在内网，绕一圈公网代理既慢又没意义。
var lanRoutes = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"}

// ensureEgressNetwork 建自定义网络（已存在则忽略），返回其网关地址。
func (n *Node) ensureEgressNetwork(ctx context.Context) (string, error) {
	_, _ = n.docker(ctx, "network", "create", EgressNetwork) // 已存在时报错，忽略
	out, err := n.docker(ctx, "network", "inspect", EgressNetwork,
		"-f", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		return "", fmt.Errorf("取 %s 网关: %w", EgressNetwork, err)
	}
	gw := strings.TrimSpace(out)
	if gw == "" {
		return "", fmt.Errorf("%s 没有网关地址", EgressNetwork)
	}
	return gw, nil
}

// startRelay 起中继。proxy 为空表示直连出网（仍然经过中继，方便之后热切换）。
func (n *Node) startRelay(ctx context.Context, deviceID, proxy string) error {
	name := RelayName(deviceID)
	_, _ = n.docker(ctx, "rm", "-f", name)
	// udp=true 不可省：没有 UDP ASSOCIATE 就没有 DNS，容器里域名全解析不了。
	args := []string{"run", "-d", "--name", name, "--network", EgressNetwork,
		relayImage, "-L", "socks5://:1080?udp=true"}
	if proxy != "" {
		args = append(args, "-F", proxy)
	}
	_, err := n.docker(ctx, args...)
	return err
}

// startTun 起 tun2socks 边车，adb 端口发布在它身上——共享 netns 的容器
// 不能自己发布端口，redroid 那边必须不带 -p。
func (n *Node) startTun(ctx context.Context, deviceID string, port int, dns string) error {
	name := TunName(deviceID)
	_, _ = n.docker(ctx, "rm", "-f", name)
	args := []string{"run", "-d", "--name", name, "--network", EgressNetwork,
		"--cap-add", "NET_ADMIN", "--device", "/dev/net/tun",
		"-e", "PROXY=socks5://" + RelayName(deviceID) + ":1080",
		// 默认 MTU 9000 大于实际路径 MTU，握手过后传数据就卡住。
		"-e", "MTU=1500",
		"-p", strconv.Itoa(port) + ":5555"}
	if dns != "" {
		// --dns 只能设在边车上：它和 --network container: 互斥，redroid 那边设不了。
		args = append(args, "--dns", dns)
	}
	args = append(args, tunImage)
	_, err := n.docker(ctx, args...)
	return err
}

// applyEgressRoutes 把设备流量导进隧道。**必须在 Android 起来之后调用。**
//
// Android 的 netd 开机时会在 netns 里装自己的 ip rule，压过 tun2socks 自带的
// 那条 32765，其中 `32000: from all unreachable` 还会把带 fwmark 的代理上行
// 一并堵死。所以要插两条更高优先级的规则抢在 netd 前面，且顺序要紧：
// 代理自身先拿到真实网卡，其余才进隧道。
//
// 优先级取 9000/9001 是因为实测 netd 自己占着 10000~32000（含 17000，
// 最初选的就是它，撞号了）。9000 在 netd 之下、local 表（0）之上，
// 既一定先于 netd 生效，又不会挡住本地回环。
func (n *Node) applyEgressRoutes(ctx context.Context, deviceID, gateway string) error {
	tun := TunName(deviceID)
	for _, cidr := range lanRoutes {
		if _, err := n.docker(ctx, "exec", tun,
			"ip", "route", "replace", cidr, "via", gateway, "dev", "eth0", "table", egressTable); err != nil {
			return fmt.Errorf("加内网直连路由 %s: %w", cidr, err)
		}
	}
	rules := [][]string{
		{"ip", "rule", "add", "pref", "9000", "fwmark", egressFwmark, "lookup", "main"},
		{"ip", "rule", "add", "pref", "9001", "not", "fwmark", egressFwmark, "lookup", egressTable},
	}
	for _, r := range rules {
		if _, err := n.docker(ctx, append([]string{"exec", tun}, r...)...); err != nil {
			return fmt.Errorf("加路由规则 %v: %w", r, err)
		}
	}
	return nil
}

// startEgress 建好出口链路的两层容器，供 Create 在起 redroid 之前调用。
// 上游此刻留空（直连），由控制面在设备就绪后按每台设备的设置调 SetEgress 落上去。
func (n *Node) startEgress(ctx context.Context, deviceID string, port int) error {
	if _, err := n.ensureEgressNetwork(ctx); err != nil {
		return err
	}
	if err := n.startRelay(ctx, deviceID, ""); err != nil {
		return fmt.Errorf("起中继: %w", err)
	}
	if err := n.startTun(ctx, deviceID, port, n.EgressDNS); err != nil {
		return fmt.Errorf("起 tun 边车: %w", err)
	}
	return nil
}

// FinishEgress 在设备起来之后把流量导进隧道。Egress 关闭时是空操作。
func (n *Node) FinishEgress(ctx context.Context, deviceID string) error {
	if !n.Egress {
		return nil
	}
	gw, err := n.ensureEgressNetwork(ctx)
	if err != nil {
		return err
	}
	return n.applyEgressRoutes(ctx, deviceID, gw)
}

// SetEgress 换上游出口。只重建中继，设备与边车都不动，所以租约不中断。
func (n *Node) SetEgress(ctx context.Context, deviceID, proxy string) error {
	if _, err := n.ensureEgressNetwork(ctx); err != nil {
		return err
	}
	return n.startRelay(ctx, deviceID, proxy)
}

// removeEgress 清掉出口链路上的两个辅助容器。
func (n *Node) removeEgress(ctx context.Context, deviceID string) {
	_, _ = n.docker(ctx, "rm", "-f", TunName(deviceID))
	_, _ = n.docker(ctx, "rm", "-f", RelayName(deviceID))
}
