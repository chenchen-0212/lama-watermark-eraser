# -*- coding: utf-8 -*-
"""
big-lama 推理核心（逐项对齐 IOPaint）。

本模块只依赖 numpy + torch，不引入 IOPaint 全家桶；把 IOPaint 的关键逻辑内联：
  - norm_img           <-> iopaint.helper.norm_img
  - ceil_modulo        <-> iopaint.helper.ceil_modulo
  - pad_img_to_modulo  <-> iopaint.helper.pad_img_to_modulo（mode="symmetric"）
  - forward            <-> iopaint.model.base.InpaintModel._pad_forward
                          + iopaint.model.lama.LaMa.forward（HD ORIGINAL 默认策略）

关键对齐点（P0 红线，效果必须 >= IOPaint）：
  1. pad 到 mod 8 使用 mode="symmetric"（不是 reflect / constant）。
     IOPaint 源码：iopaint/helper.py -> pad_img_to_modulo -> np.pad(mode="symmetric")。
  2. 图像与掩膜一起 pad（同一 symmetric mode），掩膜 pad 后经 norm_img 再 (mask>0)*1。
  3. 全 fp32；torch.no_grad()；无任何 512 缩放 / ONNX 逻辑。
  4. IOPaint 的 LaMa.forward 返回 BGR（OpenCV 约定，cv2.cvtColor(COLOR_RGB2BGR)），
     其保存路径用 cv2.imencode（BGR 原生）最终落盘为 RGB；故本模块直接返回
     RGB888（= 模型 RGB 输出），与 IOPaint 落盘结果逐像素一致。
"""
import numpy as np
import torch

MOD = 8  # LaMa pad_mod


def ceil_modulo(x, mod):
    """对齐 iopaint.helper.ceil_modulo：向上取整到 mod 的倍数。"""
    if x % mod == 0:
        return x
    return (x // mod + 1) * mod


def norm_img(np_img):
    """对齐 iopaint.helper.norm_img：HWC -> CHW，float32 / 255。

    2D 输入（掩膜）会先补一个通道维：HxW -> HxWx1。
    """
    if len(np_img.shape) == 2:
        np_img = np_img[:, :, np.newaxis]
    np_img = np.transpose(np_img, (2, 0, 1))
    np_img = np_img.astype("float32") / 255
    return np_img


def pad_img_to_modulo(img, mod):
    """对齐 iopaint.helper.pad_img_to_modulo（square=False, min_size=None）。

    注意：pad mode 为 "symmetric"，这是 IOPaint 的唯一取值。
    """
    if len(img.shape) == 2:
        img = img[:, :, np.newaxis]
    height, width = img.shape[:2]
    out_height = ceil_modulo(height, mod)
    out_width = ceil_modulo(width, mod)
    return np.pad(
        img,
        ((0, out_height - height), (0, out_width - width), (0, 0)),
        mode="symmetric",
    )


def load_model(model_path, device="cpu"):
    """对齐 iopaint.helper.load_jit_model 的 CPU 分支：torch.jit.load + eval。"""
    model = torch.jit.load(model_path, map_location="cpu")
    if device and device != "cpu":
        model = model.to(device)
    model.eval()
    return model


def set_num_threads(n):
    """设置 torch 线程数。n<=0 表示使用系统默认（不显式调用，避免版本差异）。"""
    if n and n > 0:
        torch.set_num_threads(n)


def forward(model, image_rgb, mask_0255):
    """对齐 IOPaint `_pad_forward` + `LaMa.forward`（HD ORIGINAL 默认策略）。

    Args:
        model:      torch.jit.load 得到的 ScriptModule（已 eval）
        image_rgb:  [H, W, 3] uint8 RGB
        mask_0255:  [H, W]    uint8 {0,255}

    Returns:
        [H, W, 3] uint8 RGB（与 IOPaint 落盘结果逐像素一致）
    """
    origin_height, origin_width = image_rgb.shape[:2]

    # ① pad 到 mod 8（symmetric），图像与掩膜同一 mode
    pad_image = pad_img_to_modulo(image_rgb, MOD)  # [H', W', 3] uint8
    pad_mask = pad_img_to_modulo(mask_0255, MOD)   # [H', W', 1] uint8

    # ② norm_img + mask 二值化（逐项对齐 IOPaint）
    image = norm_img(pad_image)        # [3, H', W'] float32 0..1
    mask = norm_img(pad_mask)          # [1, H', W'] float32 0..1
    mask = (mask > 0) * 1              # {0,1}（numpy int64，与 IOPaint 一致）

    image = torch.from_numpy(image).unsqueeze(0)  # [1,3,H',W']
    mask = torch.from_numpy(mask).unsqueeze(0)    # [1,1,H',W']

    # ③ 无梯度前向
    with torch.no_grad():
        inpainted_image = model(image, mask)

    # ④ 后处理：permute -> clip(*255) -> uint8，再裁剪回原尺寸
    cur_res = inpainted_image[0].permute(1, 2, 0).detach().cpu().numpy()  # [H',W',3] RGB
    cur_res = np.clip(cur_res * 255, 0, 255).astype("uint8")

    # IOPaint 此处为 cv2.cvtColor(cur_res, COLOR_RGB2BGR)，再在 _pad_forward 里
    # result[0:origin_height, 0:origin_width] 裁剪；其 OpenCV 落盘（BGR 原生）最终
    # 等价于模型 RGB 输出。本模块为满足 Go 侧 RGB888 契约，直接返回模型 RGB 输出，
    # 与 IOPaint 落盘结果逐像素一致，故不做 BGR 翻转。
    cur_res = cur_res[0:origin_height, 0:origin_width, :]
    return cur_res
