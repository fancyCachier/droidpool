#!/bin/bash
# 编译 redroid AOSP 树。在构建机上跑，普通用户身份（别用 root，会污染 out/）。
#
# 前提：AOSP 树 bind-mount 在 /src（soong 把绝对路径记进产物，增量构建必须
# 从同一路径进；见 README「/src bind mount」一节）。树本体在 /ssd/redroid/src，
# 起构建前先： sudo mount --bind /ssd/redroid/src /src
cd /src
source build/envsetup.sh
lunch redroid_arm64_only-userdebug || exit 1
# -j40：这台 56 核，留一部分核给机器上其它常驻服务。按机器核数自行调整。
m -j40
echo "=== BUILD rc=$? $(date -Is) ==="
