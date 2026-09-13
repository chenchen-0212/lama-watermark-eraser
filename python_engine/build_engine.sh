#!/usr/bin/env bash
# ============================================================
# lamacore 一键构建脚本（macOS / Linux，PyInstaller onedir）
#
# 用法:   bash python_engine/build_engine.sh
# 可选:   LAMA_MODEL_SRC=/path/to/big-lama.pt （覆盖权重路径，默认取本目录 big-lama.pt）
# 产物:   python_engine/dist/lamacore/lamacore (+ _internal)
# 分发:   将 dist/lamacore 整目录复制到主程序同级；macOS .app 则放 Contents/Resources/
#
# 与 build_engine.bat 的差异：Windows 走 py -3.10/3.12 + venv\Scripts\activate.bat，
# 本脚本优先复用项目根 .venv（uv 或 venv 建的均可），条目名不带 .exe。
# ============================================================
set -euo pipefail
cd "$(dirname "$0")"

MODEL_SRC="${LAMA_MODEL_SRC:-$PWD/big-lama.pt}"
if [ ! -f "$MODEL_SRC" ]; then
  echo "[ERROR] 找不到权重文件: $MODEL_SRC （可设 LAMA_MODEL_SRC 覆盖）" >&2
  exit 1
fi

# ---------- Python 运行时（优先项目根 .venv）----------
PY=""
if [ -x ../.venv/bin/python ]; then
  PY="$(cd .. && pwd)/.venv/bin/python"
elif command -v python3.12 >/dev/null 2>&1; then
  PY="$(command -v python3.12)"
elif command -v python3 >/dev/null 2>&1; then
  PY="$(command -v python3)"
else
  echo "[ERROR] 未找到 Python 运行时（建议 python3.12）" >&2
  exit 1
fi
echo "[INFO] Python: $PY ($("$PY" -V 2>&1))"

if ! "$PY" -c "import torch, numpy" 2>/dev/null; then
  echo "[ERROR] 当前环境缺 torch/numpy，请先安装依赖：" >&2
  echo "        \"$PY\" -m pip install -r requirements.txt" >&2
  exit 1
fi
if ! "$PY" -c "import PyInstaller" 2>/dev/null; then
  echo "[INFO] 安装 pyinstaller ..."
  "$PY" -m pip install "pyinstaller==6.6.0"
fi

# ---------- 打包前像素级对齐自测（不过不许打包）----------
echo "[INFO] 运行对齐自测 align_smoke.py ..."
LAMA_ENGINE_MODEL="$MODEL_SRC" "$PY" align_smoke.py || {
  echo "[ERROR] 对齐自测未通过，终止打包" >&2
  exit 1
}

# ---------- PyInstaller onedir ----------
rm -rf build dist
echo "[INFO] PyInstaller 打包中（含 torch 与约 205MB 权重，需数分钟）..."
LAMA_MODEL_SRC="$MODEL_SRC" "$PY" -m PyInstaller lamacore.spec --noconfirm

echo ""
echo "[OK] 构建完成: $PWD/dist/lamacore/lamacore"
echo "[OK] 分发方式: 将 dist/lamacore 整目录复制到主程序同级（.app 则放 Contents/Resources/）"
