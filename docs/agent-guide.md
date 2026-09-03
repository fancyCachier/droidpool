# droidpool agent 使用指南

> 给在 worktree 里改 cashier-app 的 agent 看（Claude Code / dsh 均适用）。
> 一句话：**开工时 claim 一台独占设备，验证完 release**。不 claim 就直接 adb 连设备
> 会和别的 agent 撞车——那正是这个项目要消灭的问题。

## 0. 什么时候需要它

| 情况 | 用不用 |
|---|---|
| 改 cashier-app 的 UI / 交互 / 流程，要在 Android 上看效果 | **用** |
| 只改 Go 后端、只跑单元测试 | 不用 |
| 要测蓝牙锁、USB 打印机、扫码、微信登录 | **用不了**，见 §6 |
| 只改 Windows 桌面端 | 用不了（池里只有 Android） |

## 1. 一次完整流程

```bash
export DROIDPOOL_URL=http://192.168.14.32:8600      # 控制面
export DROIDPOOL_TOKEN=<找管理员要，在 devopt 的 /opt/droidpool/env>

cd .worktree/fix-3482                                # 必须在 worktree 里
droidpool claim                                      # 拿设备，自动 adb connect
# → 已分配 设备 3588-a-2
#   adb: 192.168.14.54:5562
#   租约: L1788... （到期 23:40:12）

cd cashier-app
flutter build apk --debug --target-platform android-arm64
droidpool run                                        # 装包 → 写 Edge 端点 → 启动 → 过引导页到登录页

# …驱动 UI 做验证…

droidpool release                                    # 用完必还
```

**所有 adb 命令都要带 `-s $(droidpool addr)`**。不带的话，本机若连着多台设备
adb 会报 `more than one device`，或者更糟——命令打到别人的设备上。

## 2. 命令速查

| 命令 | 作用 |
|---|---|
| `droidpool claim [--ttl 4h]` | 取一台设备。**幂等**：同一 worktree 重复调用返回同一台，不会占第二台 |
| `droidpool addr` | 打印 adb 地址，供 `-s` 使用 |
| `droidpool status` | 查租约。**人工接管中时以退出码 10 结束**（见 §4） |
| `droidpool heartbeat` | 发一次心跳，告诉 watchdog 自己还活着 |
| `droidpool watch &` | 持续心跳。跑长任务时挂后台 |
| `droidpool run [--apk 路径]` | **装包 → seed-edge → 启动 → 自动过引导页**，一步到登录页。默认 apk 路径是 `build/app/outputs/flutter-apk/app-debug.apk` |
| `droidpool seed-edge [--host --port]` | 单独写 Edge 端点 + 证书 pin（`run` 已包含） |
| `droidpool release` | 归还设备 |
| `droidpool devices` | 列出池里所有设备 |

## 3. 设备拿到手之后

**设备是干净的**：上一个租约归还时数据目录被清空并重建了容器。所以每次 claim 后
都要自己装包、写 Edge 端点、走一遍首启引导。

`droidpool run` 会自动过掉首启的两步引导（隐私政策「同意并继续」→ 设备角色「共享收银机」），
把你送到登录页。**登录（选员工、输 PIN）属于验证流程，由你自己驱动**，
参考 `bench/login_flow.sh`，它把从登录到首页走通了：

```bash
bench/login_flow.sh $(droidpool addr) ./out           # 退出码 0 = 到首页
```

**adb 驱动 cashier-app 的已知坑**（都踩过）：

- 数字键的 content-desc 就是 `"1"`，**必须精确匹配**。模糊匹配会命中「Edge 已连接 192.168.14.53」。
- 登录按钮的 content-desc 是 `登 录`，**中间有空格**。
- 首页导航是 `桌台包厢`，不是旧文档里的 `球台`。
- 员工卡 content-desc 含换行（`经\n经理\njingli`），用 `jingli` 片段匹配。
- OTA 弹窗「以后再说」在登录后 1~3 秒出现，流程里要轮询处理，**别点「立即更新」**。
- 登录按钮可能在首屏之下，要先按屏幕尺寸向上滑一段。
- `uiautomator dump` 单次约 2.6 秒，一个登录流程十几次就是半分钟。别在循环里无脑 dump。

## 4. watchdog：别被收走设备

控制面会**强制回收**租约，不问 agent 是否同意。三道闸：

