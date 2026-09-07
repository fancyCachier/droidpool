#!/usr/bin/env bash
# 部署 droidpoold 到 devopt。用法: deploy/deploy.sh [ssh别名=office-devopt]
# 幂等：重复执行只更新二进制与配置，不动 token 与数据库。
set -euo pipefail
HOST=${1:-office-devopt}
HERE=$(cd "$(dirname "$0")/.." && pwd)
JAR=${SCRCPY_SERVER_JAR:-$(ls /opt/homebrew/Cellar/scrcpy/*/share/scrcpy/scrcpy-server 2>/dev/null | tail -1)}
# uiagent.dex 给 /api/devices/{id}/ui 用。缺了不阻断部署，那个接口返回 503 而已。
DEX=${DROIDPOOL_UIAGENT_DEX:-$HERE/device/uiagent/uiagent.dex}

[ -f "$HERE/dist/droidpoold-linux-amd64" ] || { echo "先构建: make dist"; exit 1; }
[ -f "$JAR" ] || { echo "找不到 scrcpy-server jar，设 SCRCPY_SERVER_JAR"; exit 1; }

echo "→ 上传到 $HOST"
ssh "$HOST" 'sudo -n mkdir -p /opt/droidpool && sudo -n chown sa:sa /opt/droidpool'
scp -q "$HERE/dist/droidpoold-linux-amd64" "$HOST:/opt/droidpool/droidpoold.new"
scp -q "$HERE/dist/droidpool-linux-amd64"  "$HOST:/opt/droidpool/droidpool"
scp -q "$HERE/deploy/config.toml"           "$HOST:/opt/droidpool/config.toml"
scp -q "$JAR"                                "$HOST:/opt/droidpool/scrcpy-server"
if [ -f "$DEX" ]; then
  scp -q "$DEX" "$HOST:/opt/droidpool/uiagent.dex"
else
  echo "  ⚠ 没有 $DEX（跑 device/uiagent/build.sh 生成），界面层级接口将返回 503"
fi
scp -q "$HERE/deploy/droidpoold.service"    "$HOST:/tmp/droidpoold.service"
scp -q "$HERE/deploy/cert/recv-cert.sh"     "$HOST:/tmp/recv-cert.sh"

ssh "$HOST" bash -s <<'REMOTE'
set -e
cd /opt/droidpool
chmod +x droidpoold.new droidpool
# 证书接收端（office-gateway 的 acme.sh 续签后经 ssh forced command 推过来，见 docs/2026-09-06-https-cert.md）
mkdir -p bin tls && chmod 700 tls && install -m 755 /tmp/recv-cert.sh bin/recv-cert.sh && rm -f /tmp/recv-cert.sh
# 配置开了 [tls] 但证书还没推到：新二进制会拒绝启动，别把正在跑的旧进程换掉
if grep -q '^\[tls\]' config.toml && [ ! -s tls/fullchain.pem ]; then
  echo "config.toml 开了 [tls] 但 /opt/droidpool/tls/fullchain.pem 不存在；先在 office-gateway 跑 ~/bin/droidpool-deploy-cert.sh 推证书" >&2
  rm -f droidpoold.new
  exit 1
fi
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

# 节点侧脚本：从 config.toml 的 docker_host 认出节点，把 deploy/node/ 推过去。
# 之前不带这一步，setup-node.sh 与 pull-image.sh 只能手工 scp——节点重装一次
# 就得靠人记得有这两个东西。推过去只是放着，不自动执行：setup-node.sh 要 root，
# 而且加载内核模块这种事该由人明确触发。
NODE_SSH=$(sed -n 's|^docker_host *= *"ssh://\(.*\)"|\1|p' "$HERE/deploy/config.toml" | head -1)
if [ -n "$NODE_SSH" ]; then
  echo "→ 推节点脚本到 $NODE_SSH"
  if ssh -o BatchMode=yes -o ConnectTimeout=8 "$NODE_SSH" 'mkdir -p ~/droidpool-node' 2>/dev/null; then
    scp -q "$HERE"/deploy/node/*.sh "$NODE_SSH:~/droidpool-node/" && \
      ssh -o BatchMode=yes "$NODE_SSH" 'chmod +x ~/droidpool-node/*.sh' && \
      echo "  已放到 ~/droidpool-node/（首次装节点跑 sudo bash ~/droidpool-node/setup-node.sh）"
  else
    echo "  ⚠ 连不上节点，跳过（节点脚本要手工 scp）"
  fi
fi

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
echo "✅ 部署完成: https://droidpool.daboshi.cn （设备墙） · http://192.168.14.32:8600 （API / CLI）"
