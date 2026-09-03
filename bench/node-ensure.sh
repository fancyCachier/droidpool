#!/usr/bin/env bash
# 节点侧：保证 redroid-1..N 都在运行且 boot 完成（缺的用 redroid-up.sh 起）。端口 5560+i，数据目录 /data/droidpool/n<i>。
# 用法: node-ensure.sh <N>
set -eu
N=$1
HERE=$(cd "$(dirname "$0")" && pwd)
for i in $(seq 1 "$N"); do
  if docker ps --format '{{.Names}}' | grep -qx "redroid-$i"; then
    echo "redroid-$i running"
  else
    "$HERE/redroid-up.sh" "redroid-$i" $((5560 + i)) "/data/droidpool/n$i"
  fi
done
