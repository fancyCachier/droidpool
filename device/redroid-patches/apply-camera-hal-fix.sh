#!/usr/bin/env bash
# 修 AOSP external camera HAL 的一个析构顺序竞态。
#
# 用法: apply-camera-hal-fix.sh <AOSP-SRC>
#
# 症状：应用一打开相机，HAL 进程就 abort，应用报「无法连接到相机」。
#
#   FORTIFY: pthread_mutex_lock called on a destroyed mutex
#     ExternalCameraDeviceSession::BufferRequestThread::waitForNextRequest()
#
# 原因是 C++ 析构顺序：BufferRequestThread 继承自 SimpleThread，而
# SimpleThread 在**自己的析构里**才 join 线程。销毁派生类时顺序是
#   1) ~BufferRequestThread 毁掉 mLock / mRequestCond
#   2) ~SimpleThread 才 join
# 而线程此刻正卡在 waitForNextRequest() 里用 mLock —— 第 1 步已经把它毁了。
#
# 同文件里的 OutputThread 和 provider 里的 HotplugThread 都有自己的析构、
# 在里面先 requestExitAndWait()，只有 BufferRequestThread 漏了。补上即可。
set -euo pipefail
SRC=${1:?用法: apply-camera-hal-fix.sh <AOSP-SRC>}
H="$SRC/hardware/interfaces/camera/device/default/ExternalCameraDeviceSession.h"
[ -f "$H" ] || { echo "找不到 $H"; exit 1; }

if grep -q '~BufferRequestThread' "$H"; then
  echo "  已打过，跳过"
  exit 0
fi

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
print("  已给 BufferRequestThread 加上析构函数")
PY
