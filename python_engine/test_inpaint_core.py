# -*- coding: utf-8 -*-
"""load_model 路径回归测试（Bug A / v1.1.1：非 ASCII 安装路径）。

背景：torch.jit.load 在 Windows 上把 str 路径按 UTF-8 交给 LibTorch C++ 层，
后者以 ANSI(GBK) 代码页 fopen —— 模型位于中文安装目录时报 errno 2。
修复：load_model 检测非 ASCII 路径后 chdir 至模型目录、以 ASCII 裸文件名加载。

运行方式（在 python_engine/ 目录下，用带 torch 的 venv）：
    venv\\Scripts\\python.exe -m unittest test_inpaint_core -v

模型文件默认取打包产物 build/bin/lamacore/_internal/models/big-lama.pt；
可用环境变量 LAMA_TEST_MODEL 覆盖；模型缺失时整体 skip（CI 无模型场景）。
"""
import os
import shutil
import tempfile
import unittest

import numpy as np

import inpaint_core

MODEL_ENV = "LAMA_TEST_MODEL"
_DEFAULT_MODEL = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
    "build", "bin", "lamacore", "_internal", "models", "big-lama.pt",
)


def _model_path():
    """定位测试用 big-lama.pt：环境变量优先，其次仓库内打包产物。"""
    p = os.environ.get(MODEL_ENV, "").strip()
    return p if p else _DEFAULT_MODEL


class LoadModelPathTest(unittest.TestCase):
    """load_model 在纯 ASCII 与含中文路径下均能成功加载并完成前向。"""

    @classmethod
    def setUpClass(cls):
        cls.model_src = _model_path()
        if not os.path.isfile(cls.model_src):
            raise unittest.SkipTest(
                "未找到 big-lama.pt（设 %s 指向模型文件后重跑）" % MODEL_ENV)

    def test_ascii_path_loads(self):
        """纯 ASCII 路径维持直载行为（回归保护，不触发 chdir 分支）。"""
        self.assertTrue(inpaint_core._is_pure_ascii(self.model_src))
        model = inpaint_core.load_model(self.model_src, device="cpu")
        self.assertIsNotNone(model)

    def test_non_ascii_dir_loads(self):
        """中文目录下的模型可加载、前向形状正确，且 cwd 被恢复（Bug A 修复验证）。"""
        # %TEMP% 本身通常为 ASCII，中文由前缀引入 => 模型路径必含非 ASCII
        tmp_root = tempfile.mkdtemp(prefix="社媒水印测试_")
        cn_dir = os.path.join(tmp_root, "中文模型目录")
        os.makedirs(cn_dir)
        dst = os.path.join(cn_dir, "big-lama.pt")
        shutil.copyfile(self.model_src, dst)
        cwd_before = os.getcwd()
        try:
            self.assertFalse(dst.isascii())
            model = inpaint_core.load_model(dst, device="cpu")
            self.assertIsNotNone(model)
            # chdir 方案不得污染进程工作目录
            self.assertEqual(os.getcwd(), cwd_before)

            # 64x64 冒烟前向：白框水印区域，输出形状/类型契约正确
            image = np.zeros((64, 64, 3), dtype="uint8")
            image[16:48, 16:48] = 255
            mask = np.zeros((64, 64), dtype="uint8")
            mask[16:48, 16:48] = 255
            out = inpaint_core.forward(model, image, mask)
            self.assertEqual(out.shape, (64, 64, 3))
            self.assertEqual(out.dtype, np.uint8)
        finally:
            os.chdir(cwd_before)
            shutil.rmtree(tmp_root, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
