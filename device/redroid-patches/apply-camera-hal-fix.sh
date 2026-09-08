#!/usr/bin/env bash
# 修 AOSP external camera HAL 在 redroid + v4l2loopback 组合下才暴露的三处问题。
# 改两个文件（同一 session 的 .h / .cpp），每段各自幂等，重复跑安全。
#
# 用法: apply-camera-hal-fix.sh <AOSP-SRC>    例: apply-camera-hal-fix.sh /ssd/redroid/src
#
# --- 修正 1：析构顺序竞态（.h）---
# 症状：应用一打开相机，HAL 进程 abort，报「无法连接到相机」。
#   FORTIFY: pthread_mutex_lock called on a destroyed mutex
#     ExternalCameraDeviceSession::BufferRequestThread::waitForNextRequest()
# BufferRequestThread 继承 SimpleThread，后者在自己析构里才 join，此时 mLock 已被
# 派生类析构毁掉，而线程还在 waitForNextRequest() 用它。同文件 OutputThread、
# provider 的 HotplugThread 都在自己析构里先 requestExitAndWait()，只有它漏了。
#
# --- 修正 2：v4l2loopback 的 sizeimage 被判成非法（.h）---
# 症状：修正 1 后不再崩，但预览起不来：
#   ExtCamDevSsn: V4L2 buffer size: 3686400 looks invalid. Expected maximum size: 1843200
# configureV4l2StreamLocked 的闸门要求 sizeimage <= kMaxBytesPerPixel(2)*w*h。
# v4l2loopback 对 MJPG 按 32 位保守申报 sizeimage=4*w*h（1280x720→3686400），超闸。
# 该上界随分辨率同比缩放，改喂流分辨率无用，只能放宽闸门。抬到 4（RGBA 最坏值）。
#
# --- 修正 3：DQBUF 要求 buffer 已 mmap（.cpp + .h）---
# 症状：修正 2 后流配置仍失败：
#   ExtCamDevSsn: configureV4l2StreamLocked: DQBUF fails: Invalid argument
#   Camera3-Device: configureStreamsLocked: Unable to configure streams with HAL: -38
# v4l2loopback 的 vidioc_dqbuf 对 capture 有这一句：
#   if (!(dev->buffers[index].buffer.flags & V4L2_BUF_FLAG_MAPPED)) return -EINVAL;
# 即只返回**已 mmap** 的 buffer。真实 UVC 驱动不要求 dqbuf 前先 mmap，所以这个
# HAL 对真机没问题——它是在取帧时才惰性 mmap（V4L2Frame::map）。但 v4l2loopback
# 要求先 mmap，于是 STREAMON 后的首个 DQBUF 因 buffer 未 mmap 直接 EINVAL。
# 修法：在 configureV4l2StreamLocked 的 QUERYBUF 之后、QBUF 之前，就把每个 buffer
# mmap 一次（PROT_READ/MAP_SHARED，与 V4L2Frame::map 同参），令其带上
# V4L2_BUF_FLAG_MAPPED，并在 stopV4l2StreamingLocked 里 munmap。这份预映射在整个
# 推流期间保活，与 V4L2Frame 每帧各自的 mmap/unmap 并存互不影响。
set -euo pipefail
SRC=${1:?用法: apply-camera-hal-fix.sh <AOSP-SRC>}
D="$SRC/hardware/interfaces/camera/device/default"
H="$D/ExternalCameraDeviceSession.h"
CPP="$D/ExternalCameraDeviceSession.cpp"
[ -f "$H" ] || { echo "找不到 $H"; exit 1; }
[ -f "$CPP" ] || { echo "找不到 $CPP"; exit 1; }

# 修正 1：BufferRequestThread 补析构函数
if grep -q '~BufferRequestThread' "$H"; then
  echo "  [1/3] 析构竞态：已打过，跳过"
else
  python3 - "$H" <<'PY'
import io, sys
p = sys.argv[1]
s = io.open(p, encoding='utf-8').read()
anchor = """        BufferRequestThread(std::weak_ptr<OutputThreadInterface> parent,
                            std::shared_ptr<ICameraDeviceCallback> callbacks);
"""
add = """
        // 必须在**派生类**析构里先停线程再让成员销毁。SimpleThread 是在它自己的
        // 析构里 join 的，那时 mLock / mRequestCond 已经被这个类的析构毁掉了，
        // 而 waitForNextRequest() 里的线程还在用它们，于是
        //   FORTIFY: pthread_mutex_lock called on a destroyed mutex
        // 同文件的 OutputThread 与 provider 的 HotplugThread 都是这么做的，
        // 只有这个类漏了。
        ~BufferRequestThread() override { requestExitAndWait(); }
"""
assert s.count(anchor) == 1, 'BufferRequestThread 构造函数锚点没找到，AOSP 版本变了？'
io.open(p, 'w', encoding='utf-8').write(s.replace(anchor, anchor + add))
print("  [1/3] 析构竞态：已给 BufferRequestThread 加上析构函数")
PY
fi

