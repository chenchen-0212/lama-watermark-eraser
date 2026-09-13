#!/usr/bin/env bash
# ============================================================
# macOS 打包脚本：把 lamacore 引擎集成进 .app，并做 ad-hoc 签名
#
# 前置：
#   1) wails build -platform darwin/arm64   → build/bin/<App>.app
#   2) bash python_engine/build_engine.sh   → python_engine/dist/lamacore/
# 用法：
#   bash packaging/package_macos.sh ["App 名称"]
#
# 说明：未签名的 helper 可执行文件会被 Gatekeeper 拦截，这里用 ad-hoc 签名
#（--sign -）满足本机运行；对外分发需改用 Apple Developer ID 并公证。
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

APP_NAME="${1:-lama-watermark-eraser}"
APP="build/bin/${APP_NAME}.app"
ENGINE="python_engine/dist/lamacore"

if [ ! -d "$APP" ]; then
  echo "[ERROR] 未找到 $APP，请先执行: wails build -platform darwin/arm64" >&2
  exit 1
fi
if [ ! -x "$ENGINE/lamacore" ]; then
  echo "[ERROR] 未找到 $ENGINE/lamacore，请先执行: bash python_engine/build_engine.sh" >&2
  exit 1
fi

ENGINE_DEST="$APP/Contents/Resources/lamacore"

# 引擎内容一致时跳过（避免每次重打包都做一次 1700+ 文件的大批量删除，
# 该操作在部分环境会触发安全策略拦截导致脚本中断）
if [ -d "$ENGINE_DEST" ] && diff -rq "$ENGINE" "$ENGINE_DEST" >/dev/null 2>&1; then
  echo "[INFO] 包内引擎与 $ENGINE 内容一致，跳过复制"
else
  if [ -d "$ENGINE_DEST" ]; then
    echo "[INFO] 引擎已变化，清理旧目录：$ENGINE_DEST"
    rm -rf "$ENGINE_DEST"
  fi
  echo "[INFO] 集成引擎 -> $ENGINE_DEST"
  cp -R "$ENGINE" "$ENGINE_DEST"
  chmod +x "$ENGINE_DEST/lamacore"
fi

echo "[INFO] ad-hoc 签名（未签名的 helper 会被 Gatekeeper 拦截）..."
codesign --force --deep --sign - "$APP"
codesign --verify --verbose=1 "$APP" 2>&1 | tail -3 || true
xattr -dr com.apple.quarantine "$APP" 2>/dev/null || true

echo "[OK] 打包完成：$APP"
du -sh "$APP"
