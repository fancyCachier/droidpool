#!/usr/bin/env bash
# 给 redroid 的 AOSP 树加上外接摄像头 HAL。
#
# 用法: apply-camera.sh <AOSP-SRC>    例: apply-camera.sh /ssd/redroid/src
#
# 为什么是脚本而不是 patch 文件：device/redroid 是 remote-android 的仓库，
# 版本会动；按行号打 patch 很容易在换版本时失败，而这里要做的只是往
# redroid.mk 追加几行、往 manifest.xml 插一段，用幂等的方式追加更稳。
set -euo pipefail
SRC=${1:?用法: apply-camera.sh <AOSP-SRC>}
HERE=$(cd "$(dirname "$0")" && pwd)
DEV="$SRC/device/redroid"
[ -d "$DEV" ] || { echo "找不到 $DEV"; exit 1; }

# 1) 配置文件
install -m 644 "$HERE/external_camera_config.xml" "$DEV/external_camera_config.xml"

# 2) redroid.mk：HAL 服务 + 权限声明 + 配置
if grep -q 'etc/external_camera_config.xml' "$DEV/redroid.mk"; then
  echo "  redroid.mk 已含 camera 配置，跳过"
else
  cat >> "$DEV/redroid.mk" <<'MK'

# 外接摄像头：画面来自宿主的 v4l2loopback（RTSP → ffmpeg → /dev/videoN），
# 容器以 --device 拿到该节点。用 AOSP 现成的 external provider，不写 HAL 代码。
#
# 用 AIDL 版而不是 HIDL 的 @2.7-external-service：Android 14 的 camera
# provider 已转 AIDL，HIDL 那个模块虽然还在 Android.bp 里，但对这个 product
# 并没有产出（module-info.json 里只有 -V1-external-service 与
# @2.7-external-vsock-service）。写 HIDL 的话编译不报错，产物里却没有
# 二进制，要到设备上才发现 HAL 根本没起。
#
# 只拷 camera.external.xml：它本身就声明了 android.hardware.camera.any，
# 而 camera.any.xml 这个文件在 AOSP 14 里并不存在（写上去 ninja 直接报
# missing and no known rule to make it）。
PRODUCT_PACKAGES += \
    android.hardware.camera.provider-V1-external-service

PRODUCT_COPY_FILES += \
    frameworks/native/data/etc/android.hardware.camera.external.xml:$(TARGET_COPY_OUT_VENDOR)/etc/permissions/android.hardware.camera.external.xml \
    $(LOCAL_PATH)/external_camera_config.xml:$(TARGET_COPY_OUT_VENDOR)/etc/external_camera_config.xml
MK
  echo "  redroid.mk 已追加"
fi

# 2.5) media_profiles：redroid 不带这个文件，MediaProfiles 退回内置默认，
#      而默认只有 H.263/M4V 两个编码器、最大帧尺寸 352x288。老式 Camera1 API
#      用「视频编码器最大帧尺寸」当可录制尺寸上界过滤相机输出，1280x720 超界
#      被滤光，相机初始化失败报「无法连接到相机」（AOSP Camera2 应用走 Camera1）。
#      补一份带 h264/1080p 的进去，把上界抬过 720p。详见该 xml 的头注释。
install -m 644 "$HERE/media_profiles_V1_0.xml" "$DEV/media_profiles_V1_0.xml"
if grep -q 'media_profiles_V1_0.xml' "$DEV/redroid.mk"; then
  echo "  redroid.mk 已含 media_profiles，跳过"
else
  cat >> "$DEV/redroid.mk" <<'MK'

# media_profiles：没有它相机应用打不开（见 apply-camera.sh 注释）。
# 拷到 vendor/etc，命中 MediaProfiles 的搜索序（product/odm/vendor/system）。
PRODUCT_COPY_FILES += \
    $(LOCAL_PATH)/media_profiles_V1_0.xml:$(TARGET_COPY_OUT_VENDOR)/etc/media_profiles_V1_0.xml
MK
  echo "  redroid.mk 已追加 media_profiles"
fi

# 3) VINTF manifest：不声明的话 hwservicemanager 找不到这个 HAL，
#    表现为 cameraserver 起来了但 "Number of camera devices: 0"
if grep -q 'android.hardware.camera.provider' "$DEV/manifest.xml"; then
  echo "  manifest.xml 已含 camera provider，跳过"
else
  python3 - "$DEV/manifest.xml" <<'PY'
import io, sys
p = sys.argv[1]
s = io.open(p, encoding='utf-8').read()
frag = '''    <hal format="aidl">
        <name>android.hardware.camera.provider</name>
        <version>1</version>
        <fqname>ICameraProvider/external/0</fqname>
    </hal>
</manifest>'''
assert s.count('</manifest>') == 1, 'manifest.xml 结构意外'
io.open(p, 'w', encoding='utf-8').write(s.replace('</manifest>', frag))
PY
  echo "  manifest.xml 已插入 camera provider"
fi
echo "✅ camera 改动已应用到 $SRC"
