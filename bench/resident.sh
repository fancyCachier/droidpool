#!/usr/bin/env bash
# 常驻容量测试（第二个天花板）：N 台设备都开着 app 挂在首页、无人操作，看节点还剩多少余量。
# 与 sweep.sh 的区别：sweep 测「N 台同时跑登录流程」的最坏瞬时并发；本脚本测「N 台常驻」的稳态占用，
# 后者才是池子实际能放多少台的依据（agent 不会同时冷启动）。
# 逐级加码，任一停止条件命中即收手并报出上一级为常驻上限。
#
# 用法: APK=path/app-debug.apk bench/resident.sh
# 环境: NODE_SSH NODE_IP LEVELS("2 4 6 8 10 12 14 16") SETTLE_S(60) SAMPLE_S(60)
#       MIN_AVAIL_MIB(1500) 剩余可用内存低于此值即判定到顶
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
NODE_SSH=${NODE_SSH:-office-3588-sa}; NODE_IP=${NODE_IP:-192.168.14.54}
APK=${APK:?需要 APK=path/to/app-debug.apk}
LEVELS=${LEVELS:-"2 4 6 8 10 12 14 16"}
SETTLE_S=${SETTLE_S:-60}; SAMPLE_S=${SAMPLE_S:-60}; MIN_AVAIL_MIB=${MIN_AVAIL_MIB:-1500}
BASE_PORT=5560; PKG=cn.daboshi.cashier_app.dev
export ADB=${ADB:-adb}
OUT=${OUT:-$HERE/out/resident-$(date +%Y%m%d-%H%M%S)}; mkdir -p "$OUT"

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$OUT/resident.log"; }
dev() { echo "$NODE_IP:$((BASE_PORT + $1))"; }

ssh "$NODE_SSH" "mkdir -p /tmp/bench" && scp -q "$HERE/redroid-up.sh" "$HERE/sampler.sh" "$HERE/node-ensure.sh" "$NODE_SSH:/tmp/bench/" && ssh "$NODE_SSH" "chmod +x /tmp/bench/*.sh"
echo "level,ok_devices,mem_used_mib,mem_avail_mib,swap_used_mib,total_cpu_pct,temp_c,load1,oom" > "$OUT/resident.csv"

LAST_OK=0
for n in $LEVELS; do
  log "=== 常驻 N=$n ==="
  ssh "$NODE_SSH" "/tmp/bench/node-ensure.sh $n" 2>&1 | tee -a "$OUT/resident.log"

  ok=0
  for i in $(seq 1 "$n"); do
    d=$(dev "$i")
    $ADB connect "$d" >/dev/null 2>&1
    for _ in $(seq 1 20); do [ "$($ADB -s "$d" get-state 2>/dev/null)" = "device" ] && break; sleep 1; done
    [ "$($ADB -s "$d" get-state 2>/dev/null)" = "device" ] || { log "  $d 连不上"; continue; }
    $ADB -s "$d" shell pm path $PKG 2>/dev/null | grep -q package || $ADB -s "$d" install -r -t "$APK" >/dev/null 2>&1
    # 已登录过的设备重启 app 直接回首页；未登录的先过一遍 login_flow
    $ADB -s "$d" shell "am start -n $PKG/cn.daboshi.cashier_app.MainActivity" >/dev/null 2>&1
    sleep 3
    if ! $ADB -s "$d" shell "rm -f /sdcard/ui.xml; uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; cat /sdcard/ui.xml" 2>/dev/null | grep -q "桌台包厢"; then
      bash "$HERE/seed-edge.sh" "$d" >/dev/null 2>&1
      bash "$HERE/login_flow.sh" "$d" "$OUT/N$n-${d##*:}" >"$OUT/N$n-${d##*:}.log" 2>&1
    fi
    $ADB -s "$d" shell "rm -f /sdcard/ui.xml; uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; cat /sdcard/ui.xml" 2>/dev/null | grep -q "桌台包厢" \
      && ok=$((ok + 1)) || log "  $d 未停在首页"
  done
  log "  $ok/$n 台停在首页，静置 ${SETTLE_S}s 后采样 ${SAMPLE_S}s"
  sleep "$SETTLE_S"

  ssh "$NODE_SSH" "nohup /tmp/bench/sampler.sh /tmp/bench/res-N$n.log 3 >/dev/null 2>&1 & echo \$! > /tmp/bench/sampler.pid"
  sleep "$SAMPLE_S"
  ssh "$NODE_SSH" "kill \$(cat /tmp/bench/sampler.pid) 2>/dev/null; true"
  scp -q "$NODE_SSH:/tmp/bench/res-N$n.log" "$OUT/res-N$n.log"

  read -r avail swap oom < <(ssh "$NODE_SSH" 'free -m | awk "NR==2{printf \"%s \", \$7} NR==3{printf \"%s \", \$3}"; sudo -n dmesg 2>/dev/null | grep -ciE "out of memory|oom-kill" || echo 0')
  read -r cpu temp load mem < <(awk '{for(i=1;i<=NF;i++){k=split($i,kv,"="); if(k==2){if(kv[1]=="total_cpu"){c+=kv[2];n++} if(kv[1]=="temp"&&kv[2]/1000>t)t=kv[2]/1000; if(kv[1]=="load1")l=kv[2]; if(kv[1]=="mem_used")m=kv[2]}}}
    END{printf "%.0f %.1f %s %s\n", (n?c/n:0), t, l, m}' "$OUT/res-N$n.log")
  echo "$n,$ok,$mem,$avail,${swap:-0},$cpu,$temp,$load,$oom" >> "$OUT/resident.csv"
  log "  常驻 $ok 台：mem_used=${mem}MiB avail=${avail}MiB swap=${swap:-0}MiB cpu均值=${cpu}% temp=${temp}C oom=$oom"

  # 停止条件
  if [ "$ok" -lt "$n" ]; then log "!! N=$n 只有 $ok 台就位 → 到顶"; break; fi
  if [ "${oom:-0}" -gt 0 ]; then log "!! 出现 OOM → 到顶"; break; fi
  if [ "${avail:-0}" -lt "$MIN_AVAIL_MIB" ]; then log "!! 剩余可用内存 ${avail}MiB < ${MIN_AVAIL_MIB}MiB → 到顶"; break; fi
  if awk "BEGIN{exit !($temp > 80)}"; then log "!! 温度 ${temp}C > 80 → 到顶"; break; fi
  LAST_OK=$n
done
log "常驻上限（全部条件满足的最大 N）= $LAST_OK"
log "done → $OUT"
