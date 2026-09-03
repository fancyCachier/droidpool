#!/usr/bin/env bash
# 扩展并发扫描：在基础扫描（1 2 4 6 8）之外继续加码，逐级独立调用 sweep.sh，
# 每级开跑前检查节点剩余内存，不足即停手（避免 OOM 把节点拖垮）。
# 每级用独立 OUT 目录，最后合并成一张 results.csv 供 summarize.sh 用。
#
# 用法: APK=path/app-debug.apk bench/sweep-ext.sh [WAIT_PID]
#   WAIT_PID 给出时先等该进程退出（用于串在基础扫描后面）
# 环境: LEVELS("10 12 14 16") MIN_AVAIL_MIB(2500) ROUNDS(3)
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
NODE_SSH=${NODE_SSH:-office-3588-sa}
APK=${APK:?需要 APK=path/to/app-debug.apk}
LEVELS=${LEVELS:-"10 12 14 16"}; MIN_AVAIL_MIB=${MIN_AVAIL_MIB:-2500}; ROUNDS=${ROUNDS:-3}
WAIT_PID=${1:-}
OUT=${OUT:-$HERE/out/sweep-ext-$(date +%Y%m%d-%H%M%S)}; mkdir -p "$OUT"
log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$OUT/ext.log"; }

if [ -n "$WAIT_PID" ]; then
  log "等待基础扫描 pid=$WAIT_PID 退出"
  while kill -0 "$WAIT_PID" 2>/dev/null; do sleep 15; done
  log "基础扫描已结束，开始扩展"
fi

for n in $LEVELS; do
  avail=$(ssh "$NODE_SSH" "free -m | awk 'NR==2{print \$7}'" 2>/dev/null)
  log "N=$n 开跑前 avail=${avail}MiB"
  if [ -z "$avail" ] || [ "$avail" -lt "$MIN_AVAIL_MIB" ]; then
    log "!! 剩余可用内存不足 ${MIN_AVAIL_MIB}MiB，停在 N=$n 之前"; break
  fi
  LEVELS="$n" ROUNDS="$ROUNDS" ROUNDS_N1="$ROUNDS" OUT="$OUT/N$n" bash "$HERE/sweep.sh" 2>&1 | tee -a "$OUT/ext.log"
  # grep -c 计数为 0 时退出码为 1，会额外触发 || echo 0 产生两行，故取首行
  oom=$(ssh "$NODE_SSH" "sudo -n dmesg 2>/dev/null | grep -ciE 'out of memory|oom-kill'; true" | head -1)
  log "N=$n 完成 oom=$oom"
  [ "${oom:-0}" -gt 0 ] && { log "!! 出现 OOM，停手"; break; }
done

# 合并各级 results.csv + sample.log 目录，供 summarize.sh
echo "level,round,device,rc,cold_start_ms,onboarding_s,login_s,total_s" > "$OUT/results.csv"
for f in "$OUT"/N*/results.csv; do [ -f "$f" ] && tail -n +2 "$f" >> "$OUT/results.csv"; done
for d in "$OUT"/N*/N*; do [ -d "$d" ] && ln -sfn "$d" "$OUT/$(basename "$d")" 2>/dev/null; done
log "done → $OUT"
