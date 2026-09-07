# 专项 B：GPU host 模式 spike（2026-09-06）

> 节点 .54（Orange Pi 5B / RK3588S / Mali-G610）· 镜像 `redroid/redroid:14.0.0_64only-latest`
> 目标：验证 `androidboot.redroid_gpu_mode=host` 能否把渲染从 SwiftShader 挪到真 GPU。

## 0. 结论

**跑不通，且不是配置问题，是三层各自独立地缺条件。** 按路线图 §4.1 第 8 条限时半天的口径，
这条路不成立；要做成不是一天的活，是换内核 + 自建 redroid 镜像的多周项目，且没有把握。

同时**路线图 §1.3 里「带 G610 补丁的 panfrost 已加载，`/dev/dri/renderD128`、`renderD129`」
这条实测记录是错的**——那两个 render node 不是 GPU。错误的前提正是这条路被估成「有尝试基础」的原因，
已在 `2026-09-03-roadmap.md` 就地更正。

顺带测出：**编码器不是瓶颈，渲染才是**（§3）。也就是说 GPU host 如果能通，方向确实对，
可惜堵在内核驱动那一层。

## 1. 三层为什么都不通

### 1.1 内核：GPU 归属专有 `mali` 驱动，没有 DRM render node

```
$ cat /sys/devices/platform/fb000000.gpu/gpuinfo
Mali-G610 4 cores r0p0 0xA867
$ readlink -f /sys/devices/platform/fb000000.gpu/driver
/sys/bus/platform/drivers/mali
$ ls /dev/mali0 /dev/dri/
/dev/mali0
/dev/dri/: by-path  card0  card1  renderD128  renderD129
```

GPU 绑在 Arm 专有的 `mali_kbase`（内建，非模块，`modinfo mali` 查不到），只暴露 `/dev/mali0`。
sysfs 下的 `csg_scheduling_period`、`firmware_config`、`mcu_shader_pwroff_timeout`
是 CSF（Command Stream Frontend）架构独有的，佐证 G610 走 CSF 而非 Job Manager。

**`/dev/dri` 里那两个 render node 与 GPU 无关**：

```
$ cat /sys/class/drm/renderD128/device/uevent | head -2
DRIVER=rockchip-drm          # 显示子系统
$ cat /sys/class/drm/renderD129/device/uevent | head -2
DRIVER=RKNPU                 # NPU
```

`panfrost.ko` 确实 loaded，但 refcount 0、没绑任何设备，而且它压根不支持 G610：

```
$ modinfo panfrost | grep alias
alias: of:N*T*Carm,mali-valhall-jm     # JM = Job Manager，即 G57/G68 一代
...（无 CSF / valhall-csf 别名）
```

驱动 CSF GPU 需要 `panthor`，本机内核 `6.1.0-1025-rockchip` 没有，
apt 源里能装到的也只有 6.1.0-1016 ~ 1026，全是同一条 BSP 线。

### 1.2 宿主用户态：只有 blob，与容器无关

```
/usr/lib/aarch64-linux-gnu/libmali-x11/libmali-valhall-g610-g13p0-x11-wayland-gbm.so
libgl1-mesa-dri  1:23.0.5-0ubuntu1~panfork~git221210...
```

宿主能用 GPU 靠的是 Rockchip 的 libmali 专有 blob。装着的 Mesa 是 panfork 23.0.5
（roadmap 里说的「G610 补丁」应该指的是它），但 panfork 要的 panfrost DRM node 不存在，所以它也没在用 GPU。
无论如何这两者都在宿主侧，redroid 容器用的是镜像自带的 Mesa，宿主装什么都不影响容器。

### 1.3 容器用户态：镜像里的 Mesa 最高只到 Mali-G57

```
$ docker exec droidpool-3588-a-1 strings /vendor/lib64/dri/panfrost_dri.so | grep -oE 'Mali-[A-Z0-9]+' | sort -u
Mali-G31 Mali-G51 Mali-G52 Mali-G57 Mali-G71 Mali-G72 Mali-G76
Mali-T620 Mali-T720 Mali-T760 Mali-T820 Mali-T830 Mali-T860 Mali-T880
```

Mesa **24.0.8**（镜像构建于 2024-05-27），genxml 里只有 `decode_jm.c`，没有 CSF 解码路径。
**没有 Mali-G610。** 就算内核那层解决了，这个镜像也认不出这块 GPU。
panfrost gallium 的 CSF/panthor 支持要 Mesa ≥ 24.2。

## 2. 实机验证：按路线图口径跑了一遍，记下报错

在空闲端口起容器（不进池，用完即删）：

```bash
docker run -d --privileged --name gpu-spike --device /dev/dri \
  -v /data/droidpool/gpuspike:/data -p 5580:5555 \
  redroid/redroid:14.0.0_64only-latest \
  androidboot.use_memfd=true \
  androidboot.redroid_width=1366 androidboot.redroid_height=768 \
  androidboot.redroid_dpi=160 \
  androidboot.redroid_gpu_mode=host androidboot.redroid_gpu_node=/dev/dri/renderD128
```

