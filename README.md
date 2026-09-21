# 社媒图文去水印工作台 · lama-watermark-eraser

一个基于 **LaMa AI** 的单文件桌面应用，把「社媒图文下载 / 本地图片识别」与「批量去水印」串成一条完整流水线：

**输入链接或文件夹 → 下载/识别图片（视频自动拦截）→ 预览并框选水印 → 批量去水印 → 结果预览 → 导出 zip**

> 技术栈：Go 1.25 + Wails v2.15 + Vue 3 + Python(big-lama PyTorch) 常驻子进程引擎（IOPaint 对齐）
> 上游：[advimman/lama](https://github.com/advimman/lama)（Apache-2.0）· 下载能力设计参考 [zinan92/content-downloader](https://github.com/zinan92/content-downloader)（MIT）

## 下载安装

当前版本：**1.1.10**

| 平台 | 安装包 | 安装方式 |
|---|---|---|
| Windows 10/11 (x64) | `社媒图文水印抹除工具_Setup_1.1.10.exe`（约 300MB，经 Git LFS 分发） | 双击安装，**按用户安装**（默认 `%LOCALAPPDATA%\Programs\LaMaWatermarkRemover`），无需管理员权限；中文界面，可自定义安装目录；含卸载入口 |
| macOS (Apple Silicon) | `暂无安装包` | 打开镜像，把应用拖入「应用程序」。应用为 ad-hoc 签名（未公证），首次打开需右键 →「打开」放行，详见镜像内《首次打开说明.txt》 |

- 两个安装包都已内置 big-lama 权重与完整推理引擎，**无需额外下载模型**
- 兼容非 ASCII 安装路径；卸载/退出时自动回收引擎子进程

## 功能特性

- **社媒图文下载**：支持微信公众号文章、小红书图文、抖音图文（纯 Go 原生实现，零 Python 依赖）
- **本地文件夹模式**：无需链接，直接选择本机文件夹，识别并批量去水印
- **多水印区域框选**：可拖拽框选多个水印区域，合并为单掩膜推理；支持绝对位置 / 按比例适配两种定位
- **智能推理策略**：默认 `auto` 按每张图水印占比自动选择整图推理（效果最佳）或逐框裁剪推理（大图更快）；GUI 可手动切换
- **LaMa 批量修复**：big-lama PyTorch 权重 CPU 推理（与 IOPaint 输出逐像素对齐），原生分辨率、无缩放
- **引擎状态可视**：启动期自动预热并推送状态（starting/ready/error），未就绪时禁用消除按钮
- **批量 / 勾选处理**：全部处理或仅处理勾选图片
- **结果预览与导出**：左右滑动预览、点击放大，一键导出 zip 压缩包；第 2 步还支持「源图打包」导出去水印前的原图
- **BGM 下载与试听**：自动识别小红书 / 抖音图文的背景音乐，可单独另存为独立文件，或存入源图目录随 zip 打包；第 3 步内嵌播放控件（播放/暂停、进度跳转、时间），优先播放已下载的本地 BGM 文件，未下载时按需抓取到本机试听缓存
- **BGM 取源容错**：抖音音频按「候选链」逐个尝试（页面 `url_list` 全部地址 → 由 `music.id` 构造的确定性直链 → detail 接口兜底），单条 CDN 地址失效不再导致整体失败；落盘前校验响应体魔数，风控页 / 错误 JSON 会被拒绝而不会存成无法播放的假音频
- **视频链接拦截**：自动识别视频链接并提示不支持（仅支持图文）
- **版本信息可见**：界面底部作者签名处标注当前版本号（与安装包版本同源，构建时注入）；顶栏「注意事项」旁提供「更新明细」入口，弹窗内按版本列出更新内容，便于用户确认自己使用的版本与变更

## 工作流程

```
输入社媒链接 ──┬──► 下载图片（视频拦截）
               └──► 选择本地文件夹 ──► 识别图片
                              │
                              ▼
                        预览图片列表（勾选/预览分离）
                              │
                              ▼
                    拖拽框选水印（支持多区域、调整大小）
                              │
                              ▼
                     LaMa 批量去水印（进度实时可见）
                              │
                              ▼
                        结果预览（点击放大）
                              │
                              ▼
                         导出 zip 压缩包
```

## 目录结构

```
├── main.go                  # CLI/GUI 分流；WebView2 数据目录
├── app.go                   # Wails 绑定 + 流水线调度 + engine:status 事件
├── internal/
│   ├── downloader/          # 纯 Go 社媒图文下载器
│   │   ├── router.go        # 平台识别 / 短链展开 / 视频拦截
│   │   ├── wechat.go        # 公众号 HTML→图片
│   │   ├── xhs.go           # 小红书 __INITIAL_STATE__ 解析（含视频拦截）
│   │   ├── douyin.go        # 抖音分享页解析
│   │   └── httpclient.go    # UA/Referer/Cookie/防盗链
│   ├── inpaint/             # 去水印引擎客户端与流水线
│   │   ├── python_client.go # Python 子进程 JSONL 客户端（进程树管理/超时/懒重启）
│   │   ├── protocol.go      # JSONL 信封与 RGB888 编解码
│   │   ├── pipeline.go      # 批量调度 / 掩膜 / original|crop 策略
│   │   └── engine.go        # 引擎工厂（伴生 lamacore 或开发期环境变量）
│   ├── ziputil/             # 结果打包
│   └── cli/                 # 命令行模式
├── python_engine/           # big-lama 推理引擎（PyInstaller 打包为 lamacore/）
│   ├── inpaint_core.py      # IOPaint 对齐核心（pad mod8 symmetric / norm / forward）
│   ├── worker.py            # stdin/stdout JSONL 常驻子进程
│   ├── lamacore.spec        # PyInstaller onedir 配置
│   ├── align_smoke.py       # 打包前像素级对齐自测
│   ├── build_engine.bat     # 一键构建脚本（Windows）
│   ├── build_engine.sh      # 一键构建脚本（macOS / Linux）
│   └── requirements.txt     # 依赖（torch CPU / numpy / pillow）
├── packaging/               # 打包脚本
│   ├── installer.iss        # Inno Setup 6 安装器脚本（Windows）
│   ├── build_windows.ps1    # Windows 一键打包：引擎 + 主程序 + 安装器
│   ├── package_macos.sh     # macOS：集成 lamacore + ad-hoc 签名
│   └── make_dmg.sh          # macOS：生成 DMG 安装器
└── frontend/                # Vue3 流水线界面（Apple 流体设计）
```

最终发布产物布局（lamacore 与主程序同目录分发）：

```
社媒图文水印抹除工具.exe
lamacore/
├── lamacore.exe             # 引擎入口（console 子进程，stdin/stdout JSONL）
└── _internal/               # torch / numpy / 引擎脚本
    └── models/big-lama.pt   # 权重（205MB，随包分发）
```

## 构建

### 前置依赖

- Go 1.25+
- [Wails CLI](https://wails.io) v2
- MinGW-w64（cgo 编译）
- Node 18+
- Python 3.10（优先）或 3.12（打包 lamacore 引擎用；降级路径详见下文）

### 拉取仓库

```bash
git clone https://github.com/chenchen-0212/lama-watermark-eraser.git
cd lama-watermark-eraser
```

### 编译 AI 引擎（lamacore）

```bat
cd python_engine
build_engine.bat
```

脚本行为：建 venv（优先 Python 3.10 + torch 2.2.2 CPU；本机无 3.10 时按终裁降级为
Python 3.12 + 系统 torch，venv 以 `--system-site-packages` 复用已装 torch，并在脚本头注释注明）
→ 安装依赖与 pyinstaller → 先跑 `align_smoke.py` 像素级对齐自测（不过不许打包）
→ PyInstaller onedir 打包（权重经 `LAMA_MODEL_SRC` 注入，缺省
`D:\clear_mask\lama\watermark_tool\models\big-lama.pt`）→ 产出 `python_engine\dist\lamacore\`。

### 编译主程序

```bash
# Windows（需 MinGW-w64，CC 使用 Windows 绝对路径）
export CC='D:\mingw64\bin\gcc.exe' CXX='D:\mingw64\bin\g++.exe'
export CGO_ENABLED=1
export PATH="/d/mingw64/bin:$PATH"
wails build -platform windows/amd64 -webview2 embed

# 产物：build/bin/社媒图文水印抹除工具.exe
# 分发：把 python_engine/dist/lamacore 整目录复制到 build/bin/ 下（与主 exe 同级）
```

主程序启动时按 `exe 同级 lamacore/lamacore.exe` → `exe 同级 lamacore.exe` → cwd 同规则
自动定位引擎；找不到时经 `engine:status` 事件报可读错误（不 panic）。

### 打包 Windows 安装器（Inno Setup 6）

```bat
:: 先完成上述引擎与主程序编译，并确认 build/bin/ 下已就位主 exe 与 lamacore\ 目录
"C:\Program Files (x86)\Inno Setup 6\ISCC.exe" /DMyAppVersion=1.1.10 packaging\installer.iss
:: 产物：build\installer\社媒图文水印抹除工具_Setup_<版本>.exe
```

按用户安装（`PrivilegesRequired=lowest`，
默认 `%LOCALAPPDATA%\Programs\LaMaWatermarkRemover`）、中文界面、目录页强制显示、
非 ASCII 安装路径兼容；lamacore 整目录以 lzma2/max 压缩递归打包。

一键打包（引擎 + 主程序 + 安装器，版本号自动读取 `wails.json`）：

```powershell
powershell -ExecutionPolicy Bypass -File packaging\build_windows.ps1
# 可选：-SkipEngine（复用已有引擎，省几十分钟） -SkipApp（只重打安装器） -Version 1.1.10
```

版本号单一来源：`wails.json` 的 `productVersion`（macOS 的 `CFBundleShortVersionString`
与 Windows 安装器版本都由它派生）；`installer.iss` 内的 `MyAppVersion` 仅作默认兜底，
打包脚本会通过 `/DMyAppVersion=` 覆盖。

### 打包 macOS 安装器（DMG）

```bash
# 1) 编译 .app（Apple Silicon）
wails build -platform darwin/arm64
# 2) 集成 lamacore 引擎 + ad-hoc 签名
bash packaging/package_macos.sh
# 3) 生成 DMG 安装器（含 Applications 拖拽链接与首次打开说明）
bash packaging/make_dmg.sh
# 产物：build/installer/社媒图文水印抹除工具_<版本>_arm64.dmg
```

macOS 包为 **ad-hoc 签名**（未做 Apple 公证），首次打开需右键 →「打开」放行；
对外分发需 Apple Developer ID 签名并公证。

### 开发期（不打包，直接驱动本机 Python）

设置环境变量后，主程序/CLI 会跳过 lamacore 定位，直接用指定 Python 运行 worker.py：

```bash
export LAMA_ENGINE_PYTHON='C:\path\to\python.exe'   # 需已装 torch/numpy
export LAMA_ENGINE_WORKER='D:\repo\python_engine\worker.py'   # 缺省 <cwd>/python_engine/worker.py
export LAMA_ENGINE_MODEL='D:\path\to\big-lama.pt'   # 缺省由 worker 自动定位
```

## 使用说明

### 图形界面

双击 `社媒图文水印抹除工具.exe`，然后：

1. **选择来源**：粘贴社媒图文链接点「下载图片」，或点「选择本地文件夹」直接处理本机图片；此步可用「⬇ 源图打包」把原图导出为 zip
2. **框选水印**：在左侧大图上按住左键拖拽框选水印区域（可框选多个、拖动八向手柄调整大小）
3. **批量去水印**：选择处理策略（默认「智能」自动权衡速度与效果，可切「整图」/「逐框裁剪」），点「全部去水印」或「仅勾选(n)」，进度实时可见
4. **预览与导出**：点击图片放大查看，点「导出 zip 压缩包」选择保存位置

### 命令行模式

```bash
# 仅下载图片
社媒图文水印抹除工具.exe --url <社媒链接>

# 本地文件夹去水印（单个框选）
社媒图文水印抹除工具.exe --input <图片文件夹> --output <输出目录> --box x1,y1,x2,y2

# 按比例坐标（0..1），多个水印区域用 | 分隔
社媒图文水印抹除工具.exe --input <图片文件夹> --output <输出目录> \
  --relative-boxes "0.02,0.02,0.3,0.25|0.65,0.05,0.95,0.3"

可选参数：
  --dilate N       掩膜边缘外扩（默认 12）
  --margin N       修复上下文边距（默认 64）
  --strategy S     处理策略：auto（智能，默认）| original（整图推理）| crop（逐框裁剪推理）
  --fast           快速模式：等价 --strategy crop（两者不可同时使用）
  --mask file.png  不规则水印掩膜（白色=水印区域）
  --recursive      递归子文件夹
  --zip            完成后自动打包 zip
```

> auto 策略规则：按合并掩膜 bbox 外扩 margin 后的面积占比判断——占比 ≤ 60% 走
> crop（多次小前向更快），否则走 original（整图单次前向效果最佳）；裁剪上下文
> 边距下限 256，对齐 IOPaint CROP 默认值。

## 平台支持

| 平台 | 状态 |
| --- | --- |
| 微信公众号 | ✅ 完全可用（零配置） |
| 小红书 | ⚠️ 解析链路已验证；图文下载需 App「分享→复制链接」的最新链接（xsec_token 约 5 分钟有效） |
| 抖音 | ⚠️ 风控较强；支持 `/note/{id}`、`/video/{id}` 与 `?modal_id={id}` 形态，**建议配置 Cookie 提高成功率**（见下） |
| 视频链接 | 🚫 自动识别并提示不支持（仅支持图文） |
| 本地文件夹 | ✅ 完全可用 |

### 抖音 Cookie 配置（可选，推荐）

抖音对未登录请求风控较强，容易失败。在初始页点击「🍪 抖音 Cookie 设置」，按弹窗指引复制浏览器 Cookie 即可：

1. 用浏览器打开 `www.douyin.com` 并登录；
2. `F12` → Network 标签 → 刷新页面 → 点第一个请求；
3. 在 Request Headers 中找到 `Cookie`，复制完整值粘贴保存。

Cookie 仅保存在本机 `%LOCALAPPDATA%\LaMaWatermarkRemover\douyin_cookie.txt`，不会上传到任何服务器。

## 引擎与对齐说明

引擎为 [big-lama](https://github.com/advimman/lama) PyTorch JIT 权重（`big-lama.pt`），
经 Python 常驻子进程推理，Go 侧经 stdin/stdout JSONL 协议通信（半双工同步 + stderr
排空 + 进程树管理）。推理实现逐行对齐 IOPaint `LaMa.forward` 语义：

- 输入整图 RGB888 + 单通道 0/255 掩膜，**无缩放无 512 画布**（原生分辨率）
- 四边 `np.pad(mode="symmetric")` 填充到 8 的倍数（IOPaint `_pad_forward` 同款）
- 归一化 /255 → CHW；mask `(m>0)*1` int64；`torch.no_grad()` 前向；输出 `clip(0,255)` 后裁回原尺寸
- 贴回仅覆写掩膜区，非掩膜区像素保持原图不变

打包前 `python_engine/align_smoke.py` 会以独立参考实现逐像素比对 worker 推理路径
（覆盖奇数 / 非 8 对齐尺寸，断言 max diff ≤ 1），对齐不过不许出包。

## 版权与免责声明

- 本工具基于 [advimman/lama](https://github.com/advimman/lama)（Apache-2.0）与 [zinan92/content-downloader](https://github.com/zinan92/content-downloader)（MIT）的公开代码与思路二次开发，遵守相应开源协议。
- **本工具仅用于处理您本人拥有合法权利的内容。** 未经授权去除他人图片水印可能侵害著作权，抓取平台内容可能违反平台用户协议。
- **使用者需自行承担因使用本工具所产生的一切风险、责任与法律后果；开发者不对任何侵权、违规或不当使用行为承担责任。**
- 请遵守各平台用户协议与相关法律法规。

## License

[Apache-2.0](LICENSE)（继承上游 LaMa 的许可证）
