# Phase 0 就绪 + 首次发烟记录（2026-09-03）

> 节点 .54（Orange Pi 5B / RK3588S / 16 GB / eMMC）· 控制面 devopt .32 · 镜像 `redroid/redroid:14.0.0_64only-latest`
> 结论：**Go**。redroid 在 .54 上一次起来，binderfs 容器内自举，Impeller（Vulkan/SwiftShader）渲染 cashier-app 完全正常，登录到首页跑通。

## 1. Phase 0 完成项

| 项 | 结果 |
|---|---|
| .54 docker | 29.7.2，`sa` 已入 docker 组 |
| .54 目录 | `/data/droidpool/{base,diff,smoke}` |
| 镜像拉取 | 675 MB，57 s（office 网直连 Docker Hub） |
| eMMC 基线（fio 4k randrw QD8 direct 20 s） | 读 3311 IOPS / 12.9 MB/s，写 3294 IOPS / 12.9 MB/s |
| devopt → .54 SSH | ed25519 免密通，`DOCKER_HOST=ssh://sa@192.168.14.54 docker ps` 正常 |
| devopt adb | 1.0.41 已装 |
| opt-manual 台账 | `office-3588-sa` tags 加 `droidpool-node`（commit 9d18ee9） |
| 空载温度 | 30~38°C |

## 2. 发烟结果

| 步骤 | 结果 | 备注 |
|---|---|---|
| 容器启动到 `sys.boot_completed=1` | **13 s** | 远好于 60 s 合格线 |
| binderfs | **容器内自举**：`/dev/binder → /dev/binderfs/binder`，宿主无需挂载 | Phase 0 的悬念解除 |
| 系统属性 | Android 14，`arm64-v8a`，`ro.hardware.egl=angle`，`ro.hardware.vulkan=pastel` | GLES 经 ANGLE 转 Vulkan，Vulkan 由 SwiftShader 软实现 |
| 多 adb server 并存 | Mac 与 devopt 同时 `adb connect` 同一容器，双方 screencap 均正常 | 发烟第 2 步通过 |
| `adb install -r` 190 MB debug apk | **3 s** | LAN + eMMC 顺序写不慢 |
| 冷启动 `am start -W` TotalTime | **4.4 s** | debug 构建 |
| 渲染器 | `Using the Impeller rendering backend (Vulkan)`，SwiftShader 设备 | **专项 A 通过**：隐私政策页 / 设备角色页 / 登录页 / 桌台首页均无花屏、字体与布局正确 |
| 登录流程 | 同意隐私 → 选「共享收银机」→ 选 jingli → PIN×6 → 登 录 → 「以后再说」（OTA 弹窗）→ 桌台包厢首页 | 首页可见台桌卡片、本班营收、实时计时 |
| screencap（2560×1600 RGBA PNG，3 MB） | 启动期 3.2 s；稳态 **0.77 s** ×3 | 稳态远优于 2 s 合格线 |
| `uiautomator dump` | **2.6 s** | 驱动流程的主要耗时来源 |
| 容器内存 | 启动完成 673 MiB；app 在首页时 **1.40 GiB** | app 自身 PSS 706 MB（debug） |
| 容器 CPU（app 停在首页，无操作） | **126 %**（约 1.3 核） | ⚠️ 见 §3.1 |
| `/data`（装完 app） | 192 MB | 基本是 apk + dex |
| 温度（一个容器跑 app 15 分钟后） | 38°C | 无散热压力 |

截图存于本机 scratchpad `smoke/`：`00-home`（Android 桌面）、`01-app`（隐私政策）、`13-login-state`（登录页）、`21-home`（桌台首页）。

## 3. 观察与对 Phase 1 的影响

### 3.1 空闲 CPU 126 % 是最大的变量

app 停在桌台首页什么都不做，容器就吃 1.3 核。首页有每秒跳动的计时卡片与「实时」指示，软渲染下每次重绘都是 CPU。这直接压 N_max：4 个容器都开着 app 就是 5 核起步，A76 只有 4 个。Phase 1 并发扫描要分两种状态测：① app 在首页空闲 ② app 在静态页（如设置页）空闲。若差异大，租约空闲时把 app 切到静态页或直接 `am force-stop`，可以把常驻容器数拉回 6~8。

### 3.2 dpi 选择要按目标机的逻辑像素定

2560×1600 @ 320 dpi 等于 1280×800 dp，登录页那张竖向卡片放不下，「登 录」按钮在首屏之下要滑一次才能点。Sunmi D1s 是 1366×768 @ 160 dpi = 1366×768 dp。Phase 2 的默认 `boot_args` 应按目标机 dp 尺寸定，候选：`2560x1600 @ 240 dpi`（1707×1067 dp，接近 10 寸平板）或直接 `1366x768 @ 160 dpi`（与 Sunmi 一致，截图也小 4 倍、screencap 更快）。建议池里支持两种 profile，claim 时选。

### 3.3 uiautomator dump 是驱动瓶颈

单次 2.6 s，一个登录流程要 dump 十几次。agent 侧 CLI 应缓存 dump、按需刷新，或改用 `flutter driver` / 无障碍事件流。Phase 2 的 `droidpool ui-dump` 先做最简版，优化放 Phase 4。

### 3.4 onboarding 比预期多一步

新装 app 首启：隐私政策 → **设备角色（共享收银机 / 我的个人设备）** → 登录页。golden 不预装 app（多宿主机 keystore 不同），所以每次 claim 后的首启都要过这两步。`droidpool run` 在 `am start` 后应自动点掉这两步（content-desc 稳定：`同意并继续`、`共享收银机`），把 agent 直接送到登录页。

### 3.5 adb 驱动的几个已确认细节

- 员工卡 content-desc 含换行（`经\n经理\njingli`），用 `jingli` 片段匹配。
- 数字键 content-desc 就是 `"1"`，必须精确匹配；模糊匹配会命中「Edge 已连接 192.168.14.53」。
- 登录按钮 content-desc 是 `登 录`（含空格）。
- 首页导航 content-desc 是 `桌台包厢`（不是旧配方里的 `球台`）。
- OTA 弹窗 `以后再说` 在登录后 1~3 s 内出现，流程要轮询处理。

### 3.6 bench 脚本已端到端复验

`pm clear` 清掉 app 数据后，`seed-edge.sh` → `login_flow.sh` 一次跑通（exit 0）：onboarding 17.5 s、登录到首页 41.3 s。这两个数含每次 `uiautomator dump` 约 2.6 s 的开销（流程里十余次），不是 app 本身的耗时；Phase 1 基线里 login_flow 要同时记「脚本墙钟」与「扣除 dump 的净值」。

## 4. 下一步（Phase 1）

按路线图 §4：把 `bench/` 三个脚本跑成基线（含 §3.1 的两种空闲状态），并发扫描 N ∈ {1,2,4,6,8}，专项 B（GPU host）、C（overlayfs 共享 data）、D（scrcpy 软编码）。当前 `redroid-smoke` 容器保留在 .54 上（端口 5561），可直接用来继续；清理用 `docker rm -f redroid-smoke`。
