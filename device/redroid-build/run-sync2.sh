#!/bin/bash
export PATH=/ssd/redroid/bin:$PATH
cd /ssd/redroid/src
for i in $(seq 1 15); do
  echo "=== finish attempt $i $(date -Is) ==="
  repo sync -c -j4 --no-clone-bundle --optimized-fetch --retry-fetches=3 --force-sync && {
    echo "=== SYNC CLEAN ==="; exit 0; }
  sleep 30
done
echo "=== STILL DIRTY ==="
