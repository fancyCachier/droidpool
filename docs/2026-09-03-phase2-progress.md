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

## 未完成

| 项 | 说明 |
|---|---|
| 健康检查循环 | 连续 3 次 `adb shell true` 失败转 broken，模型里已有字段与阈值，循环未接 |
| 设备墙（Phase 3） | htmx 页面、缩略图循环、SSE、ws-scrcpy 放大 |
| `seed-edge` / `run` 子命令 | CLI 目前只做租约，装包与写 Edge 端点仍靠 `bench/` 脚本 |
| `request-human` / `wait-human` | 服务端 `POST /api/leases/{id}/human` 已实现，CLI 侧未接 |
| 工作流挂点 | big-boss 的 take-issue / merge skill、dsh-plugins 插件 |
| 专项 C（overlayfs） | 已排队待跑，是零拷贝复位的前提；未验证前 `use_redroid_overlayfs` 只是配置默认值 |

## 部署形态（待做）

`droidpoold` 跑在 devopt（192.168.14.32:8600），systemd 常驻，bare 二进制（沿用 dev 环境 bare 部署约定）。CLI 分发 linux/amd64、linux/arm64、darwin/arm64 三个产物。
