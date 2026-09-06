#!/bin/sh
# 放在签证书的机器（office-gateway）上，作为 acme.sh 的 --reloadcmd：
# 签发/续签成功后把 fullchain + 私钥推给 droidpoold 所在机器。
# 远端 key 绑定了 forced command（recv-cert.sh），拿不到 shell。
#
# 首次签发（DNS-01，凭据已存在该机的 acme.sh 里）：
#   acme.sh --issue --server letsencrypt --dns dns_tencent -d droidpool.daboshi.cn \
#           --keylength ec-256 --reloadcmd /home/sa/bin/droidpool-deploy-cert.sh
set -eu
DOMAIN=${DOMAIN:-droidpool.daboshi.cn}
TARGET=${TARGET:-sa@192.168.14.32}
KEYFILE=${KEYFILE:-/home/sa/.ssh/id_ed25519_certdeploy}
CD="/home/sa/.acme.sh/${DOMAIN}_ecc"
tar cf - -C "$CD" fullchain.cer "$DOMAIN.key" \
  | ssh -i "$KEYFILE" -o BatchMode=yes -o ConnectTimeout=15 "$TARGET"
