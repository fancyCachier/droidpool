# 设备墙 HTTPS 与证书链路（2026-09-06）

> **2026-09-10 起 TLS 改由同机 nginx 终止**，§1 的拓扑与 §2 的「换证」一行已被 §5 取代；证书的签发与推送链路不变。

> 起因见 `2026-09-03-远程操作方案对比.md` §5.6.2：WebCodecs 只在安全上下文里存在，
> `http://192.168.14.32:8600` 上的放大视图只有 3 fps 截图流。

## 1. 拓扑

```
浏览器 ──https──▶ droidpool.daboshi.cn:443 ──▶ droidpoold [tls]（devopt 192.168.14.32）
agent CLI / MCP ──http──▶ 192.168.14.32:8600 ──▶ 同一个 droidpoold（API 不变）
```

- DNS：`droidpool.daboshi.cn` A → `192.168.14.32`，DNSPod 公网记录指向内网 IP，只在 918 内网可达。
- `wall_url` 设了之后，走 http 打开的 `/` 与 `/device/{id}` 302 到 https；`/api/*` 不跳转。
- 443 由 systemd 的 `AmbientCapabilities=CAP_NET_BIND_SERVICE` 放行给 `sa`。

## 2. 证书从哪来、怎么到

| 环节 | 在哪 | 是什么 |
|---|---|---|
| 签发 / 续签 | office-gateway（192.168.14.30）`~sa/.acme.sh/droidpool.daboshi.cn_ecc/` | acme.sh，Let's Encrypt，DNS-01 `dns_tencent`（凭据早已存在 acme.sh 里，与 `*.big-boss.cn` 同一套），ec-256 |
| 推送 | office-gateway `~/bin/droidpool-deploy-cert.sh` = `deploy/cert/push-cert.sh` | acme.sh 的 `Le_ReloadCmd`：`tar fullchain.cer + key \| ssh -i ~/.ssh/id_ed25519_certdeploy sa@192.168.14.32` |
| 接收 | devopt `/opt/droidpool/bin/recv-cert.sh` = `deploy/cert/recv-cert.sh` | `authorized_keys` 里该 key 绑定的 forced command（`restrict`），对端拿不到 shell；校验证书与私钥配对后写 `/opt/droidpool/tls/{privkey,fullchain}.pem` |
| 换证 | droidpoold `internal/certfile` | 每次握手最多每 30 s stat 一次 `fullchain.pem`，mtime 变了就重载；新文件坏了继续用旧的并打 warn |

续签由 gateway 上已有的 acme.sh cron（每天 06:53）负责；ARI 给出的下次续签时间 **2026-11-05**。
消费者只有 droidpoold 一处，不像 `*.big-boss.cn` 有四处。

## 3. 一次性安装（已做，记下来以便重装）

devopt：

```bash
mkdir -p /opt/droidpool/{bin,tls} && chmod 700 /opt/droidpool/tls
install -m 755 deploy/cert/recv-cert.sh /opt/droidpool/bin/recv-cert.sh      # deploy.sh 也会做
# gateway 的推送 key（~/.ssh/id_ed25519_certdeploy.pub）绑 forced command：
echo 'command="/opt/droidpool/bin/recv-cert.sh",restrict ssh-ed25519 AAAA… acme-cert-deploy@office-gateway' >> ~/.ssh/authorized_keys
```

office-gateway：

```bash
install -m 700 deploy/cert/push-cert.sh ~/bin/droidpool-deploy-cert.sh
~/.acme.sh/acme.sh --issue --server letsencrypt --dns dns_tencent -d droidpool.daboshi.cn \
    --keylength ec-256 --reloadcmd /home/sa/bin/droidpool-deploy-cert.sh
```

DNS（本机，凭据 `opt-manual/secret/dns.secret`）：

```bash
tccli dnspod CreateRecord --Domain daboshi.cn --SubDomain droidpool --RecordType A \
    --RecordLine 默认 --Value 192.168.14.32 --TTL 600 --secretId "$SID" --secretKey "$SKEY" --token ""
```

`--token ""` 不能省：本机 tccli 全局配置里躺着一个过期的 OAuth token，不显式清空会报
`AuthFailure.TokenFailure`，看起来像是 SecretId/SecretKey 错了，其实不是。

