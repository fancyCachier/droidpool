# droidpool

redroid Android 设备池：给多个开发 agent（DeepSeek harness / Claude Code）并行验证 cashier-app 用的独占、干净、可丢弃的 Android 设备，附内网「设备墙」供操作人员观测、放大操作与接管。

- **agent 怎么用：[docs/agent-guide.md](docs/agent-guide.md)** ← 先看这个
- 路线图与全部决策：[docs/2026-09-03-roadmap.md](docs/2026-09-03-roadmap.md)
- 组件：`droidpoold`（控制面，跑 devopt）· `droidpool`（agent CLI）· redroid 容器（跑 .54 节点）· ws-scrcpy（放大操作）
- 语言：Go；面板 htmx 内嵌；全内网，不出公网

## 目录

```
docs/     路线图、基线报告
bench/    Phase 1 发烟与基线脚本（smoke.sh / login_flow.sh / sweep.sh）
cmd/      droidpoold、droidpool（Phase 2 起）
```

## 状态

- Phase 0 完成（2026-09-03）：.54 docker + 镜像、devopt SSH/docker CLI/adb、台账。
- 首次发烟通过（2026-09-03）：13 s boot、binderfs 容器内自举、Impeller(Vulkan/SwiftShader) 渲染正常、登录到首页跑通。记录：[docs/2026-09-03-phase0-smoke.md](docs/2026-09-03-phase0-smoke.md)
- Phase 1（性能基线与四个专项）进行中。

## bench 脚本

```bash
# 节点 .54 上：起容器并等 boot（IMAGE/WIDTH/HEIGHT/DPI 可用环境变量覆盖）
bench/redroid-up.sh redroid-1 5561
# agent 宿主机上：装包 → 写 Edge 端点 → 登录到首页并计时
adb connect 192.168.14.54:5561
adb -s 192.168.14.54:5561 install -r app-debug.apk
bench/seed-edge.sh 192.168.14.54:5561            # 默认 Edge 192.168.14.53:8090
bench/login_flow.sh 192.168.14.54:5561 ./out     # 退出码 0 = 到首页
```
