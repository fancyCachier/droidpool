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
  # devices=8 对应 max_devices，video_nr 从 20 起避开板载的编解码节点。
  cat > /etc/modprobe.d/droidpool-v4l2.conf <<'MODCONF'
options v4l2loopback devices=8 video_nr=20,21,22,23,24,25,26,27 card_label=droidpool-cam0,droidpool-cam1,droidpool-cam2,droidpool-cam3,droidpool-cam4,droidpool-cam5,droidpool-cam6,droidpool-cam7 exclusive_caps=1
MODCONF
  echo v4l2loopback > /etc/modules-load.d/droidpool-v4l2.conf
  # v4l2loopback-ctl 用来设 fps。不设的话设备报 30 fps，超出
  # external_camera_config.xml 里 720p 的 15 上限，HAL 会把整个设备丢掉，
  # 相机数恒为 0——错误信息只说 characteristics 失败，看不出是帧率的事。
  command -v v4l2loopback-ctl >/dev/null || apt-get install -y -qq v4l2loopback-utils
  echo "   已配置（摄像头默认不启用，用 rtsp-camera.sh 起流才生效）"
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
