# droidpool

redroid Android 设备池：给多个开发 agent（DeepSeek harness / Claude Code）并行验证 cashier-app 用的独占、干净、可丢弃的 Android 设备，附内网「设备墙」供操作人员观测、放大操作与接管。

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

Phase 0（节点与控制面就绪）进行中。
