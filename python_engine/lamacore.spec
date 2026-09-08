# -*- mode: python ; coding: utf-8 -*-
"""PyInstaller spec：将 big-lama 去水印引擎打包为 onedir lamacore/。

产物布局（PyInstaller 6.x onedir）：
    dist/lamacore/lamacore.exe          引擎入口（console 进程，stdin/stdout JSONL）
    dist/lamacore/_internal/            torch / numpy / 引擎脚本
    dist/lamacore/_internal/models/big-lama.pt   权重（worker 经 sys._MEIPASS 定位）

分发约定（resources.LocatePythonEngine）：主程序 exe 同级 lamacore/lamacore.exe。

用法：
    cd python_engine
    set LAMA_MODEL_SRC=D:\path\to\big-lama.pt   （可选，缺省用本机已验证路径）
    pyinstaller lamacore.spec --noconfirm
"""
import os

# 权重来源：环境变量 LAMA_MODEL_SRC 优先，缺省为本机已验证路径
MODEL_SRC = os.environ.get(
    "LAMA_MODEL_SRC", r"D:\clear_mask\lama\watermark_tool\models\big-lama.pt"
)
if not os.path.isfile(MODEL_SRC):
    raise SystemExit(
        "big-lama.pt not found: %s (set LAMA_MODEL_SRC to override)" % MODEL_SRC
    )
print("bundling model:", MODEL_SRC)

a = Analysis(
    ["worker.py"],
    pathex=["."],
    binaries=[],
    datas=[(MODEL_SRC, "models")],
    # inpaint_core 为函数内延迟导入，显式声明确保打入
    hiddenimports=["inpaint_core", "numpy", "torch"],
    hookspath=[],
    hooksconfig={},
    runtime_hooks=[],
    excludes=[
        # torch 生态可选项（系统 site-packages 里装有 torchvision/iopaint，
        # 会被 torch hook 顺带拖入 cv2/timm/peft/transformers，必须显式裁剪）
        "torchvision", "torchaudio", "cv2", "timm", "peft",
        "transformers", "safetensors", "huggingface_hub",
        "onnx", "onnxruntime", "iopaint",
        "PIL",  # worker/inpaint_core 不用 PIL（解码在 Go 侧完成）
        # 引擎仅需 torch/numpy；裁剪无关大包以控制体积
        "tkinter", "matplotlib", "scipy", "pandas", "sklearn",
        "IPython", "PyQt5", "PySide2", "PySide6",
        "notebook", "jupyter", "test", "tests", "pydoc_data",
    ],
    noarchive=False,
)

pyz = PYZ(a.pure)

exe = EXE(
    pyz,
    a.scripts,
    [],
    exclude_binaries=True,
    name="lamacore",
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=False,
    # 必须 console=True：引擎经 stdin/stdout 与 Go 主程序通信，
    # windowed 模式会脱离标准流导致协议失效
    console=True,
    disable_windowed_traceback=False,
)

coll = COLLECT(
    exe,
    a.binaries,
    a.datas,
    strip=False,
    upx=False,
    upx_exclude=[],
    name="lamacore",  # 产物目录名必须为 lamacore/（LocatePythonEngine 约定）
)
