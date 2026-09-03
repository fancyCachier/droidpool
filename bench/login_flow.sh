#!/usr/bin/env bash
# cashier-app 登录到首页的最小流程（基线主指标 login_flow），在 agent 宿主机执行。
# 前置：app 已安装、seed-edge.sh 已写入端点。脚本自己启动 app，处理隐私政策 / 设备角色 / OTA 弹窗。
# 用法: login_flow.sh <ip:port> <输出目录> [员工工号=jingli] [PIN=111111]
# 退出码: 0 到首页 · 2 停在连接 Edge 页 · 3 无员工卡 · 4 无登录按钮 · 5 超时未见首页
set -u
DEV=$1; OUT=$2; EMP=${3:-jingli}; PIN=${4:-111111}
PKG=${PKG:-cn.daboshi.cashier_app.dev}
ADB=${ADB:-adb}
mkdir -p "$OUT"

a() { $ADB -s "$DEV" "$@"; }
dump() { a shell "rm -f /sdcard/ui.xml; uiautomator dump /sdcard/ui.xml >/dev/null 2>&1; cat /sdcard/ui.xml" 2>/dev/null; }  # 先删旧文件，dump 失败时不会读到陈旧层级
descs() { dump | grep -oE 'content-desc="[^"]{1,60}"' | sort -u | tr '\n' ' '; echo; }
# 精确匹配 content-desc
centerX() { dump | grep -oE "content-desc=\"$1\"[^>]*bounds=\"\[[0-9]+,[0-9]+\]\[[0-9]+,[0-9]+\]\"" | head -1 \
  | grep -oE '\[[0-9]+,[0-9]+\]\[[0-9]+,[0-9]+\]' | sed 's/\]\[/,/; s/[][]//g' | awk -F, '{print int(($1+$3)/2), int(($2+$4)/2)}'; }
# 片段匹配（用于含换行的员工卡）
centerF() { dump | grep -oE "content-desc=\"[^\"]*$1[^\"]*\"[^>]*bounds=\"\[[0-9]+,[0-9]+\]\[[0-9]+,[0-9]+\]\"" | head -1 \
  | grep -oE '\[[0-9]+,[0-9]+\]\[[0-9]+,[0-9]+\]' | sed 's/\]\[/,/; s/[][]//g' | awk -F, '{print int(($1+$3)/2), int(($2+$4)/2)}'; }
tapX() { local c; c=$(centerX "$1"); [ -z "$c" ] && return 1; a shell input tap $c; echo "tap [$1] @ $c"; }
tapF() { local c; c=$(centerF "$1"); [ -z "$c" ] && return 1; a shell input tap $c; echo "tap [~$1] @ $c"; }
shot() { a exec-out screencap -p > "$OUT/$1.png"; }
now() { date +%s.%N; }
dt() { echo "$2 - $1" | bc; }

T_start=$(now)
a shell "am start -W -n $PKG/cn.daboshi.cashier_app.MainActivity" 2>&1 | grep -E "TotalTime" | tr -d ' ' | sed 's/^/cold_start_ms=/; s/TotalTime://'
sleep 3

# onboarding：最多绕 6 轮，直到见到员工卡或连接 Edge 页
for _ in 1 2 3 4 5 6; do
  D=$(descs)
  echo "$D" | grep -qE "$EMP|测试并连接" && break
  echo "$D" | grep -q "同意并继续" && { tapX "同意并继续"; sleep 3; continue; }
  echo "$D" | grep -q "这台设备是" && { tapF "共享收银机"; sleep 4; continue; }
  sleep 2
done
shot 10-login-page
descs | grep -q "测试并连接" && { echo "RESULT: 停在连接 Edge 页（未 seed 或 pin 不符）"; exit 2; }
T_login_page=$(now)

tapF "$EMP" || { echo "RESULT: 无员工卡 $EMP"; exit 3; }
sleep 1
for d in $(echo "$PIN" | grep -o .); do c=$(centerX "$d"); a shell input tap $c; sleep 0.3; done
shot 11-pin
if ! tapX "登 录"; then
  a shell input swipe 1280 1400 1280 500 300; sleep 1     # 登录按钮可能在首屏之下
  tapX "登 录" || { echo "RESULT: 无登录按钮"; exit 4; }
fi

ok=0
for _ in $(seq 1 20); do
  sleep 1
  D=$(descs)
  echo "$D" | grep -q "以后再说" && { tapX "以后再说"; sleep 1; }
  echo "$D" | grep -qE "桌台包厢|球台" && { ok=1; break; }
done
T_home=$(now)
shot 12-home
printf "onboarding_s=%.1f login_s=%.1f total_s=%.1f\n" "$(dt "$T_start" "$T_login_page")" "$(dt "$T_login_page" "$T_home")" "$(dt "$T_start" "$T_home")"
[ $ok = 1 ] && { echo "RESULT: OK 首页"; exit 0; } || { echo "RESULT: FAIL 未见首页"; exit 5; }
