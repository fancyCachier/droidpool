#!/usr/bin/env bash
# 部署 droidpoold 到 devopt。用法: deploy/deploy.sh [ssh别名=office-devopt]
# 幂等：重复执行只更新二进制与配置，不动 token 与数据库。
set -euo pipefail
HOST=${1:-office-devopt}
HERE=$(cd "$(dirname "$0")/.." && pwd)
JAR=${SCRCPY_SERVER_JAR:-$(ls /opt/homebrew/Cellar/scrcpy/*/share/scrcpy/scrcpy-server 2>/dev/null | tail -1)}

[ -f "$HERE/dist/droidpoold-linux-amd64" ] || { echo "先构建: make dist"; exit 1; }
[ -f "$JAR" ] || { echo "找不到 scrcpy-server jar，设 SCRCPY_SERVER_JAR"; exit 1; }

echo "→ 上传到 $HOST"
ssh "$HOST" 'sudo -n mkdir -p /opt/droidpool && sudo -n chown sa:sa /opt/droidpool'
scp -q "$HERE/dist/droidpoold-linux-amd64" "$HOST:/opt/droidpool/droidpoold.new"
scp -q "$HERE/dist/droidpool-linux-amd64"  "$HOST:/opt/droidpool/droidpool"
scp -q "$HERE/deploy/config.toml"           "$HOST:/opt/droidpool/config.toml"
scp -q "$JAR"                                "$HOST:/opt/droidpool/scrcpy-server"
scp -q "$HERE/deploy/droidpoold.service"    "$HOST:/tmp/droidpoold.service"

ssh "$HOST" bash -s <<'REMOTE'
set -e
cd /opt/droidpool
chmod +x droidpoold.new droidpool
mv -f droidpoold.new droidpoold
# token 只在首次生成，之后保留
if [ ! -f env ]; then
  echo "DROIDPOOL_TOKEN=$(head -c 24 /dev/urandom | base64 | tr -d '/+=' | head -c 32)" > env
  chmod 600 env
  echo "  已生成 token（/opt/droidpool/env）"
fi
sudo -n install -m 644 /tmp/droidpoold.service /etc/systemd/system/droidpoold.service
sudo -n systemctl daemon-reload
sudo -n systemctl enable --now droidpoold
sleep 2
sudo -n systemctl restart droidpoold
sleep 2
systemctl is-active droidpoold
REMOTE

echo "→ 探活（端口应立即可达，补池在后台）"
for i in $(seq 1 10); do
  ssh "$HOST" 'curl -sf -m 2 http://127.0.0.1:8600/api/health' >/dev/null 2>&1 && break
  sleep 1
done
ssh "$HOST" 'curl -sf http://127.0.0.1:8600/api/health | head -c 300; echo'
echo "→ 等待补池（首次要造 golden + 起 8 台，约 3 分钟）"
for i in $(seq 1 60); do
  ready=$(ssh "$HOST" 'curl -sf http://127.0.0.1:8600/api/health' 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("devices",{}).get("ready",0))' 2>/dev/null || echo 0)
  printf "\r  ready=%s  (%ds)" "$ready" $((i*5))
  [ "${ready:-0}" -ge 1 ] && [ $i -ge 3 ] && { echo; break; }
  sleep 5
done
# 只看本次启动之后的日志，否则旧启动的错误会混进来误导人
ssh "$HOST" 'sudo -n journalctl -u droidpoold _SYSTEMD_INVOCATION_ID=$(systemctl show -p InvocationID --value droidpoold) --no-pager 2>/dev/null | grep -E "golden|清理|补齐|设备就绪|失败" | tail -12 | cut -c60-220'
echo "✅ 部署完成: http://192.168.14.32:8600"
