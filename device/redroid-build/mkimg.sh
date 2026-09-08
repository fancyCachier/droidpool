#!/bin/bash
# 把 AOSP 产物打成 redroid docker 镜像。必须 root：要 loop-mount system.img /
# vendor.img，解包目录里没有 xattr（SELinux 标签、file capabilities）且属主全错。
set -e
D=/ssd/redroid/src/out/target/product/redroid_arm64_only
W=/ssd/redroid/pkg
TAG=${TAG:-droidpool/redroid:14-custom}

cleanup(){ umount "$W/vendor" 2>/dev/null || true; umount "$W/system" 2>/dev/null || true; }
trap cleanup EXIT

cleanup; rm -rf "$W"; mkdir -p "$W/system" "$W/vendor"
mount -o ro,loop "$D/system.img" "$W/system"
mount -o ro,loop "$D/vendor.img" "$W/vendor"

cd "$W"
# --platform 不可省：这台是 x86，docker import 默认把宿主架构写进元数据，
# 内容却是 arm64，拿到 arm64 节点上会报平台不匹配。
tar --xattrs -c vendor -C system --exclude="./vendor" . \
  | docker import --platform linux/arm64 \
      -c 'ENTRYPOINT ["/init", "androidboot.hardware=redroid"]' - "$TAG"

cleanup
rm -rf "$W"
docker images "$TAG" --format '打包完成: {{.Repository}}:{{.Tag}}  {{.Size}}'
