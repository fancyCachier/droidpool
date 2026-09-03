#!/usr/bin/env bash
# 并发扫描（路线图 §4.3）：对 N∈LEVELS，起 N 台容器，各轮 N 台并发跑 pm clear → seed-edge → login_flow，
# 同时在节点上采样 CPU/内存/温度/eMMC。每级末尾再采 IDLE_S 秒「app 停在首页」，全部结束后采「app force-stop」。
# 在 agent 宿主机执行。用法:
#   APK=path/app-debug.apk bench/sweep.sh
# 环境: NODE_SSH(ssh 别名) NODE_IP LEVELS("1 2 4 6 8") ROUNDS(3) ROUNDS_N1(5) IDLE_S(45) ADB OUT
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
NODE_SSH=${NODE_SSH:-office-3588-sa}; NODE_IP=${NODE_IP:-192.168.14.54}
APK=${APK:?需要 APK=path/to/app-debug.apk}
LEVELS=${LEVELS:-"1 2 4 6 8"}; ROUNDS=${ROUNDS:-3}; ROUNDS_N1=${ROUNDS_N1:-5}; IDLE_S=${IDLE_S:-45}
BASE_PORT=5560; PKG=cn.daboshi.cashier_app.dev
export ADB=${ADB:-adb}
OUT=${OUT:-$HERE/out/sweep-$(date +%Y%m%d-%H%M%S)}; mkdir -p "$OUT"
MAXN=$(echo "$LEVELS" | awk '{print $NF}')

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$OUT/sweep.log"; }
dev() { echo "$NODE_IP:$((BASE_PORT + $1))"; }
sampler_start() { ssh "$NODE_SSH" "nohup /tmp/bench/sampler.sh /tmp/bench/$1.log 3 >/dev/null 2>&1 & echo \$! > /tmp/bench/sampler.pid"; }
sampler_stop() { ssh "$NODE_SSH" "kill \$(cat /tmp/bench/sampler.pid) 2>/dev/null; true"; scp -q "$NODE_SSH:/tmp/bench/$1.log" "$2"; }

ensure_app() { # 连上并保证已装包
  local d=$1
  $ADB connect "$d" >/dev/null 2>&1
  for _ in $(seq 1 15); do [ "$($ADB -s "$d" get-state 2>/dev/null)" = "device" ] && break; sleep 1; done
  if ! $ADB -s "$d" shell pm path $PKG 2>/dev/null | grep -q package; then
    local t0; t0=$(date +%s)
    $ADB -s "$d" install -r -t "$APK" >/dev/null 2>&1 && echo "$d install_s=$(( $(date +%s) - t0 ))" || echo "$d INSTALL FAILED"
  fi
}

one_run() { # dev level round → 追加一行到 results.csv
  local d=$1 n=$2 r=$3 dir rc t0 t1 cold tot lg onb
  dir="$OUT/N$n/r$r-${d//:/_}"; mkdir -p "$dir"
  $ADB -s "$d" shell pm clear $PKG >/dev/null 2>&1
  bash "$HERE/seed-edge.sh" "$d" >/dev/null 2>&1
  t0=$(date +%s.%N)
  bash "$HERE/login_flow.sh" "$d" "$dir" > "$dir/flow.log" 2>&1; rc=$?
  t1=$(date +%s.%N)
  cold=$(grep -oE 'cold_start_ms=[0-9]+' "$dir/flow.log" | cut -d= -f2)
  tot=$(grep -oE 'total_s=[0-9.]+' "$dir/flow.log" | cut -d= -f2)
  lg=$(grep -oE 'login_s=[0-9.]+' "$dir/flow.log" | cut -d= -f2)
  onb=$(grep -oE 'onboarding_s=[0-9.]+' "$dir/flow.log" | cut -d= -f2)
  echo "$n,$r,$d,$rc,${cold:-},${onb:-},${lg:-},${tot:-$(echo "$t1 - $t0" | bc)}" >> "$OUT/results.csv"
}

echo "level,round,device,rc,cold_start_ms,onboarding_s,login_s,total_s" > "$OUT/results.csv"
ssh "$NODE_SSH" "mkdir -p /tmp/bench" && scp -q "$HERE/redroid-up.sh" "$HERE/sampler.sh" "$HERE/node-ensure.sh" "$NODE_SSH:/tmp/bench/" && ssh "$NODE_SSH" "chmod +x /tmp/bench/*.sh"

for n in $LEVELS; do
  log "=== N=$n ==="
  ssh "$NODE_SSH" "/tmp/bench/node-ensure.sh $n" | tee -a "$OUT/sweep.log"
  for i in $(seq 1 "$n"); do ensure_app "$(dev "$i")"; done | tee -a "$OUT/sweep.log"
  mkdir -p "$OUT/N$n"
  sampler_start "sample-N$n"
  rounds=$ROUNDS; [ "$n" = 1 ] && rounds=$ROUNDS_N1
  for r in $(seq 1 "$rounds"); do
    log "N=$n round $r/$rounds"
    for i in $(seq 1 "$n"); do one_run "$(dev "$i")" "$n" "$r" & done
    wait
    tail -n "$n" "$OUT/results.csv" | tee -a "$OUT/sweep.log"
  done
  log "N=$n 全部停在首页，采样 ${IDLE_S}s"
  sleep "$IDLE_S"
  sampler_stop "sample-N$n" "$OUT/N$n/sample.log"
  ssh "$NODE_SSH" "sudo -n dmesg 2>/dev/null | grep -ciE 'out of memory|oom-kill' || true" | sed 's/^/oom_lines=/' | tee -a "$OUT/sweep.log"
done

log "=== 全部 force-stop，容器空闲采样 ${IDLE_S}s ==="
sampler_start "sample-idle"
for i in $(seq 1 "$MAXN"); do $ADB -s "$(dev "$i")" shell am force-stop $PKG >/dev/null 2>&1; done
sleep "$IDLE_S"
sampler_stop "sample-idle" "$OUT/sample-idle.log"
log "done → $OUT"
