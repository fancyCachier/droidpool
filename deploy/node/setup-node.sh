#!/usr/bin/env bash
# 节点侧一次性准备。在节点上以能 sudo 的账号执行：
#
#   scp -r deploy/node <节点>:/tmp/ && ssh <节点> 'sudo bash /tmp/node/setup-node.sh'
#
# 幂等，可重复跑。装的是「docker 之外」的那些前提——这些东西之前只散在
# 操作记录里，节点重装一次就全丢了。
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "要用 root 跑（sudo bash $0）"; exit 1; }

echo "== tun：每设备独立公网出口用（config.toml 的 egress）"
# CONFIG_TUN=m 且默认不加载。/dev/net/tun 的节点在，但 open 会 ENODEV，
# 症状是 tun2socks 边车起不来说 "create tun: no such device"。
modprobe tun
echo tun > /etc/modules-load.d/droidpool-tun.conf

echo "== v4l2loopback：外接摄像头用（device/redroid-patches）"
if modinfo v4l2loopback >/dev/null 2>&1; then
  # Ubuntu 的 rockchip 内核自带这个模块，不需要 DKMS 现编。
  # devices=8 对应 max_devices。号段从 21 起而不是 20：设备序号从 1 开始
  # （3588-a-1 … 3588-a-8），camera_video_base=20 加序号正好落在 21..28。
  # 按 20..27 建的话第 8 台的节点根本不存在——configure 时看不出来，
  # 要到给那台设备设摄像头才报错。
  # exclusive_caps 要逐个写满 8 个：它是数组参数，写一个 1 只作用于第 0 个，
  # 其余设备会同时暴露 Video Capture 与 Video Output（实测 video21 就是
  # 0x05200003 两个都有），而 exclusive 模式才是摄像头 HAL 期望的形态。
  cat > /etc/modprobe.d/droidpool-v4l2.conf <<'MODCONF'
options v4l2loopback devices=8 video_nr=21,22,23,24,25,26,27,28 card_label=droidpool-cam1,droidpool-cam2,droidpool-cam3,droidpool-cam4,droidpool-cam5,droidpool-cam6,droidpool-cam7,droidpool-cam8 exclusive_caps=1,1,1,1,1,1,1,1
MODCONF
  echo v4l2loopback > /etc/modules-load.d/droidpool-v4l2.conf
  # v4l2loopback-ctl 用来设 fps。不设的话设备报 30 fps，超出
  # external_camera_config.xml 里 720p 的 15 上限，HAL 会把整个设备丢掉，
  # 相机数恒为 0——错误信息只说 characteristics 失败，看不出是帧率的事。
  command -v v4l2loopback-ctl >/dev/null || apt-get install -y -qq v4l2loopback-utils
  # 权限：宿主上节点默认是 root:video 0660，而 camera HAL 跑在 cameraserver
  # 名下、组里没有 video，直接 EACCES。用 udev 规则而不是 chmod——模块每次
  # 重载都会重建节点，靠人记得补 chmod 必然会漏，而漏了的症状是「相机 0 个」，
  # 完全不指向权限。
  cat > /etc/udev/rules.d/99-droidpool-v4l2.rules <<'UDEV'
KERNEL=="video[0-9]*", ATTR{name}=="droidpool-cam*", MODE="0666"
UDEV
  udevadm control --reload-rules 2>/dev/null || true

  # 真的把它加载起来。tun 那段有 modprobe，这段原来只写配置文件就完事了——
  # 脚本照样报「节点准备完成」，而设备节点一个都没有，症状是给设备设摄像头时
  # 报「节点不存在」，完全不指向这里。
  modprobe -r v4l2loopback 2>/dev/null || true
  modprobe v4l2loopback
  # udev 建节点要一会儿，等它出来再往下走，否则后面的校验会误判
  for _ in $(seq 1 10); do [ -e /dev/video21 ] && break; sleep 1; done
  udevadm trigger --subsystem-match=video4linux 2>/dev/null || true
  n=$(ls /dev/video2? 2>/dev/null | wc -l)
  echo "   已加载，$n 个摄像头节点：$(ls /dev/video2? 2>/dev/null | tr '\n' ' ')"
else
  echo "   跳过：这个内核没有 v4l2loopback，摄像头功能不可用"
fi

echo "== polkitd 看门狗"
# 实测这台节点上 polkitd 跑 4 天涨到 961 MiB（正常个位数 MiB），把 available
# 压到准入闸 2048 以下，池子就拒绝新 claim 了。上游的泄漏我们修不了，
# 只能定期回收。按用量触发而不是无脑定时重启：没涨就不动它。
install -m 755 /dev/stdin /usr/local/bin/droidpool-polkit-guard <<'GUARD'
#!/bin/sh
# polkitd 涨过阈值就重启它。sudo 走 sudoers 不依赖 polkit，docker 也不依赖，
# 重启是安全的。
LIMIT_MIB=${LIMIT_MIB:-300}
RSS=$(ps -o rss= -C polkitd 2>/dev/null | tail -1 | tr -d ' ')
[ -n "$RSS" ] || exit 0
MIB=$((RSS / 1024))
[ "$MIB" -lt "$LIMIT_MIB" ] && exit 0
logger -t droidpool-polkit-guard "polkitd 占用 ${MIB} MiB 超过 ${LIMIT_MIB}，重启回收"
systemctl restart polkit
GUARD

cat > /etc/systemd/system/droidpool-polkit-guard.service <<'UNIT'
[Unit]
Description=droidpool: polkitd 涨过阈值就回收
[Service]
Type=oneshot
ExecStart=/usr/local/bin/droidpool-polkit-guard
UNIT

cat > /etc/systemd/system/droidpool-polkit-guard.timer <<'UNIT'
[Unit]
Description=droidpool: 每小时查一次 polkitd 占用
[Timer]
OnBootSec=15min
OnUnitActiveSec=1h
[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload
systemctl enable --now droidpool-polkit-guard.timer

echo
echo "✅ 节点准备完成"
lsmod | grep -E '^(tun|v4l2loopback) ' || true
systemctl list-timers droidpool-polkit-guard.timer --no-pager 2>/dev/null | head -3