## 4. 排查

```bash
# 对外拿到的是哪张证书
echo | openssl s_client -connect 192.168.14.32:443 -servername droidpool.daboshi.cn 2>/dev/null \
  | openssl x509 -noout -subject -issuer -dates
# 文件里的是哪张
ssh office-devopt 'openssl x509 -in /opt/droidpool/tls/fullchain.pem -noout -dates'
# 手动推一次（续签后没到位时）
ssh office-gateway '~/bin/droidpool-deploy-cert.sh'
# droidpoold 有没有换上
ssh office-devopt 'sudo journalctl -u droidpoold --no-pager | grep -E "换用新证书|证书文件变了"'
```

| 现象 | 看哪 |
|---|---|
| 页面还是「截图流」 | 地址栏是不是 http；`typeof VideoDecoder` 在控制台是不是 undefined |
| 浏览器报证书过期 | gateway 上 `acme.sh --list` 看续签时间；推送是否失败（cron 输出）；devopt `tls/` 的 mtime |
| 推送失败 `Permission denied` | devopt `authorized_keys` 里那行 forced command 还在不在 |
| droidpoold 起不来「HTTPS 证书」 | 配置开了 `[tls]` 但 `tls/` 没文件；deploy.sh 会先拦 |

## 5. 2026-09-10 起：nginx 统一终止 TLS

同机要多挂一个 HTTPS 服务（另一个域名），`:443` 不能再由 droidpoold 独占，改为：

```
浏览器 ──https──▶ nginx :443 ──按 SNI/Host──▶ droidpool.daboshi.cn → droidpoold http 127.0.0.1:8600
                                            └▶ （同机另一个服务的域名）→ 它自己的本机端口
agent CLI / MCP ──http──▶ 192.168.14.32:8600 ──▶ droidpoold（不变）
```

- **droidpoold 不再开 `[tls]`**（`deploy/config.toml` 已删）。`wall_url` 的跳转条件是 `r.TLS == nil`，放在反代后面恒成立、会无限重定向，所以连同 `wall_url` 一起去掉；
  代价是直接打开 `http://192.168.14.32:8600/` 不再自动跳到 https。
- **证书**：acme.sh 那张重签成同时覆盖 `droidpool.daboshi.cn` 与另一个域名（仍是 `droidpool.daboshi.cn_ecc`，DNS-01，同一个推送钩子），下次续签 2026-11-09。
- **换证**：推送与接收不变（`recv-cert.sh` 仍写 `/opt/droidpool/tls/`）；由 systemd path 单元盯 `fullchain.pem`，变了就 `nginx -t && systemctl reload nginx`。
- `droidpoold.service` 里的 `CAP_NET_BIND_SERVICE` 已用不到，留着无害。

droidpool 这一段 nginx server（完整配置还含另一个服务的 server 与 :80 跳转）：

```nginx
map $http_upgrade $connection_upgrade { default upgrade; '' close; }

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name droidpool.daboshi.cn;
    ssl_certificate     /opt/droidpool/tls/fullchain.pem;
    ssl_certificate_key /opt/droidpool/tls/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:8600;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header Upgrade $http_upgrade;          # /api/devices/{id}/ws 的 H.264
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;                             # 设备墙的 SSE 与 MJPEG 截图流
        proxy_read_timeout 1h;
        proxy_send_timeout 1h;
    }
}
```

切换后实测：设备墙 8 台缩略图正常刷新；放大页 `wss://droidpool.daboshi.cn/api/devices/3588-a-1/ws` 8 秒收到 21 帧（H.264，WebCodecs 可用）；
`http://droidpool.daboshi.cn/` 301 到 https；未知 SNI 在握手阶段被拒（`ssl_reject_handshake`）。

排查（替代 §4 里 droidpoold 换证那条）：

```bash
ssh office-devopt 'systemctl status nginx-reload-cert.path; sudo journalctl -u nginx-reload-cert --no-pager | tail'
echo | openssl s_client -connect 192.168.14.32:443 -servername droidpool.daboshi.cn 2>/dev/null | openssl x509 -noout -ext subjectAltName -dates
```