# 修正 2：放宽 kMaxBytesPerPixel 2 → 4
if grep -q 'kMaxBytesPerPixel = 4' "$H"; then
  echo "  [2/3] buffer 闸门：已放宽，跳过"
else
  python3 - "$H" <<'PY'
import io, sys
p = sys.argv[1]
s = io.open(p, encoding='utf-8').read()
old = 'static const uint32_t kMaxBytesPerPixel = 2;'
new = ('static const uint32_t kMaxBytesPerPixel = 4;  '
       '// redroid: v4l2loopback 对 MJPG 按 32 位申报 sizeimage(4*w*h)，'
       '原值 2 会把它判成非法致预览失败，见本脚本头注释')
assert s.count(old) == 1, 'kMaxBytesPerPixel = 2 锚点没找到，AOSP 版本变了？'
io.open(p, 'w', encoding='utf-8').write(s.replace(old, new))
print("  [2/3] buffer 闸门：kMaxBytesPerPixel 已 2 → 4")
PY
fi

# 修正 3：预 mmap 每个 V4L2 buffer（.cpp 三处 + .h 一处）
if grep -q 'mV4l2PremapAddrs' "$H"; then
  echo "  [3/3] DQBUF 预映射：已打过，跳过"
else
  python3 - "$H" "$CPP" <<'PY'
import io, sys
hp, cp = sys.argv[1], sys.argv[2]

# --- .h：加成员 ---
h = io.open(hp, encoding='utf-8').read()
h_anchor = '    size_t mV4L2BufferCount = 0;\n'
h_add = ('''
    // redroid/v4l2loopback：configureV4l2StreamLocked 里对每个 V4L2 buffer 预先
    // mmap 得到的地址。v4l2loopback 的 dqbuf 只返回带 V4L2_BUF_FLAG_MAPPED 的
    // buffer，故必须在 DQBUF 前先 mmap；整个推流期间保活，stop 时 munmap。
    std::vector<void*> mV4l2PremapAddrs;
''')
assert h.count(h_anchor) == 1, 'mV4L2BufferCount 成员锚点没找到'
io.open(hp, 'w', encoding='utf-8').write(h.replace(h_anchor, h_anchor + h_add))

# --- .cpp：include ---
c = io.open(cp, encoding='utf-8').read()
inc_anchor = '#include <linux/videodev2.h>\n'
inc_add = '#include <sys/mman.h>\n'
assert c.count(inc_anchor) == 1, 'linux/videodev2.h include 锚点没找到'
assert '#include <sys/mman.h>' not in c, 'sys/mman.h 已存在？'
c = c.replace(inc_anchor, inc_anchor + inc_add)

# --- .cpp：QUERYBUF 之后、QBUF 之前，预 mmap ---
qbuf_block = '''        if (TEMP_FAILURE_RETRY(ioctl(mV4l2Fd.get(), VIDIOC_QBUF, &buffer)) < 0) {
            ALOGE("%s: QBUF %d failed: %s", __FUNCTION__, i, strerror(errno));
            return -errno;
        }
'''
premap = '''        // redroid/v4l2loopback：先 mmap 本 buffer，使其带上 V4L2_BUF_FLAG_MAPPED，
        // 否则 v4l2loopback 的 vidioc_dqbuf 会以「not mapped」返回 EINVAL，
        // 流配置失败。参数与 V4L2Frame::map 一致；munmap 在 stopV4l2StreamingLocked。
        void* premapAddr = mmap(nullptr, buffer.length, PROT_READ, MAP_SHARED, mV4l2Fd.get(),
                                buffer.m.offset);
        if (premapAddr == MAP_FAILED) {
            ALOGE("%s: premap buffer %d failed: %s", __FUNCTION__, i, strerror(errno));
            return -errno;
        }
        mV4l2PremapAddrs.push_back(premapAddr);

'''
assert c.count(qbuf_block) == 1, 'config 循环的 QBUF 块锚点没找到（应唯一）'
c = c.replace(qbuf_block, premap + qbuf_block)

# --- .cpp：stopV4l2StreamingLocked 里 munmap ---
stop_anchor = '''    mV4L2BufferCount = 0;

    // VIDIOC_STREAMOFF
'''
stop_add = '''    mV4L2BufferCount = 0;

    // redroid/v4l2loopback：解除 configureV4l2StreamLocked 里的预 mmap
    for (void* addr : mV4l2PremapAddrs) {
        munmap(addr, mMaxV4L2BufferSize);
    }
    mV4l2PremapAddrs.clear();

    // VIDIOC_STREAMOFF
'''
assert c.count(stop_anchor) == 1, 'stopV4l2StreamingLocked 锚点没找到'
c = c.replace(stop_anchor, stop_add)

io.open(cp, 'w', encoding='utf-8').write(c)
print("  [3/3] DQBUF 预映射：.h 成员 + .cpp(include/premap/munmap) 已加")
PY
fi
echo "✅ HAL 修正已应用到 $SRC"
