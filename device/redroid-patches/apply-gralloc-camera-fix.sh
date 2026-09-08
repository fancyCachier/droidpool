#!/usr/bin/env bash
# 让 redroid 的 guest（软件）gralloc 支持相机缓冲。分三段，各自幂等，重复跑安全。
#
# 用法: apply-gralloc-camera-fix.sh <AOSP-SRC>    例: apply-gralloc-camera-fix.sh /src
#
# 背景：相机 HAL 已好（注册/打开/推流都过），但相机帧到不了应用。redroid 的软件
# gralloc（vendor/redroid/gralloc，ashmem 版）有两处缺口：
#
#   段1  gralloc_alloc 的 format switch 只认 RGB(A)+YV12+RAW16，相机要的
#        YCbCr_420_888(35)/IMPLEMENTATION_DEFINED(34)/BLOB(33) 落到 default:EINVAL
#        → 任何相机缓冲都分配失败（预览、JPEG、扫码取帧全炸）。
#        补上这三种格式（ashmem CPU 缓冲，YUV 按 2bpp 足量、BLOB 按 1bpp）。
#
#   段2+3  即使分配成功，取 YUV 帧（CameraX ImageAnalysis / mobile_scanner 扫码走
#        这条）时 HAL 要 lockYCbCr 拿平面指针，而这个 gralloc 没实现 lock_ycbcr，
#        且 private_handle 没存 format/宽高/stride，于是
#          HandleImporter: failed to lockYCbCr error 3
#          formatConvert: unsupported flexible yuv layout ... str 0
#        补：alloc 时把 format/width/height/aligned_w 存进 handle（段2），
#        实现 gralloc_lock_ycbcr 返回 I420 平面布局并挂到 module（段3）。
#        JPEG/BLOB 走普通 lock 不受此限（段1 后已可抓拍）；补完 YUV 取帧也能用。
#        屏上实时预览仍要硬件 GPU（guest 软件 GL 显示不了 YUV 纹理），另说。
set -euo pipefail
SRC=${1:?用法: apply-gralloc-camera-fix.sh <AOSP-SRC>}
D="$SRC/vendor/redroid/gralloc"
G="$D/gralloc.cpp"; H="$D/gralloc_priv.h"; M="$D/mapper.cpp"
for f in "$G" "$H" "$M"; do [ -f "$f" ] || { echo "找不到 $f"; exit 1; }; done

# 段1：format switch 加相机格式
if grep -q 'HAL_PIXEL_FORMAT_YCbCr_420_888' "$G"; then
  echo "  [1/3] 相机格式：已打过，跳过"
else
  python3 - "$G" <<'PY'
import io, sys
p = sys.argv[1]; s = io.open(p, encoding='utf-8').read()
anchor = """            bytesPerPixel = 2;
            break;
        default:
            return -EINVAL;
"""
add = """            bytesPerPixel = 2;
            break;
        case HAL_PIXEL_FORMAT_YCbCr_420_888:
        case HAL_PIXEL_FORMAT_IMPLEMENTATION_DEFINED:
            // redroid: 相机 YUV / 隐式格式。原落 default 被 EINVAL，导致相机所有
            // 输出缓冲分配失败。按 2 bpp 足量分配（>= 12-bit YUV420），走 ashmem。
            bytesPerPixel = 2;
            break;
        case HAL_PIXEL_FORMAT_BLOB:
            // JPEG blob：线性缓冲，width 即字节数、height=1。
            bytesPerPixel = 1;
            break;
        default:
            return -EINVAL;
"""
assert s.count(anchor) == 1, 'format switch 锚点没找到'
io.open(p, 'w', encoding='utf-8').write(s.replace(anchor, add))
print("  [1/3] 相机格式：已加 YCbCr_420_888 / IMPLEMENTATION_DEFINED / BLOB")
PY
fi

# 段2：private_handle_t 存 format/width/height/aligned_w
if grep -q 'aligned_w' "$H"; then
  echo "  [2/3] handle 字段：已加，跳过"
else
  python3 - "$H" <<'PY'
import io, sys
p = sys.argv[1]; s = io.open(p, encoding='utf-8').read()
# 字段声明：加在 pid 之后
d_anchor = """    uint64_t base __attribute__((aligned(8)));
    int     pid;
"""
d_add = d_anchor + """    // redroid: 相机 lock_ycbcr 用 —— alloc 时填
    int     format;
    int     width;
    int     height;
    int     aligned_w;
"""
assert s.count(d_anchor) == 1, 'handle 字段锚点没找到'
s = s.replace(d_anchor, d_add)
# 构造函数初始化列表
c_anchor = """        base(0), pid(getpid())
    {"""
c_add = """        base(0), pid(getpid()),
        format(0), width(0), height(0), aligned_w(0)
    {"""
assert s.count(c_anchor) == 1, 'handle 构造函数锚点没找到'
s = s.replace(c_anchor, c_add)
io.open(p, 'w', encoding='utf-8').write(s)
print("  [2/3] handle 字段：format/width/height/aligned_w 已加")
PY
fi

