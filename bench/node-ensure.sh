#!/usr/bin/env bash
# 节点侧：保证 redroid-1..N 都在运行且 boot 完成（缺的用 redroid-up.sh 起）。
# 用法: node-ensure.sh <N> [起始端口=5600]
# 端口 base+i，数据目录 /data/droidpool/n<i>。
#
# 起始端口默认 5600 而不是紧挨着生产池：deploy/config.toml 里池子占 5561~5576，
# 撞上去有两种坏法——要么 docker 绑不上端口直接失败，要么 droidpool 的对账
# 逻辑把这些容器当成「抢占池子端口的残留」删掉（internal/node 的 hogging 判定）。
# 两种都难归因，所以下面还有一道显式护栏。
set -eu
N=$1
BASE=${2:-5600}
HERE=$(cd "$(dirname "$0")" && pwd)

# 护栏：默认值可以被覆盖，所以真正拦的是「这个端口已经被别的容器占着」。
for i in $(seq 1 "$N"); do
  PORT=$((BASE + i))
  OWNER=$(docker ps --format '{{.Names}}\t{{.Ports}}' | awk -v p=":$PORT->" '$0 ~ p {print $1; exit}')
  if [ -n "$OWNER" ] && [ "$OWNER" != "redroid-$i" ]; then
    echo "端口 $PORT 已被容器 $OWNER 占用。" >&2
    echo "bench 会自己起 redroid-1..N，和它抢会互相破坏——换个起始端口：node-ensure.sh $N <base>" >&2
    exit 1
  fi
done

for i in $(seq 1 "$N"); do
  if docker ps --format '{{.Names}}' | grep -qx "redroid-$i"; then
    echo "redroid-$i running"
  else
    "$HERE/redroid-up.sh" "redroid-$i" $((BASE + i)) "/data/droidpool/n$i"
  fi
done
