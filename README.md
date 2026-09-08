# 社媒图文去水印工作台 · lama-watermark-eraser

一个基于 **LaMa AI** 的单文件桌面应用，把「社媒图文下载 / 本地图片识别」与「批量去水印」串成一条完整流水线：

**输入链接或文件夹 → 下载/识别图片（视频自动拦截）→ 预览并框选水印 → 批量去水印 → 结果预览 → 导出 zip**

> 技术栈：Go 1.25 + Wails v2.15 + Vue 3 + onnxruntime_go（LaMa ONNX CPU 推理）
> 上游：[advimman/lama](https://github.com/advimman/lama)（Apache-2.0）· 下载能力设计参考 [zinan92/content-downloader](https://github.com/zinan92/content-downloader)（MIT）

## 功能特性

- **社媒图文下载**：支持微信公众号文章、小红书图文、抖音图文（纯 Go 原生实现，零 Python 依赖）
- **本地文件夹模式**：无需链接，直接选择本机文件夹，识别并批量去水印
- **多水印区域框选**：可拖拽框选多个水印区域，逐个独立修复；支持绝对位置 / 按比例适配两种定位
- **LaMa 批量修复**：ONNX CPU 推理，单张约 2–4 秒；≤512px 原生分辨率，>512px 等比缩放
- **批量 / 勾选处理**：全部处理或仅处理勾选图片
- **结果预览与导出**：左右滑动预览、点击放大，一键导出 zip 压缩包
- **视频链接拦截**：自动识别视频链接并提示不支持（仅支持图文）

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
├── main.go                  # CLI/GUI 分流；资源解压；WebView2 数据目录
├── app.go                   # Wails 绑定 + 流水线调度 + 事件
├── internal/
│   ├── downloader/          # 纯 Go 社媒图文下载器
│   │   ├── router.go        # 平台识别 / 短链展开 / 视频拦截
│   │   ├── wechat.go        # 公众号 HTML→图片
│   │   ├── xhs.go           # 小红书 __INITIAL_STATE__ 解析（含视频拦截）
│   │   ├── douyin.go        # 抖音分享页解析
│   │   └── httpclient.go    # UA/Referer/Cookie/防盗链
│   ├── inpaint/             # LaMa 去水印引擎
│   │   ├── model.go         # onnxruntime_go 会话；固定 512 画布；NCHW
│   │   └── pipeline.go      # 批量调度 / 掩膜 / 多框修复
│   ├── ziputil/             # 结果打包
│   └── cli/                 # 命令行模式
├── resources/               # go:embed 运行时与模型（Git LFS 管理）
│   ├── onnxruntime.dll      # ONNX 运行时（16MB）
│   └── lama_fp32.onnx       # LaMa 模型权重（199MB）
└── frontend/                # Vue3 流水线界面（Apple 流体设计）
```

## 构建

### 前置依赖

- Go 1.25+
- [Wails CLI](https://wails.io) v2
- MinGW-w64（cgo 编译，用于 onnxruntime_go）
- Node 18+
- Git LFS（拉取大文件）

### 拉取仓库（含模型）

```bash
# 模型与运行时由 Git LFS 托管，clone 时需拉取 LFS 对象
git lfs install
git clone https://github.com/chenchen-0212/lama-watermark-eraser.git
cd lama-watermark-eraser
git lfs pull
```

### 编译打包

```bash
# Windows（需 MinGW-w64，CC 使用 Windows 绝对路径）
export CC='D:\mingw64\bin\gcc.exe' CXX='D:\mingw64\bin\g++.exe'
export CGO_ENABLED=1
export PATH="/d/mingw64/bin:$PATH"
wails build -platform windows/amd64 -webview2 embed

# 产物：build/bin/社媒图文水印抹除工具.exe（单文件，内嵌模型与运行时）
```

## 使用说明

### 图形界面

双击 `社媒图文水印抹除工具.exe`，然后：

1. **选择来源**：粘贴社媒图文链接点「下载图片」，或点「选择本地文件夹」直接处理本机图片
2. **框选水印**：在左侧大图上按住左键拖拽框选水印区域（可框选多个、拖动八向手柄调整大小）
3. **批量去水印**：点「全部去水印」或「仅勾选(n)」，进度实时可见
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
  --mask file.png  不规则水印掩膜（白色=水印区域）
  --recursive      递归子文件夹
  --zip            完成后自动打包 zip
```

## 平台支持

| 平台 | 状态 |
| --- | --- |
| 微信公众号 | ✅ 完全可用（零配置） |
| 小红书 | ⚠️ 解析链路已验证；图文下载需 App「分享→复制链接」的最新链接（xsec_token 约 5 分钟有效） |
| 抖音 | ⚠️ 风控较强，分享页接口不稳定，失败时给出可读提示，建议用本地文件夹模式 |
| 视频链接 | 🚫 自动识别并提示不支持（仅支持图文） |
| 本地文件夹 | ✅ 完全可用 |

## 512 适配策略

ONNX 版 `lama_fp32` 固定 512×512 输入（batch 维动态）。裁剪 ≤512 时反射填充到 512（原生分辨率，不缩放）；裁剪 >512 时等比缩放到 512 内切，推理后缩回贴回。

## 版权与免责声明

- 本工具基于 [advimman/lama](https://github.com/advimman/lama)（Apache-2.0）与 [zinan92/content-downloader](https://github.com/zinan92/content-downloader)（MIT）的公开代码与思路二次开发，遵守相应开源协议。
- **本工具仅用于处理您本人拥有合法权利的内容。** 未经授权去除他人图片水印可能侵害著作权，抓取平台内容可能违反平台用户协议。
- **使用者需自行承担因使用本工具所产生的一切风险、责任与法律后果；开发者不对任何侵权、违规或不当使用行为承担责任。**
- 请遵守各平台用户协议与相关法律法规。

## License

[Apache-2.0](LICENSE)（继承上游 LaMa 的许可证）
