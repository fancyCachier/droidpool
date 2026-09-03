#!/usr/bin/env bash
# 给已安装的 cashier-app（debug 包）写入 Edge 端点 + 证书 pin，免走引导页。
# 在 agent 宿主机执行（需 adb、openssl）。写完 force-stop，下次启动生效。
# 用法: seed-edge.sh <ip:port> [edge_host] [edge_port] [package]
set -eu
DEV=$1; EH=${2:-192.168.14.53}; EP=${3:-8090}; PKG=${4:-cn.daboshi.cashier_app.dev}
ADB=${ADB:-adb}
if command -v sha256sum >/dev/null; then SHA="sha256sum"; else SHA="shasum -a 256"; fi
PIN=$(echo | openssl s_client -connect "$EH:$EP" 2>/dev/null | openssl x509 -outform DER 2>/dev/null | $SHA | cut -d' ' -f1)
[ -n "$PIN" ] || { echo "取不到 $EH:$EP 的证书 pin" >&2; exit 1; }
TMP=$(mktemp)
cat > "$TMP" <<XML
<?xml version='1.0' encoding='utf-8' standalone='yes' ?>
<map>
    <string name="flutter.edge_endpoint_v1">{"host":"$EH","port":$EP}</string>
    <string name="flutter.edge_cert_pins_v1">{"$EH:$EP":"$PIN"}</string>
</map>
XML
$ADB -s "$DEV" push "$TMP" /data/local/tmp/fsp.xml >/dev/null
# run-as 里相对路径的 cwd 不可靠，一律绝对路径 + push 后 cp
$ADB -s "$DEV" shell "run-as $PKG mkdir -p /data/data/$PKG/shared_prefs && run-as $PKG cp /data/local/tmp/fsp.xml /data/data/$PKG/shared_prefs/FlutterSharedPreferences.xml"
$ADB -s "$DEV" shell "am force-stop $PKG"
rm -f "$TMP"
echo "seeded $PKG -> $EH:$EP pin=$PIN"
