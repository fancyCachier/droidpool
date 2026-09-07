#!/usr/bin/env bash
# 在节点上把一路 RTSP 变成一个 v4l2 摄像头节点，供 redroid 的外接摄像头 HAL 使用。
#
#   rtsp-camera.sh start <设备号> <rtsp-url>   例: start 20 rtsp://cam/live
#   rtsp-camera.sh stop  <设备号>
#   rtsp-camera.sh status
#
# 设备号即 /dev/videoN 的 N，一台 redroid 用一个，容器起的时候 --device 进去。
#
# 前提：节点要有 v4l2loopback 模块（Ubuntu rockchip 内核自带，不用 DKMS 编）。
# 首次用 `modprobe v4l2loopback`，持久化写 /etc/modules-load.d/。
set -euo pipefail

PIDDIR=/run/droidpool-cam
mkdir -p "$PIDDIR"

need_module() {
  lsmod | grep -qw v4l2loopback && return 0
  echo "加载 v4l2loopback…"
  # devices=0 先建个空的，之后按需用 v4l2loopback-ctl 加；这里图简单一次给够
  modprobe v4l2loopback devices=8 video_nr=20,21,22,23,24,25,26,27 \
    card_label=droidpool-cam0,droidpool-cam1,droidpool-cam2,droidpool-cam3,droidpool-cam4,droidpool-cam5,droidpool-cam6,droidpool-cam7 \
    exclusive_caps=1
}

case "${1:-}" in
start)
  N=${2:?设备号}; URL=${3:?rtsp url}
  need_module
  DEV=/dev/video$N
  [ -e "$DEV" ] || { echo "$DEV 不存在（v4l2loopback 的 video_nr 覆盖到了吗）"; exit 1; }
  PID=$PIDDIR/$N.pid
  if [ -f "$PID" ] && kill -0 "$(cat "$PID")" 2>/dev/null; then
    echo "$DEV 已有推流在跑（pid $(cat "$PID")），先 stop"; exit 1
  fi
  # 设备节点必须让容器里的 cameraserver 读得到。宿主上它是 root:video 0660，
  # 而 HAL 跑在 cameraserver 名下、组是 audio/camera/input/drmrpc/usb——没有
  # video，直接 EACCES。容器和宿主共用 uid 空间，宿主的 video 组（gid 44）
  # 在 Android 侧也不对应 camera（1006），所以只能放开权限位。
  chmod 0666 "$DEV"

  # 必须编成 MJPEG。AOSP 的 external camera HAL 只认两种 fourcc：
  #   const std::array<uint32_t, 2> kSupportedFourCCs{{V4L2_PIX_FMT_MJPEG, V4L2_PIX_FMT_Z16}};
  # （Z16 是深度相机）。喂 YUYV 之类的它会判 "Unsupported format found" 并把
  # 这个设备整个丢掉，症状是 /dev/video* 在容器里看得见、相机却报 0 个设备
  # ——redroid-doc#178 那个悬着的问题就是这个形状。
  #
  # -rtsp_transport tcp：UDP 丢包在容器里表现为花屏，排查成本高，直接走 TCP。
  nohup ffmpeg -hide_banner -loglevel warning -nostdin \
    -rtsp_transport tcp -re -i "$URL" \
    -vf scale=1280:720 -r 15 -c:v mjpeg -q:v 5 -f v4l2 "$DEV" \
    > "$PIDDIR/$N.log" 2>&1 &
  echo $! > "$PID"
  sleep 2
  if kill -0 "$(cat "$PID")" 2>/dev/null; then
    echo "✅ $URL → $DEV （pid $(cat "$PID")）"
  else
    echo "❌ 推流没起来，日志："; tail -5 "$PIDDIR/$N.log"; rm -f "$PID"; exit 1
  fi
  ;;
stop)
  N=${2:?设备号}; PID=$PIDDIR/$N.pid
  [ -f "$PID" ] || { echo "/dev/video$N 上没有在跑的推流"; exit 0; }
  kill "$(cat "$PID")" 2>/dev/null || true
  rm -f "$PID"
  echo "已停 /dev/video$N"
  ;;
status)
  lsmod | grep -w v4l2loopback || echo "(v4l2loopback 未加载)"
  for f in "$PIDDIR"/*.pid; do
    [ -e "$f" ] || continue
    n=$(basename "$f" .pid)
    if kill -0 "$(cat "$f")" 2>/dev/null; then echo "  /dev/video$n  推流中 pid $(cat "$f")"
    else echo "  /dev/video$n  pid 文件在但进程没了"; fi
  done
  ;;
*)
  sed -n '2,12p' "$0"; exit 2;;
esac
