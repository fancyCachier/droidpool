#!/usr/bin/env bash
# 汇总 sweep.sh 的输出目录为 markdown 表（兼容 macOS BSD awk，不用 asort）。用法: summarize.sh <OUT目录>
set -u
OUT=$1
median() { sort -n | awk '{a[NR]=$1} END {if (NR==0) {print "-"; exit} if (NR%2) print a[(NR+1)/2]; else printf "%.1f\n", (a[NR/2]+a[NR/2+1])/2}'; }
echo "| N | 样本 | 失败 | login_flow 中位 s | login_flow 最大 s | 冷启中位 ms | 峰值 total_cpu % | 首页空闲 total_cpu % | 峰值温度 °C | 峰值 eMMC util % | 峰值 mem_used MiB |"
echo "|---|---|---|---|---|---|---|---|---|---|---|"
for n in $(awk -F, 'NR>1{print $1}' "$OUT/results.csv" | sort -n | uniq); do
  rows=$(awk -F, -v n="$n" 'NR>1 && $1==n' "$OUT/results.csv")
  cnt=$(echo "$rows" | grep -c .)
  fail=$(echo "$rows" | awk -F, '$4!=0' | grep -c .)
  med=$(echo "$rows" | awk -F, '{print $8+0}' | median)
  max=$(echo "$rows" | awk -F, '{print $8+0}' | sort -n | tail -1)
  cmed=$(echo "$rows" | awk -F, '$5!=""{print $5+0}' | median)
  S="$OUT/N$n/sample.log"
  if [ -f "$S" ]; then
    read -r pcpu icpu ptemp pio pmem < <(awk '
      { for (i=1;i<=NF;i++) { n=split($i,kv,"="); if (n<2) continue;
          if (kv[1]=="total_cpu") { c=kv[2]+0; if (c>pc) pc=c; all[NR]=c }
          if (kv[1]=="temp") { t=kv[2]/1000; if (t>pt) pt=t }
          if (kv[1]=="emmc_util") { u=kv[2]+0; if (u>pu) pu=u }
          if (kv[1]=="mem_used") { m=kv[2]+0; if (m>pm) pm=m } } }
      END { k=0; for (i=NR; i>NR-12 && i>0; i--) { s+=all[i]; k++ }   # 首页空闲 = 最后 12 个样本（约 36 s）均值
        printf "%.0f %.0f %.1f %d %d\n", pc, (k?s/k:0), pt, pu, pm }' "$S")
  else pcpu=-; icpu=-; ptemp=-; pio=-; pmem=-; fi
  echo "| $n | $cnt | $fail | $med | $max | $cmed | $pcpu | $icpu | $ptemp | $pio | $pmem |"
done
if [ -f "$OUT/sample-idle.log" ]; then
  awk '{ for (i=1;i<=NF;i++) { n=split($i,kv,"="); if (n==2 && kv[1]=="total_cpu") { s+=kv[2]; k++ } } }
       END { printf "\nforce-stop 后容器空闲 total_cpu 均值：%.0f %%（%d 样本）\n", (k?s/k:0), k }' "$OUT/sample-idle.log"
fi
