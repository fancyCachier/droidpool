# 构建自建 redroid 镜像 `droidpool/redroid:14-custom`

官方 redroid 镜像没有摄像头，也不带我们需要的几处修正。这个目录是**从零复现**
`droidpool/redroid:14-custom` 的完整编排：拉源码 → 打补丁 → 编译 → 打包成 docker 镜像。
补丁本体在同仓库的 [`device/redroid-patches/`](../redroid-patches/)。

## 构建机前提

- x86_64 Linux（我们用 Ubuntu），装好 AOSP 构建依赖、`repo`、`docker`。
- 磁盘足够放整棵 AOSP 树 + 产物（几百 GB）。
- 打包（`mkimg.sh`）要 root：它 loop-mount `system.img` / `vendor.img`，普通用户挂不了。
- 编译（`build.sh`）用**普通用户**，不要 root（root 跑会污染 `out/` 的属主）。

我们这台的布局（脚本里的路径按此写死，换机器改路径即可）：

| 路径 | 用途 |
|---|---|
| `/ssd/redroid/src` | AOSP + redroid 源码树 |
| `/ssd/redroid/pkg` | `mkimg.sh` 的临时挂载/打包目录 |
| `/ssd/redroid/bin` | `repo` 等工具（进 PATH） |
| `/src` | **`/ssd/redroid/src` 的 bind mount**，编译时的工作目录 |

### /src bind mount（增量构建的坑）

soong 会把**绝对路径**记进 `out/`（实测 `soong.log` 里 `PWD=/src`）。所以增量构建
必须从记录时的同一路径进，否则 ninja 找不到输入、退化成全量重编甚至报错。起构建前：

```bash
sudo mkdir -p /src && sudo mount --bind /ssd/redroid/src /src
```

bind mount 不持久，重启后要重挂。`build.sh` 里 `cd /src` 就是走这个。

## 步骤

### 1. 拉源码

```bash
mkdir -p /ssd/redroid/src && cd /ssd/redroid/src
repo init -u https://github.com/remote-android/platform_manifests.git -b redroid-14.0.0_r2
# 基于 AOSP android-14.0.0_r2 + redroid overlay。无 local_manifests。
bash /path/to/device/redroid-build/run-sync.sh      # 限流重试同步
bash /path/to/device/redroid-build/run-sync2.sh     # 收尾（--force-sync）
```

`run-sync.sh` / `run-sync2.sh`：`repo sync` 遇 GitHub/googlesource 429 限流会反复续传，
本身幂等可续。

### 2. 打补丁（顺序如下，每个都幂等、可重复跑）

把本仓库的 `device/redroid-patches/` 拷到构建机（或直接 clone 本仓库），依次执行，
参数是 AOSP 树路径（用 `/src` 或 `/ssd/redroid/src` 均可）：

```bash
P=/path/to/device/redroid-patches
bash $P/apply-camera.sh            /src   # 外接相机 HAL：redroid.mk 包/权限/manifest
                                          #   + external_camera_config.xml + media_profiles
bash $P/apply-camera-hal-fix.sh    /src   # HAL 源码三修：析构竞态 / kMaxBytesPerPixel / 预mmap
bash $P/apply-gralloc-camera-fix.sh /src  # guest gralloc：加相机格式 + lock_ycbcr
```

每个脚本头部有详细注释说明它修什么、为什么。逐条原因见仓库 commit 与
`docs/`、以及各 `apply-*.sh` 顶部。

### 3. 编译

```bash
sudo mount --bind /ssd/redroid/src /src   # 见上「/src bind mount」
bash /path/to/device/redroid-build/build.sh 2>&1 | tee /ssd/redroid/build.log
# 结尾打印 === BUILD rc=0 ... === 即成功
```

### 4. 打包成 docker 镜像

```bash
sudo /path/to/device/redroid-build/mkimg.sh
# 产出 droidpool/redroid:14-custom（arm64）。改 TAG=... 可换标签。
```

`mkimg.sh` 关键点：`--platform linux/arm64` 不可省（构建机是 x86，`docker import`
默认把宿主架构写进元数据，内容却是 arm64，到 arm64 节点会报平台不匹配）；
`tar --xattrs` 保留 SELinux 标签与 file capabilities。

### 5. 分发到节点

镜像用 [`deploy/node/pull-image.sh`](../../deploy/node/pull-image.sh) **在节点侧**直拉
（节点→构建机 55 MB/s 直达，别从开发机中转）。节点侧前提（v4l2loopback、tun、udev
规则等）由 [`deploy/node/setup-node.sh`](../../deploy/node/setup-node.sh) 装。

## 验证

镜像上到节点后，摄像头端到端验证（相机注册、抓拍、YUV 取帧）见相机相关 commit
与 `docs/`。已知限制：屏上实时预览需要硬件 GPU（本节点的 Mali 未接入 redroid），
扫码/抓拍走数据通路不受影响。
