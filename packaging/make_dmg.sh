#!/usr/bin/env bash
# ============================================================
# macOS 安装器（DMG）制作脚本
#
# 前置：
#   1) wails build -platform darwin/arm64   → build/bin/<App>.app
#   2) bash packaging/package_macos.sh      → 集成引擎 + ad-hoc 签名
# 用法：
#   bash packaging/make_dmg.sh [App 名称]
# 产物：
#   build/installer/社媒图文水印抹除工具_<版本>_arm64.dmg
#
# 实现说明（为什么不直接 hdiutil create -srcfolder）：
#   -srcfolder 需要一个「只含待打包内容」的临时目录，500MB/1700+ 文件，
#   用完删除会触发本机批量删除安全策略（与 package_macos.sh 同因），
#   因此改为：建空镜像 → 挂载 → 拷贝 → 卸载 → 转 UDZO 压缩。
#   全程不产生需要批量删除的临时目录，只删单个 .tmp.dmg。
#
# 签名说明：DMG 内的 .app 为 ad-hoc 签名，未经 Apple 公证，首次打开
# 会被 Gatekeeper 拦下，需右键「打开」或在 系统设置→隐私与安全性 中放行；
# 镜像内已附《首次打开说明.txt》。对外分发需 Developer ID + 公证。
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."

APP_NAME="${1:-lama-watermark-eraser}"
APP="build/bin/${APP_NAME}.app"
OUT_DIR="build/installer"

if [ ! -d "$APP" ]; then
  echo "[ERROR] 未找到 $APP，请先执行: wails build -platform darwin/arm64" >&2
  exit 1
fi

# 版本号唯一来源：wails.json 的 productVersion（与 .app 的 CFBundleShortVersionString 同源）
VERSION="$(sed -n 's/^[[:space:]]*"productVersion"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' wails.json | head -1)"
if [ -z "$VERSION" ]; then
  echo "[ERROR] 无法从 wails.json 解析 productVersion" >&2
  exit 1
fi

# 镜像内的应用显示名：Finder/启动台按外层目录名显示，改中文名避免出现
# "lama-watermark-eraser.app"（重命名外层目录不影响包内 CFBundleExecutable，
# 也不改 CFBundleIdentifier，用户偏好与引擎定位均不受影响，改完重新 ad-hoc 签名）
DISPLAY_NAME="社媒图文水印抹除工具"
VOLNAME="社媒图文水印抹除工具 ${VERSION}"
FINAL="$OUT_DIR/社媒图文水印抹除工具_${VERSION}_arm64.dmg"
TMP_DMG="$OUT_DIR/.dmg_rw.tmp.dmg"

mkdir -p "$OUT_DIR"
rm -f "$TMP_DMG" "$FINAL"

# 镜像容量 = app 实际占用 + 120MB 余量（含说明文件与文件系统开销）
SIZE_MB=$(( $(du -sm "$APP" | awk '{print $1}') + 120 ))
echo "[INFO] 版本 $VERSION，镜像容量 ${SIZE_MB}MB"

echo "[INFO] 创建读写镜像..."
hdiutil create -size "${SIZE_MB}m" -fs HFS+ -volname "$VOLNAME" -ov "$TMP_DMG" >/dev/null

echo "[INFO] 挂载并写入内容..."
MNT="$(hdiutil attach "$TMP_DMG" -nobrowse -readwrite | grep -o '/Volumes/.*' | head -1)"
if [ -z "$MNT" ] || [ ! -d "$MNT" ]; then
  echo "[ERROR] 挂载失败" >&2
  exit 1
fi
# 卸载兜底：脚本中途失败也要把卷摘掉，避免残留 /Volumes
cleanup() {
  if [ -d "$MNT" ]; then
    hdiutil detach "$MNT" >/dev/null 2>&1 || hdiutil detach -force "$MNT" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

cp -R "$APP" "$MNT/${DISPLAY_NAME}.app"
ln -s /Applications "$MNT/Applications"

# 重命名后再签一次，确保镜像内的包签名始终有效
echo "[INFO] 镜像内重新 ad-hoc 签名..."
codesign --force --deep --sign - "$MNT/${DISPLAY_NAME}.app"
xattr -dr com.apple.quarantine "$MNT/${DISPLAY_NAME}.app" 2>/dev/null || true

cat > "$MNT/首次打开说明.txt" <<EOF
社媒图文水印抹除工具 ${VERSION}（macOS / Apple Silicon）

安装
  把左侧的「${DISPLAY_NAME}」拖到右侧 Applications 文件夹即可。

首次打开
  本应用为 ad-hoc 签名（未做 Apple 公证），双击可能提示
  「无法打开，因为 Apple 无法检查其是否包含恶意软件」。
  任选一种方式放行：
    1) 在应用上右键 → 打开 → 再点「打开」；
    2) 或：系统设置 → 隐私与安全性 → 拉到底 → 「仍要打开」。
  仅首次需要，之后正常双击即可。

如果提示「已损坏，无法打开」
  在「终端」执行（把应用拖进终端可自动补全路径）：
    xattr -dr com.apple.quarantine /Applications/${DISPLAY_NAME}.app

说明
  内置 AI 引擎（lamacore，约 500MB）已随应用打包，首次启动需解压预热，
  请耐心等待状态栏提示「引擎就绪」。
EOF

echo "[INFO] 卸载并压缩为 UDZO..."
cleanup
trap - EXIT

hdiutil convert "$TMP_DMG" -format UDZO -imagekey zlib-level=9 -o "$FINAL" >/dev/null
rm -f "$TMP_DMG"

echo "[OK] 安装器已生成：$FINAL"
shasum -a 256 "$FINAL"
du -sh "$FINAL"
