@echo off
setlocal EnableDelayedExpansion
REM ============================================================
REM lamacore 一键构建脚本（PyInstaller onedir）
REM
REM 终裁策略：优先 Python 3.10 + torch 2.2.2 CPU（requirements.txt）。
REM 本机若无 Python 3.10，则降级为 Python 3.12 + 系统已装
REM torch 2.14.0+cpu（venv --system-site-packages 复用系统 torch，
REM 免去重复下载约 200MB；该降级路径已在本机实测通过）。
REM
REM 用法:   python_engine\build_engine.bat
REM 可选:   set LAMA_MODEL_SRC=D:\path\to\big-lama.pt   （覆盖权重路径）
REM 产物:   python_engine\dist\lamacore\lamacore.exe (+ _internal)
REM 分发:   将 dist\lamacore\ 整目录复制到主程序 exe 同级即可
REM         （resources.LocatePythonEngine 按 exe 同级 lamacore\lamacore.exe 定位）
REM ============================================================
cd /d "%~dp0"

set "MODEL_SRC=%LAMA_MODEL_SRC%"
if "%MODEL_SRC%"=="" set "MODEL_SRC=D:\clear_mask\lama\watermark_tool\models\big-lama.pt"
if not exist "%MODEL_SRC%" (
    echo [ERROR] 找不到权重文件: %MODEL_SRC% （可设 LAMA_MODEL_SRC 覆盖）
    exit /b 1
)

REM ---------- 选择 Python（3.10 优先，降级 3.12）----------
set "PYCMD="
set "USE_SYSTEM_TORCH=0"
py -3.10 -c "import sys" >nul 2>&1
if not errorlevel 1 (
    set "PYCMD=py -3.10"
) else (
    py -3.12 -c "import sys" >nul 2>&1
    if not errorlevel 1 (
        set "PYCMD=py -3.12"
        set "USE_SYSTEM_TORCH=1"
        echo [INFO] 未找到 Python 3.10，降级: Python 3.12 + 系统 torch（终裁允许的降级路径）
    ) else (
        echo [ERROR] 未找到 Python 3.10 / 3.12 运行时
        exit /b 1
    )
)
echo [INFO] Python: !PYCMD!   USE_SYSTEM_TORCH=!USE_SYSTEM_TORCH!

REM ---------- venv ----------
if not exist venv (
    echo [INFO] 创建 venv ...
    if "!USE_SYSTEM_TORCH!"=="1" (
        !PYCMD! -m venv --system-site-packages venv
    ) else (
        !PYCMD! -m venv venv
    )
    if errorlevel 1 exit /b 1
)
call venv\Scripts\activate.bat

REM ---------- 依赖 ----------
if "!USE_SYSTEM_TORCH!"=="1" (
    REM 降级路径：torch 复用系统已装版本（如 2.14.0+cpu），仅补齐其余依赖与 pyinstaller；
    REM 不装 requirements.txt 中的 torch==2.2.2，避免向 venv 重复下载大 wheel。
    python -c "import torch" 2>nul
    if errorlevel 1 (
        echo [ERROR] 系统环境缺 torch，无法走降级路径
        exit /b 1
    )
    pip install "numpy==1.26.4" "pillow==10.3.0" "pyinstaller==6.6.0" || exit /b 1
) else (
    REM 终裁路径：全量按 requirements.txt（Windows PyPI 的 torch 2.2.2 wheel 即 CPU 版）
    pip install -r requirements.txt || exit /b 1
    pip install "pyinstaller==6.6.0" || exit /b 1
)

REM ---------- 打包前像素级对齐自测（不过不许打包）----------
echo [INFO] 运行对齐自测 align_smoke.py ...
set "LAMA_ENGINE_MODEL=%MODEL_SRC%"
python align_smoke.py
if errorlevel 1 (
    echo [ERROR] 对齐自测未通过，终止打包
    exit /b 1
)

REM ---------- PyInstaller onedir ----------
if exist build rmdir /s /q build
if exist dist rmdir /s /q dist
echo [INFO] PyInstaller 打包中（含 torch 与 205MB 权重，需数分钟）...
pyinstaller lamacore.spec --noconfirm
if errorlevel 1 (
    echo [ERROR] PyInstaller 打包失败
    exit /b 1
)

echo.
echo [OK] 构建完成: %~dp0dist\lamacore\lamacore.exe
echo [OK] 分发方式: 将 dist\lamacore 整目录复制到主程序 exe 同级
endlocal
