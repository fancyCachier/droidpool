#!/usr/bin/env bash
# 节点侧采样器：每 INTERVAL 秒追加一行到 OUT。
#   epoch load1=<1min负载> temp=<soc毫度> emmc_util=<%> mem_used=<MiB> redroid-1:<cpu%> ... | total_cpu=<所有容器cpu%之和>
# emmc_util 用 /proc/diskstats 的 io_ticks 增量算，等价于 iostat 的 %util。
# 用法: sampler.sh <输出文件> [间隔秒=3]
set -u
OUT=$1; INTERVAL=${2:-3}; DISK=${DISK:-mmcblk0}
read_ticks() { awk -v d="$DISK" '$3==d{print $13}' /proc/diskstats; }
t0=$(date +%s%3N); k0=$(read_ticks)
while :; do
  sleep "$INTERVAL"
  t1=$(date +%s%3N); k1=$(read_ticks)
  util=$(( (k1 - k0) * 100 / (t1 - t0) )); t0=$t1; k0=$k1
  load=$(cut -d' ' -f1 /proc/loadavg)
  temp=$(cat /sys/class/thermal/thermal_zone0/temp)
  memu=$(free -m | awk 'NR==2{print $3}')
  stats=$(docker stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' 2>/dev/null \
    | grep '^redroid-' | tr -d '%' \
    | awk '{printf "%s:%s ", $1, $2; tot += $2} END {printf "| total_cpu=%.0f", tot}')
  echo "$(date +%s) load1=$load temp=$temp emmc_util=$util mem_used=$memu $stats" >> "$OUT"
done
