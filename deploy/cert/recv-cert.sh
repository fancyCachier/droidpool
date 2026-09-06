#!/bin/sh
# 在 droidpoold 所在机器上接收证书（office-gateway 的 acme.sh 续签钩子 → tar over ssh）。
# 作为 authorized_keys 里的 forced command 运行，对端拿不到 shell：
#   command="/opt/droidpool/bin/recv-cert.sh",restrict ssh-ed25519 AAAA... acme-cert-deploy@office-gateway
# 只接受一个含 fullchain.cer 与 *.key 的 tar；证书与私钥配对校验通过才落盘。
# droidpoold 看 fullchain.pem 的 mtime 自动换证，不需要重启。
set -eu
DST=${DROIDPOOL_TLS_DIR:-/opt/droidpool/tls}
umask 077
TMP=$(mktemp -d "$DST/.incoming.XXXXXX")
trap 'rm -rf "$TMP"' EXIT
tar xf - -C "$TMP"
CERT=$(find "$TMP" -maxdepth 1 -name fullchain.cer | head -1)
KEY=$(find "$TMP" -maxdepth 1 -name '*.key' | head -1)
[ -n "$CERT" ] && [ -n "$KEY" ] || { echo "tar 里缺 fullchain.cer 或 *.key" >&2; exit 1; }
# 证书与私钥必须配对；不配对 droidpoold 会拒载并继续用旧证书，但问题要在这里就报出来
CPUB=$(openssl x509 -in "$CERT" -noout -pubkey)
KPUB=$(openssl pkey -in "$KEY" -pubout)
[ "$CPUB" = "$KPUB" ] || { echo "证书与私钥不匹配" >&2; exit 1; }
chmod 600 "$CERT" "$KEY"
# 私钥先就位、证书最后放：droidpoold 以证书 mtime 为换证信号，此时私钥已是新的
mv -f "$KEY" "$DST/privkey.pem"
mv -f "$CERT" "$DST/fullchain.pem"
touch "$DST/privkey.pem" "$DST/fullchain.pem"   # tar 会保留源文件的 mtime，显式刷成现在
echo "已更新 ${DST}：$(openssl x509 -in "$DST/fullchain.pem" -noout -enddate)"