容器起来了，属性也确实切过去了（`ro.hardware.egl=mesa`、`ro.hardware.gralloc=gbm`、
`gralloc.gbm.device=/dev/dri/renderD128`），但 `sys.boot_completed` 始终为空，
`vendor.gralloc-2-0` 无限 restarting，SurfaceFlinger 起不来：

```
E GRALLOC-GBM: failed to create gbm device
E AllocatorHal: failed to open gralloc0 device: Invalid argument
W ServiceManagerCppClient: Waited one second for SurfaceFlingerAIDL (is service started? ...)
```

约每 5 秒一轮，无限循环。原因就是 §1.1：`renderD128` 是显示子系统的 node，
Mesa 的 gbm 在上面建不出 device。指到 `renderD129`（NPU）只会更糟，没测。

容器与数据目录已删除，节点回到 8 台的原状。

## 3. 附带发现：瓶颈在渲染，不在编码

原先的判断是「11 fps / 185 ms 里绝大部分耗在设备侧渲染加等编码器出帧」。做了组对照，
把编码这一半单独拆出来看。方法：在空闲设备 3588-a-8 上持续滑动制造动画，
用 `screenrecord` 走与 scrcpy 完全相同的 SurfaceFlinger → MediaCodec AVC 路径，
只改分辨率，比较出帧率与容器 CPU。

| 分辨率 | 像素比 | 8 s 内出帧 | 实际 fps | 容器 CPU |
|---|---|---|---|---|
| 1366×768 | 1× | 118 | 14.8 | 316 % |
| 480×270 | 1/8× | 121 | 15.0 | 303 % |

**像素砍到 1/8，帧率和 CPU 都没变。** 编码成本随像素走，它没动，说明编码不是限制项。

同一负载下容器内分进程 CPU（`top -b`，8 核 = 800 %）：

| 进程 | %CPU | 是什么 |
|---|---|---|
| `com.android.settings` | 149 % | 应用 HWUI 渲染，跑在 SwiftShader 上 |
| `surfaceflinger` | 69 % | 合成，同样在 SwiftShader 上 |
| `media.swcodec` | 63 % | 软件 H.264 编码 |
| `screenrecord` | 3 % | 取帧 |
| 其余 | < 3 % | |

渲染 + 合成 218 %，编码 63 %，**渲染是编码的 3.5 倍**。而 idle 还剩 445 %——
四个半核闲着。所以这不是算力不够，是**单帧串行延迟**：每一帧要依次过
HWUI-on-SwiftShader → SF 合成 → 编码，各环节自身并行度有限，堆核也补不上。

结论有两面：

- GPU host 的方向是对的，那 218 % 正是它能拿掉的部分，且是延迟大头；
- 但它堵在 §1，**近期拿不到**。而编码侧即使换硬件编码器（`/dev/mpp_service` 宿主是有的）
  也只够动 63 % 里的一部分，redroid 又没有 Rockchip 的 MediaCodec HAL，性价比更低。

另注：`cmd/droidpoold/main.go` 里 `MaxFPS: 15` 与实测天花板 14.8 fps 基本重合，
目前不构成额外限制；哪天渲染这层松了，这个上限要跟着抬。

## 4. 真要做成需要什么

| 层 | 需要 | 代价 / 风险 |
|---|---|---|
| 内核 | `panthor` 驱动（CSF），即 6.10+ 主线或 BSP 回移；还要把 `gpu@fb000000` 从 `mali` 手里交出来（DT 改动） | 换的是跑着 8 台生产容器的节点的内核；RK3588 主线化后 VPU 等外设可能回退 |
| 容器 | 自建 redroid 镜像，Mesa ≥ 24.2 带 panfrost CSF | redroid 官方镜像不带；自建 + redroid 的 GLES 栈（ANGLE→Vulkan）在 panthor 上无先例 |
| 验证 | 通了要重跑 Phase 1 基线（`bench/`）与并发扫描 | — |

任一层不通全盘不通，且第二层没有已知成功案例。**建议：不做。** 手感这条线上，
真想再进一步的话，性价比更高的是先看第二块板或 x86 节点（官方模拟器有 GPU 加速与硬件编码路径），
而不是在这块板上啃驱动。

## 5. 复现

```bash
# 事实核查
ssh sa@192.168.14.54 'cat /sys/devices/platform/fb000000.gpu/gpuinfo;
  readlink -f /sys/devices/platform/fb000000.gpu/driver;
  modinfo panfrost | grep alias;
  cat /sys/class/drm/renderD12{8,9}/device/uevent | grep DRIVER'
docker exec droidpool-3588-a-1 strings /vendor/lib64/dri/panfrost_dri.so \
  | grep -oE 'Mali-[A-Z0-9]+' | sort -u

# §3 的对照测量：见本文表格，脚本是临时的，方法已写在正文里
```
