#!/bin/bash
# 编出 uiagent.dex（推到设备用 app_process 跑）。
# 只依赖公开 SDK：隐藏 API 全走反射，不需要 AOSP 源码树。
set -e
cd "$(dirname "$0")"
SDK=${ANDROID_HOME:-$HOME/Library/Android/sdk}
# 用 android-34 = Android 14，与 redroid 镜像一致
PLATFORM=${PLATFORM:-$SDK/platforms/android-34/android.jar}
D8=$(ls -d "$SDK"/build-tools/*/d8 2>/dev/null | sort -V | tail -1)
[ -f "$PLATFORM" ] || { echo "找不到 android.jar: $PLATFORM"; exit 1; }
[ -x "$D8" ] || { echo "找不到 d8（装一个 build-tools）"; exit 1; }

rm -rf out && mkdir -p out/classes
# Android 14 的 ART 吃 Java 11 字节码；本机 javac 是 21，必须降级目标
javac --release 11 -nowarn -classpath "$PLATFORM" -d out/classes \
  $(find src -name '*.java')
"$D8" --min-api 34 --output out out/classes/com/daboshi/droidpool/*.class
mv out/classes.dex uiagent.dex
rm -rf out
ls -l uiagent.dex