# 段3：填字段 + 挂 lock_ycbcr + 实现
if grep -q 'gralloc_lock_ycbcr' "$G"; then
  echo "  [3/3] lock_ycbcr：已加，跳过"
else
  python3 - "$G" "$M" <<'PY'
import io, sys
gp, mp = sys.argv[1], sys.argv[2]

# --- gralloc.cpp：extern 声明 ---
g = io.open(gp, encoding='utf-8').read()
ext_anchor = """extern int gralloc_unregister_buffer(gralloc_module_t const* module,
        buffer_handle_t handle);
"""
ext_add = ext_anchor + """extern int gralloc_lock_ycbcr(gralloc_module_t const* module,
        buffer_handle_t handle, int usage,
        int l, int t, int w, int h,
        struct android_ycbcr* ycbcr);
"""
assert g.count(ext_anchor) == 1, 'extern 锚点没找到'
g = g.replace(ext_anchor, ext_add)

# --- gralloc.cpp：module 注册 .lock_ycbcr ---
mod_anchor = """        .lock = gralloc_lock,
        .unlock = gralloc_unlock,
    },
"""
mod_add = """        .lock = gralloc_lock,
        .unlock = gralloc_unlock,
        .lock_ycbcr = gralloc_lock_ycbcr,
    },
"""
assert g.count(mod_anchor) == 1, 'module 注册锚点没找到'
g = g.replace(mod_anchor, mod_add)

# --- gralloc.cpp：alloc 后填 handle 字段 ---
alloc_anchor = """    int err = gralloc_alloc_buffer(dev, size, usage, pHandle);
    if (err < 0) {
        return err;
    }

    *pStride = stride;
"""
alloc_add = """    int err = gralloc_alloc_buffer(dev, size, usage, pHandle);
    if (err < 0) {
        return err;
    }

    // redroid: 存下相机 lock_ycbcr 需要的几何信息
    private_handle_t* hnd = (private_handle_t*)*pHandle;
    hnd->format = format;
    hnd->width = width;
    hnd->height = height;
    hnd->aligned_w = (int)stride;
    *pStride = stride;
"""
assert g.count(alloc_anchor) == 1, 'alloc 锚点没找到'
g = g.replace(alloc_anchor, alloc_add)
io.open(gp, 'w', encoding='utf-8').write(g)

# --- mapper.cpp：实现 gralloc_lock_ycbcr（插在 gralloc_lock 与 gralloc_unlock 之间）---
m = io.open(mp, encoding='utf-8').read()
lk_anchor = """    private_handle_t* hnd = (private_handle_t*)handle;
    *vaddr = (void*)hnd->base;
    return 0;
}

int gralloc_unlock(gralloc_module_t const* /*module*/,
"""
lk_add = """    private_handle_t* hnd = (private_handle_t*)handle;
    *vaddr = (void*)hnd->base;
    return 0;
}

// redroid: 相机取 YUV 帧要 lockYCbCr 拿平面指针。按存在 handle 里的几何返回
// I420 平面布局（Y 全帧，其后 Cb、Cr；chroma_step=1）。HAL 写、应用读都经此，
// 布局一致即可；缓冲按 2bpp 分配，足够放下 1.5bpp 的 YUV420。
int gralloc_lock_ycbcr(gralloc_module_t const* /*module*/,
        buffer_handle_t handle, int /*usage*/,
        int /*l*/, int /*t*/, int /*w*/, int /*h*/,
        struct android_ycbcr* ycbcr)
{
    if (private_handle_t::validate(handle) < 0)
        return -EINVAL;
    if (!ycbcr)
        return -EINVAL;

    private_handle_t* hnd = (private_handle_t*)handle;
    uint8_t* base = (uint8_t*)hnd->base;
    int ystride = hnd->aligned_w > 0 ? hnd->aligned_w : hnd->width;
    int height = hnd->height;
    int cstride = ystride / 2;
    size_t ySize = (size_t)ystride * height;
    size_t cSize = (size_t)cstride * (height / 2);

    memset(ycbcr, 0, sizeof(*ycbcr));
    ycbcr->ystride = ystride;
    ycbcr->cstride = cstride;
    ycbcr->chroma_step = 1;  // planar
    ycbcr->y = base;
    if (hnd->format == HAL_PIXEL_FORMAT_YV12) {
        // YV12: Y, 然后 Cr(V), 再 Cb(U)
        ycbcr->cr = base + ySize;
        ycbcr->cb = base + ySize + cSize;
    } else {
        // I420 给 YCbCr_420_888 / IMPLEMENTATION_DEFINED: Y, Cb(U), Cr(V)
        ycbcr->cb = base + ySize;
        ycbcr->cr = base + ySize + cSize;
    }
    return 0;
}

int gralloc_unlock(gralloc_module_t const* /*module*/,
"""
assert m.count(lk_anchor) == 1, 'mapper gralloc_lock 锚点没找到'
m = m.replace(lk_anchor, lk_add)
io.open(mp, 'w', encoding='utf-8').write(m)
print("  [3/3] lock_ycbcr：填字段 + 挂 module + mapper 实现 已加")
PY
fi
echo "✅ gralloc 相机补丁已应用到 $SRC"
