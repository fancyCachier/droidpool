#!/usr/bin/env bash
# 汇总 sweep.sh 的输出目录为 markdown 表（兼容 macOS BSD awk）。用法: summarize.sh <OUT目录>
#
# 注意：本表**不报「空闲 CPU」**。sweep 每级末尾只留 45 s 空闲窗口，前半段仍盖着上一轮登录流程的
# 衰减尾巴（N 越大尾巴越长，实测 N=1 尾部收敛到 33~39%，N=2 仍在 64~247% 之间跳），
# 拿它当稳态占用会高估。稳态数据以 resident.sh 为准（先静置 60 s 再采 60 s）。
set -u
OUT=$1
median() { sort -n | awk '{a[NR]=$1} END {if (NR==0) {print "-"; exit} if (NR%2) print a[(NR+1)/2]; else printf "%.1f\n", (a[NR/2]+a[NR/2+1])/2}'; }
p95() { sort -n | awk '{a[NR]=$1} END {if (NR==0) {print "-"; exit} i=int(NR*0.95+0.999); if (i<1) i=1; print a[i]}'; }

BASE=""
echo "| N | 样本 | 失败 | login_flow 中位 s | p95 s | 最大 s | 相对 N=1 | 冷启中位 ms | 峰值 total_cpu % | 峰值温度 °C | 峰值 eMMC util % | 峰值 mem_used MiB |"
echo "|---|---|---|---|---|---|---|---|---|---|---|---|"
for n in $(awk -F, 'NR>1{print $1}' "$OUT/results.csv" | sort -n | uniq); do
  rows=$(awk -F, -v n="$n" 'NR>1 && $1==n' "$OUT/results.csv")
  cnt=$(echo "$rows" | grep -c .)
  fail=$(echo "$rows" | awk -F, '$4!=0' | grep -c .)
  med=$(echo "$rows" | awk -F, '{print $8+0}' | median)
  p95v=$(echo "$rows" | awk -F, '{print $8+0}' | p95)
  max=$(echo "$rows" | awk -F, '{print $8+0}' | sort -n | tail -1)
  cmed=$(echo "$rows" | awk -F, '$5!=""{print $5+0}' | median)
  [ -z "$BASE" ] && BASE=$med
  rel=$(awk -v a="$med" -v b="$BASE" 'BEGIN{if(b>0) printf "%.2f×", a/b; else print "-"}')
  S="$OUT/N$n/sample.log"
  if [ -f "$S" ]; then
    read -r pcpu ptemp pio pmem < <(awk '
      { for (i=1;i<=NF;i++) { k=split($i,kv,"="); if (k<2) continue;
          if (kv[1]=="total_cpu" && kv[2]+0>pc) pc=kv[2]+0
          if (kv[1]=="temp" && kv[2]/1000>pt) pt=kv[2]/1000
          if (kv[1]=="emmc_util" && kv[2]+0>pu) pu=kv[2]+0
          if (kv[1]=="mem_used" && kv[2]+0>pm) pm=kv[2]+0 } }
      END { printf "%.0f %.1f %d %d\n", pc, pt, pu, pm }' "$S")
  else pcpu=-; ptemp=-; pio=-; pmem=-; fi
  echo "| $n | $cnt | $fail | $med | $p95v | $max | $rel | $cmed | $pcpu | $ptemp | $pio | $pmem |"
done
echo
echo "N_max 判据（路线图 §4.3）：login_flow p95 ≤ 2 × N=1 值、0 次 OOM、峰值温度 < 80 °C、峰值 eMMC util < 80 %。"
