#!/usr/bin/env bash
# 在节点本机起一个 redroid 容器并等待 boot 完成，打印耗时。
# 用法: redroid-up.sh <容器名> <宿主端口> [data目录]
# 环境: IMAGE（默认 redroid 14 64only）、ARGS（androidboot.* 启动参数）、WIDTH/HEIGHT/DPI
set -eu
NAME=$1; PORT=$2; DATA=${3:-/data/droidpool/$NAME}
IMAGE=${IMAGE:-redroid/redroid:14.0.0_64only-latest}
WIDTH=${WIDTH:-2560}; HEIGHT=${HEIGHT:-1600}; DPI=${DPI:-320}
ARGS=${ARGS:-"androidboot.use_memfd=true androidboot.redroid_width=$WIDTH androidboot.redroid_height=$HEIGHT androidboot.redroid_dpi=$DPI androidboot.redroid_gpu_mode=guest"}

mkdir -p "$DATA"
docker rm -f "$NAME" >/dev/null 2>&1 || true
T0=$(date +%s)
# shellcheck disable=SC2086
docker run -d --privileged --name "$NAME" -v "$DATA:/data" -p "$PORT:5555" "$IMAGE" $ARGS >/dev/null
B=""
for _ in $(seq 1 90); do
  sleep 2
  B=$(docker exec "$NAME" getprop sys.boot_completed 2>/dev/null || true)
  [ "$B" = "1" ] && break
done
echo "$NAME boot_completed=${B:-0} boot_s=$(( $(date +%s) - T0 )) port=$PORT data=$DATA"
[ "$B" = "1" ]
