# ============================================================
# Windows 一键打包脚本：引擎 + 主程序 + Inno Setup 安装器
#
# 用法（任意位置执行，脚本自动切到项目根目录）：
#   powershell -ExecutionPolicy Bypass -File packaging\build_windows.ps1
#
# 可选参数：
#   -Version 1.1.10    覆盖版本号（缺省读取 wails.json 的 productVersion）
#   -SkipEngine       跳过 lamacore 引擎重建（已有 python_engine\dist\lamacore 时用，省几十分钟）
#   -SkipApp          跳过主程序重建（只重打安装器）
#
# 前置：
#   - Go + wails（go install github.com/wailsapp/wails/v2/cmd/wails@latest）
#   - MinGW-w64（wails 在 Windows 上编译需要 CGO，脚本自动探测 gcc.exe）
#   - Inno Setup 6（winget install -e --id JRSoftware.InnoSetup）
#   - Python 环境已按 python_engine\build_engine.bat 准备好（仅引擎重建时需要）
#
# 产物：
#   build\installer\社媒图文水印抹除工具_Setup_<版本>.exe
# ============================================================
param(
  [string]$Version,
  [switch]$SkipEngine,
  [switch]$SkipApp
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
Write-Host "[INFO] 项目根目录: $root"

# ---------- 版本号（唯一来源：wails.json）----------
if (-not $Version) {
  # 必须显式 -Encoding UTF8：wails.json 含中文且不带 BOM，Windows PowerShell 5.1
  # 的 Get-Content 默认按 ANSI 解码，会把中文读成乱码导致 ConvertFrom-Json 报
  # 「传入的对象无效」（乱码还会破坏引号配对）。
  $Version = (Get-Content (Join-Path $root 'wails.json') -Raw -Encoding UTF8 | ConvertFrom-Json).productVersion
}
if (-not $Version) {
  throw "无法从 wails.json 读取 productVersion，可用 -Version x.y.z 指定"
}
Write-Host "[INFO] 版本号: $Version"

$engineSrc = Join-Path $root 'python_engine\dist\lamacore'
$binDir    = Join-Path $root 'build\bin'
$engineDst = Join-Path $binDir 'lamacore'
$exePath   = Join-Path $binDir '社媒图文水印抹除工具.exe'

# ---------- 1. 引擎（PyInstaller onedir）----------
if ($SkipEngine) {
  Write-Host "[INFO] -SkipEngine：跳过引擎构建"
} else {
  $bat = Join-Path $root 'python_engine\build_engine.bat'
  if (-not (Test-Path $bat)) { throw "未找到 $bat" }
  Write-Host "[INFO] 构建 lamacore 引擎（含对齐自测 + PyInstaller，需数分钟）..."
  Push-Location (Join-Path $root 'python_engine')
  try {
    & cmd /c "`"$bat`""
    if ($LASTEXITCODE -ne 0) { throw "引擎构建失败（exit $LASTEXITCODE）" }
  } finally {
    Pop-Location
  }
}
$engineExe = Join-Path $engineSrc 'lamacore.exe'
if (-not (Test-Path $engineExe)) {
  throw "未找到 $engineExe；请先执行 python_engine\build_engine.bat（或去掉 -SkipEngine）"
}

# ---------- 2. 主程序（wails build）----------
if ($SkipApp) {
  Write-Host "[INFO] -SkipApp：跳过主程序编译"
} else {
  if (-not (Get-Command wails -ErrorAction SilentlyContinue)) {
    throw "未找到 wails，请先安装：go install github.com/wailsapp/wails/v2/cmd/wails@latest"
  }
  # MinGW-w64：wails(Windows) 需要 CGO，先确保 gcc 在 PATH
  if (-not (Get-Command gcc.exe -ErrorAction SilentlyContinue)) {
    foreach ($c in @('C:\mingw64\bin', 'D:\mingw64\bin', 'C:\msys64\mingw64\bin')) {
      if (Test-Path (Join-Path $c 'gcc.exe')) {
        $env:PATH = "$c;$env:PATH"
        Write-Host "[INFO] 已加入 MinGW 到 PATH: $c"
        break
      }
    }
  }
  $gcc = Get-Command gcc.exe -ErrorAction SilentlyContinue
  if (-not $gcc) { throw "未找到 gcc.exe（MinGW-w64），wails 在 Windows 上编译需要它" }
  $gccDir = Split-Path $gcc.Source -Parent
  $env:CC = Join-Path $gccDir 'gcc.exe'
  $env:CXX = Join-Path $gccDir 'g++.exe'
  $env:CGO_ENABLED = '1'
  Write-Host "[INFO] gcc: $($gcc.Source)"

  Write-Host "[INFO] 编译主程序（windows/amd64，WebView2 内嵌）..."
  # -ldflags 注入版本号：让界面上展示的版本与安装包版本同源（app.GetAppVersion）。
  # 不能用 `wails build -ldflags "-X main.appVersion=$Version"` 的引号写法——
  # PowerShell 会把整串当单个参数传给 wails，Go 收到后无法解析。改用数组传参，
  # PowerShell 会为含空格的元素自动补引号，语义与命令行一致。
  $ldflags = "-s -w -X main.appVersion=$Version"
  & wails build -platform windows/amd64 -webview2 embed -ldflags $ldflags
  if ($LASTEXITCODE -ne 0) { throw "wails build 失败（exit $LASTEXITCODE）" }
}

# ---------- 3. 引擎就位到 build\bin\lamacore ----------
# robocopy /MIR 增量镜像：内容未变时几乎零开销，避免整目录删除+复制
Write-Host "[INFO] 同步引擎 -> $engineDst"
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
robocopy "$engineSrc" "$engineDst" /MIR /NFL /NDL /NJH /NJS /NP | Out-Null
if ($LASTEXITCODE -ge 8) { throw "引擎同步失败（robocopy exit $LASTEXITCODE）" }
$global:LASTEXITCODE = 0

if (-not (Test-Path $exePath)) { throw "未找到主程序 $exePath" }

# ---------- 4. Inno Setup 安装器 ----------
$iscc = @(
  'C:\Program Files (x86)\Inno Setup 6\ISCC.exe',
  'C:\Program Files\Inno Setup 6\ISCC.exe',
  (Join-Path $env:LOCALAPPDATA 'Programs\Inno Setup 6\ISCC.exe')
) | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $iscc) {
  $cmd = Get-Command ISCC.exe -ErrorAction SilentlyContinue
  if ($cmd) { $iscc = $cmd.Source }
}
if (-not $iscc) {
  throw "未找到 Inno Setup 6 的 ISCC.exe，请先安装：winget install -e --id JRSoftware.InnoSetup"
}
Write-Host "[INFO] ISCC: $iscc"

$issPath = Join-Path $root 'packaging\installer.iss'
Write-Host "[INFO] 编译安装器（lzma2/max 压缩 670MB 引擎，需数分钟）..."
& $iscc "/DMyAppVersion=$Version" "$issPath"
if ($LASTEXITCODE -ne 0) { throw "ISCC 编译失败（exit $LASTEXITCODE）" }

$out = Join-Path $root "build\installer\社媒图文水印抹除工具_Setup_$Version.exe"
if (-not (Test-Path $out)) { throw "未找到预期产物 $out（请检查 installer.iss 的 OutputDir/OutputBaseFilename）" }

$item = Get-Item $out
$hash = (Get-FileHash $out -Algorithm SHA256).Hash
Write-Host ""
Write-Host "[OK] 安装器已生成: $($item.FullName)"
Write-Host "[OK] 体积: $([math]::Round($item.Length / 1MB, 1)) MB"
Write-Host "[OK] SHA256: $hash"
Write-Host ""
Write-Host "发布提示：build\installer\*.exe 受 .gitattributes 的 Git LFS 规则管理，"
Write-Host "         提交大文件前请确认已安装 git-lfs（git lfs install）。"
