#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""像素级对齐自测（PyInstaller 打包前必跑）。

对比两条路径的推理输出：
  A. worker 推理路径 —— inpaint_core.forward（worker.handle_infer 实际调用的实现）
  B. 独立参考实现   —— 本文件内复刻 IOPaint 语义，不 import inpaint_core 的任何函数：
       - np.pad(mode="symmetric") 四边填充到 8 的倍数
       - HWC uint8 -> float32/255 -> CHW；mask pad 后 (m>0)*1 -> int64
       - model(image, mask) -> [1,3,H',W'] -> permute -> *255 -> clip(0,255) -> 裁回原尺寸

断言：每组尺寸 max|A-B| <= 1（uint8 量化/浮点次序容差）。
全部通过打印 ALIGN_PASS 并退出 0，否则 ALIGN_FAIL 退出 1。

用法：
    python align_smoke.py          # 模型路径同 worker 回退链（LAMA_ENGINE_MODEL 优先）
"""
import os
import sys

import numpy as np
import torch

MOD = 8

# 待测尺寸：奇数 / 非 8 对齐 / 常规，覆盖 pad 分支
CASES = [(33, 17), (64, 64), (127, 101), (100, 75), (256, 256)]


def resolve_model_path():
    """与 worker 相同的回退链：环境变量 -> 脚本同级 models/ -> 本机默认路径。"""
    here = os.path.dirname(os.path.abspath(__file__))
    candidates = []
    env = os.environ.get("LAMA_ENGINE_MODEL")
    if env:
        candidates.append(env)
    candidates += [
        os.path.join(here, "models", "big-lama.pt"),
        os.path.join(here, "big-lama.pt"),
        r"D:\clear_mask\lama\watermark_tool\models\big-lama.pt",
    ]
    for p in candidates:
        if p and os.path.isfile(p):
            return p
    raise FileNotFoundError("找不到 big-lama.pt（已尝试: %s）" % " | ".join(candidates))


# ---------------------------------------------------------------
# B. 独立参考实现（不复用 inpaint_core 代码，独立复刻 IOPaint 语义）
# ---------------------------------------------------------------

def ref_pad_to_modulo(arr, mod):
    """np.pad symmetric 四边填充到 mod 的倍数（ceil，与 IOPaint helper 一致）。"""
    h, w = arr.shape[:2]
    out_h = ((h + mod - 1) // mod) * mod
    out_w = ((w + mod - 1) // mod) * mod
    pad_width = [(0, out_h - h), (0, out_w - w)]
    if arr.ndim == 3:  # HWC 图像：通道轴不填充
        pad_width.append((0, 0))
    return np.pad(arr, pad_width, mode="symmetric")


def ref_forward(model, image_rgb, mask_0255):
    oh, ow = image_rgb.shape[:2]

    pad_img = ref_pad_to_modulo(image_rgb, MOD).astype(np.float32) / 255.0
    pad_mask = ref_pad_to_modulo(mask_0255, MOD)

    image = torch.from_numpy(pad_img.transpose(2, 0, 1)).unsqueeze(0)          # (1,3,H',W')
    mask = torch.from_numpy((pad_mask.astype(np.float32) / 255.0 > 0).astype(np.int64))
    mask = mask.unsqueeze(0).unsqueeze(0)                                      # (1,1,H',W')

    with torch.no_grad():
        out = model(image, mask)

    res = out[0].permute(1, 2, 0).detach().cpu().numpy()
    res = np.clip(res * 255, 0, 255).astype("uint8")
    return res[:oh, :ow, :]


# ---------------------------------------------------------------
# 测试数据与主流程
# ---------------------------------------------------------------

def make_case(w, h):
    """确定性渐变图 + 中央矩形水印掩膜。"""
    yy, xx = np.mgrid[0:h, 0:w]
    image = np.empty((h, w, 3), dtype=np.uint8)
    image[:, :, 0] = (xx * 255 // max(1, w - 1)).astype(np.uint8)
    image[:, :, 1] = (yy * 255 // max(1, h - 1)).astype(np.uint8)
    image[:, :, 2] = ((xx + yy) * 255 // max(1, w + h - 2)).astype(np.uint8)
    mask = np.zeros((h, w), dtype=np.uint8)
    x1, y1 = w // 4, h // 4
    x2, y2 = max(x1 + 1, w * 3 // 4), max(y1 + 1, h * 3 // 4)
    mask[y1:y2, x1:x2] = 255
    return image, mask


def main():
    print("torch:", torch.__version__)
    model_path = resolve_model_path()
    print("model:", model_path)

    import inpaint_core  # A 路径（延迟导入，与 worker 一致）

    model = inpaint_core.load_model(model_path, device="cpu")

    all_pass = True
    print("%-12s %-10s" % ("size", "max_diff"))
    for w, h in CASES:
        image, mask = make_case(w, h)

        # A: worker 推理路径
        out_worker = inpaint_core.forward(model, image, mask)
        # B: 独立参考实现
        out_ref = ref_forward(model, image, mask)

        if out_worker.shape != (h, w, 3) or out_ref.shape != (h, w, 3):
            print("%-12s SHAPE_FAIL %s / %s" % ("%dx%d" % (w, h),
                                                out_worker.shape, out_ref.shape))
            all_pass = False
            continue

        diff = np.abs(out_worker.astype(np.int16) - out_ref.astype(np.int16))
        max_diff = int(diff.max())
        status = "ok" if max_diff <= 1 else "FAIL"
        print("%-12s %-10d %s" % ("%dx%d" % (w, h), max_diff, status))
        if max_diff > 1:
            all_pass = False

    print("=" * 32)
    if all_pass:
        print("ALIGN_PASS")
        return 0
    print("ALIGN_FAIL")
    return 1


if __name__ == "__main__":
    sys.exit(main())
