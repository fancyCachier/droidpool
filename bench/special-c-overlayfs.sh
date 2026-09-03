#!/usr/bin/env bash
# 专项 C：验证 redroid 原生 overlayfs 共享 data（androidboot.use_redroid_overlayfs=1）
# 这是 Phase 2「零拷贝复位」的前提。.54 只有 ext4 eMMC、无 reflink，拷贝式复位不可行。
#
# 验证三件事：
#   1. 两个实例挂同一个 /data-base + 各自 /data-diff，都能启动
#   2. 各自 diff 互不可见（A 装的包 B 看不到）
#   3. 删掉 diff 重建容器即回到 base 状态（复位有效）
#
# 在节点本机执行。用法: special-c-overlayfs.sh [APK]
set -u
ROOT=${ROOT:-/data/droidpool/ovl}
IMAGE=${IMAGE:-redroid/redroid:14.0.0_64only-latest}
BASE_ARGS="androidboot.use_memfd=true androidboot.redroid_width=1366 androidboot.redroid_height=768 androidboot.redroid_dpi=160 androidboot.redroid_gpu_mode=guest"
APK=${1:-}
PASS=0; FAIL=0
ok()   { echo "  ✅ $*"; PASS=$((PASS+1)); }
bad()  { echo "  ❌ $*"; FAIL=$((FAIL+1)); }
boot() { # $1=name $2=port $3...=extra args
  local name=$1 port=$2; shift 2
  docker rm -f "$name" >/dev/null 2>&1
  docker run -d --privileged --name "$name" -p "$port:5555" "$@" "$IMAGE" $BASE_ARGS androidboot.use_redroid_overlayfs=1 >/dev/null || return 1
  for _ in $(seq 1 60); do
    sleep 2
    [ "$(docker exec "$name" getprop sys.boot_completed 2>/dev/null)" = "1" ] && return 0
  done
  return 1
}

echo "=== 专项 C：overlayfs 共享 data ==="
mkdir -p "$ROOT"/{base,diff-a,diff-b}
rm -rf "${ROOT:?}"/diff-a/* "${ROOT:?}"/diff-b/* 2>/dev/null

echo "[1/4] 造 base（普通 -v /data 挂载，装一次 app 作为共享基底）"
docker rm -f ovl-seed >/dev/null 2>&1
docker run -d --privileged --name ovl-seed -v "$ROOT/base:/data" -p 5591:5555 "$IMAGE" $BASE_ARGS >/dev/null
for _ in $(seq 1 60); do sleep 2; [ "$(docker exec ovl-seed getprop sys.boot_completed 2>/dev/null)" = "1" ] && break; done
if [ -n "$APK" ] && [ -f "$APK" ]; then
  adb connect "127.0.0.1:5591" >/dev/null 2>&1; sleep 2
  adb -s 127.0.0.1:5591 install -r -t "$APK" >/dev/null 2>&1 && echo "  base 已装 app" || echo "  base 装 app 失败（不阻断后续）"
  adb disconnect 127.0.0.1:5591 >/dev/null 2>&1
fi
docker rm -f ovl-seed >/dev/null 2>&1
echo "  base 大小: $(du -sh "$ROOT/base" | cut -f1)"

echo "[2/4] 两个实例挂同一 base + 各自 diff"
if boot ovl-a 5592 -v "$ROOT/base:/data-base" -v "$ROOT/diff-a:/data-diff"; then ok "ovl-a 启动"; else bad "ovl-a 启动失败"; fi
if boot ovl-b 5593 -v "$ROOT/base:/data-base" -v "$ROOT/diff-b:/data-diff"; then ok "ovl-b 启动"; else bad "ovl-b 启动失败"; fi

echo "[3/4] diff 隔离：在 a 里写文件，b 不应看到"
docker exec ovl-a sh -c 'echo hello-from-a > /data/ovl-test.txt' 2>/dev/null
A=$(docker exec ovl-a cat /data/ovl-test.txt 2>/dev/null)
B=$(docker exec ovl-b cat /data/ovl-test.txt 2>/dev/null)
[ "$A" = "hello-from-a" ] && ok "a 能读到自己写的" || bad "a 读不到自己写的（得到 '$A'）"
[ -z "$B" ] && ok "b 看不到 a 的写入（隔离成立）" || bad "b 看到了 a 的写入 '$B'（隔离失败）"
echo "  diff-a 大小: $(du -sh "$ROOT/diff-a" | cut -f1)  diff-b: $(du -sh "$ROOT/diff-b" | cut -f1)"
echo "  base 是否被污染: $(ls "$ROOT/base/ovl-test.txt" 2>/dev/null && echo '是（base 被写穿！）' || echo '否')"

echo "[4/4] 复位：删 diff-a 重建，写入应消失"
docker rm -f ovl-a >/dev/null 2>&1
T0=$(date +%s)
rm -rf "${ROOT:?}/diff-a"; mkdir -p "$ROOT/diff-a"
if boot ovl-a 5592 -v "$ROOT/base:/data-base" -v "$ROOT/diff-a:/data-diff"; then
  ok "复位后重启成功（耗时 $(( $(date +%s) - T0 ))s）"
  R=$(docker exec ovl-a cat /data/ovl-test.txt 2>/dev/null)
  [ -z "$R" ] && ok "复位后写入已消失" || bad "复位后写入仍在 '$R'"
  if [ -n "$APK" ]; then
    docker exec ovl-a sh -c 'pm path cn.daboshi.cashier_app.dev' >/dev/null 2>&1 \
      && ok "复位后 base 里的 app 仍在（共享基底有效）" || bad "复位后 app 不在（base 没生效）"
  fi
else bad "复位后重启失败"; fi

echo
echo "=== 结果: 通过 $PASS 项，失败 $FAIL 项 ==="
echo "清理: docker rm -f ovl-a ovl-b; rm -rf $ROOT"
[ "$FAIL" -eq 0 ]
