#!/bin/bash
# 429 是 GitHub/googlesource 的限流，不是错误状态：降并发 + 反复续传直到干净。
# repo sync 天然幂等可续传，所以重试是安全的。
export PATH=/ssd/redroid/bin:$PATH
cd /ssd/redroid/src
for i in $(seq 1 40); do
  echo "=== attempt $i  $(date -Is) ==="
  repo sync -c -j4 --no-clone-bundle --optimized-fetch --retry-fetches=3 && {
    echo "=== SYNC CLEAN at attempt $i ==="; exit 0; }
  echo "--- attempt $i 未干净，60s 后续传 ---"
  sleep 60
done
echo "=== SYNC GAVE UP after 40 attempts ==="
exit 1