| 闸 | 默认 | 触发条件 |
|---|---|---|
| 空闲超时 | 30 分钟 | 30 分钟没有任何 droidpool 命令碰过这个租约 |
| TTL | 4 小时 | 到了 claim 时约定的到期时间 |
| 生命周期上限 | 24 小时 | 持有总时长封顶，防止一直心跳赖着不走 |

**每条 droidpool 命令都会顺手发心跳**，所以正常干活不会被误杀。但如果你要做一件
超过 30 分钟且期间不调 droidpool 的事（长时间编译、大量思考），先挂个后台心跳：

```bash
droidpool watch &
WATCH_PID=$!
# …长任务…
kill $WATCH_PID
```

**心跳只证明活着，不等于续租。** TTL 到期照样收，需要更长时间就 claim 时给 `--ttl`。

设备被收走后，`droidpool status` 会报「租约已不存在」。这时重新 claim，**但设备已被
复位，之前装的包和登录态都没了**，要从 §3 重做。

## 5. 人工接管

操作人员可以在设备墙（`http://192.168.14.32:8600`）上接管任意一台设备，比如帮你扫码、
处理需要真人判断的弹窗。

**接管期间 agent 必须停手**，否则两边同时点屏幕会互相干扰。

```bash
droidpool status || {           # 退出码 10 = 人工接管中
  echo "人工接管中，等待交还…"
  until droidpool status; do sleep 10; done
}
```

驱动 UI 之前先查一次 status 是个好习惯。接管期间空闲闸不生效，不会因为你在等而被收走。

## 6. 池里测不了什么

不是配置问题，是 redroid 容器的物理限制。碰到这些直接找真机：

| 项 | 原因 |
|---|---|
| 微信登录 | 容器内没有微信 App，且 AppID 绑定签名 |
| 扫码 | 没有相机；ML Kit 依赖 GMS，容器里没有 |
| 蓝牙锁 K500Plus | 容器无蓝牙栈 |
| USB 打印机 / 扫码枪 | 无 USB 透传（网络打印机可以） |
| 32 位 native 插件问题 | 收银机实机是 armeabi-v7a，池里跑 arm64。UI 无差别，native 差异测不出 |
| 客显副屏 | 实机有 1024×600 第二显示，池里不模拟 |

## 7. 池的容量与拒绝

节点是一块 RK3588（8 核 / 16 GB）。实测上限：

| 口径 | 上限 |
|---|---|
| 同时活跃驱动（都在跑流程） | 10 台 |
| 常驻（app 挂首页不操作） | 12 台 |
| 生产配置 `max_devices` | 8（留余量给设备墙截图与人工操作） |

`claim` 可能被拒，两种情况都是正常的，**不是 bug**：

- **409 池满**：所有设备都被占着。等一会儿再试，或去设备墙看谁占着。
- **503 内存不足**：节点没余量再放一台。等别人 release。

CLI 会把这两种拒绝翻译成中文提示，不要当成错误往上抛。

## 8. 设备墙

`http://192.168.14.32:8600`（内网直连，不需要 token）

- **墙面**：所有设备的缩略图，标注谁在用、哪个 worktree、哪个分支、剩余时间。
- **点一格放大**：3 fps 实时画面，点击 = 点按，拖动 = 滑动，滚轮 = 上下滑。
  带返回/主页/任务三键、方向键、文本输入、截图保存。
- **接管 / 强制释放**：见 §5。

出问题时先看这里：设备是不是黑屏、app 是不是崩了、是不是有人在接管。

## 9. 排查

| 现象 | 多半是 |
|---|---|
| `claim` 报 401 | `DROIDPOOL_TOKEN` 没设或不对 |
| `claim` 说不是 git 仓库 | 没在 worktree 目录里执行 |
| `adb` 报 `device offline` | 设备正在复位。等 30 秒，或 `droidpool status` 看是否还持有 |
| app 装上但停在引导页 | `seed-edge` 没跑，或 Edge 证书换了导致 pin 不匹配 |
| 画面一直不变 | 设备墙上确认一下是不是 app 崩了 |
| 操作没反应 | 看设备墙是否有人接管中 |

## 10. 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `DROIDPOOL_URL` | `http://192.168.14.32:8600` | 控制面地址 |
| `DROIDPOOL_TOKEN` | 无，**必填** | 租约接口鉴权 |
| `DROIDPOOL_HEARTBEAT_SEC` | 60 | `watch` 的心跳间隔 |
