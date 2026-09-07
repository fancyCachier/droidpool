#!/usr/bin/env bash
# 把构建机上的 redroid 镜像拉到节点。在**节点上**执行。
#
#   pull-image.sh [镜像tag] [构建机ssh目标]
#   例: pull-image.sh droidpool/redroid:14-custom dev@172.16.151.251
#
# 为什么在节点侧拉而不是从开发机推：开发机到构建机那段只有 3.3 MB/s
# （不同网段要绕路由），而节点到构建机是 55 MB/s 直达。之前用开发机中转，
# 等于让最慢的一段扛全程，2.25 GB 的镜像要一分半；直拉十几秒。
#
# 前提：节点能免密 ssh 到构建机（把节点的 id_ed25519.pub 加到构建机的
# authorized_keys）。方向别搞反——构建机反过来连节点是不通的。
set -euo pipefail
TAG=${1:-droidpool/redroid:14-custom}
SRC=${2:-dev@172.16.151.251}

echo "从 $SRC 拉 $TAG …"
T0=$(date +%s)
# zstd 优先：同样 CPU 下比 gzip 压得更小也更快；没有就退回 gzip。
if ssh -o BatchMode=yes "$SRC" 'command -v zstd >/dev/null' && command -v zstd >/dev/null; then
  ssh -o BatchMode=yes "$SRC" "docker save $TAG | zstd -3 -T0" | zstd -d | docker load
else
  ssh -o BatchMode=yes "$SRC" "docker save $TAG | gzip -1" | gunzip | docker load
fi
echo "用时 $(( $(date +%s) - T0 ))s"
docker images "$TAG" --format '{{.Repository}}:{{.Tag}} {{.Size}} {{.ID}}'
