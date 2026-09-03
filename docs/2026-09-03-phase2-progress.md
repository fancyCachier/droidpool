# Phase 2 进度：池化与分配接口（2026-09-03）

> 路线图见 `2026-09-03-roadmap.md` §5，默认值取自 `2026-09-03-phase1-baseline.md`

## 已完成

| 包 | 职责 | 测试 |
|---|---|---|
| `internal/pool` | 设备状态机、租约模型、租约回收器、设备管理器 | 20 个 |
| `internal/config` | TOML 配置加载与校验 | 5 个 |
| `internal/store` | SQLite 持久化（设备/租约、幂等 claim、TTL） | 10 个 |
| `internal/node` | docker over SSH 节点驱动、健康快照 | 11 个 |
| `internal/api` | HTTP 接口 + Bearer 鉴权 + swap 准入闸 | 12 个 |
| `cmd/droidpoold` | 控制面守护进程 | 已对真实节点冒烟 |
| `cmd/droidpool` | agent 侧 CLI | 已端到端跑通 |

合计 58 个测试（子测试单独计），`go vet` 干净，全部在 `-race` 下通过。实现 1669 行、测试 1564 行。

## 关键设计决定（都来自 Phase 1 实测）

**准入闸看 swap，不看 CPU 或温度。** Phase 1 实测 CPU 在 N=4 就封顶在 900 %、温度全程不超过 70 °C，两者都无预警价值；只有 swap 从 0 变正与失败率拐点重合（N=10 零失败 → N=12 失败率 5.6 %）。`swap_guard_mib` 默认 256。

**幂等复用不受准入闸限制。** 已持有租约的 agent 重复 claim 不占新设备，节点换页时仍放行——否则它连自己的 adb 地址都查不到。这条有专门的测试守着。

**健康探测失败不等于节点有压力。** SSH 不通时放行 claim，不能让探测故障连累正常业务。

**TTL 到期强制回收，不问 agent。** agent 异常退出是常态，这是唯一可靠的回收路径。回收后设备进 `resetting`，由管理器重建容器（overlay 模式下等价于丢弃 diff）再放回池子。单条租约回收失败不影响其余——一台卡住的设备不该让整个池子停摆。

**归还必须经过 resetting，不能直接回 ready。** 状态机在库层强制：`leased → ready` 是非法转移。否则上一个 agent 的构建会留给下一个，正是本项目要解决的原问题。

**唯一索引保证两条不变量**：同一 `(host, worktree)` 只占一台设备；一台设备只有一个活跃租约。已归还的行不参与约束（部分索引 `WHERE released_at = 0`）。

## 测试方法

每个包写完后做变异校验：故意改坏被测代码，确认相关测试变红，再还原。共做 19 处变异，全部有效。

**踩过的两个坑，记下来**：

1. **编译不过的变异等于没做变异。** 删掉一段 `if` 后 `d, err` 变成未使用变量，`go test` 输出的是构建错误而非测试结果，差点被当成「测试通过」。变异后必须先 `go build` 确认能编译，再看测试是否变红。
2. **测试没覆盖到的边界，变异会沉默。** `expires_at<=?` 改成 `<` 时测试全绿，说明「恰好到期」这个边界没测。补上边界用例后变异才正确变红。不做变异校验就发现不了这个缺口。

## 2026-09-04 收尾：部署 + 三处真环境暴露的洞

**已部署到 devopt**（192.168.14.32:8600，systemd `droidpoold`，`deploy/deploy.sh` 幂等）。
token 在 `/opt/droidpool/env`，配置支持 `${VAR}` 展开，不进仓库。

首次上真环境暴露三个本地测试发现不了的洞，都已修并有测试守着：

| 洞 | 现象 | 修法 |
|---|---|---|
| **启动阻塞** | `Ensure` 在监听前建 8 台容器，端口迟迟不开，systemd 判死 | 先监听，补池放后台 goroutine |
| **release 后卡 resetting** | `Release` 只改状态，没人去复位；设备永远 resetting | `Server.Resetter` 后台触发 `Reset` |
| **库与节点脱节** | 重启后库说 ready 但节点没容器（一直分死设备给人）；卡在 resetting 无人接手 | 启动时 `ReconcileStore`：无容器的标 broken，中间态的重新复位 |

另外补齐：
- **golden 首次实现**（路线图写了但代码一直没有），空 base 会让 overlay 挂载失败
- **Reconcile 节点容器**：清孤儿与占端口的容器（基线测试残留的 redroid-N 让整个池起不来）
- **健康检查循环**：连续 3 次探活失败标 broken 并重建，中途恢复清零；真环境里 a-5 容器丢失后 90 s 内被正确判死重建
- **CLI `run` / `seed-edge`**：装包 → 写 Edge 端点 → 启动 → 自动过两步引导，对真实 devopt 端到端 29 s 到登录页
- 开源前审计（另一会话）：移除误提交的 3.5 MB 二进制、加 Apache-2.0、文档去人名

## 仍未做

| 项 | 说明 |
|---|---|
| `request-human` / `wait-human` CLI | 服务端接口已有，CLI 未接 |
| `shot` / `ui-dump` CLI | agent 现在直接用 adb，够用 |
| dsh-plugins 插件 | Claude Code 侧 skill 已挂进 take-issue / merge，dsh 侧一行没写 |
| H.264 路并发路数限制 | `maxStreams=4` 只在 MJPEG 路上；H.264 路同设备后来者接管，但跨设备无上限 |
| 专项 B GPU host | 唯一能再压延迟的路径，Phase 4 |
