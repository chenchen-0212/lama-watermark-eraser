# v1.1.4 需求实现与验证报告 —— 第三步 BGM 试听

## 一、需求

> 在第三步页面新增一个 BGM 试听功能：提供播放控件，点击可播放已下载的 BGM 文件；该功能的显示与隐藏逻辑需与现有的 BGM 按钮完全一致。功能实现并验证通过后，将程序打包为 exe 可执行文件，并生成对应的安装器。

## 二、实现方案

### 1. 显示/隐藏逻辑：与 BGM 按钮同源

第三步（`store.stage === 'downloaded'`）原有的「🎵 下载BGM」按钮条件是 `v-if="hasBGM"`：

```js
const hasBGM = computed(() => !!(store.post && store.post.audioUrl))
```

新增的试听控件**复用同一个 `hasBGM`**，未新增任何条件分支，因此两者的出现/消失时机严格一致（帖子自带 `audioUrl` 时才出现；本地文件夹模式 `audioUrl=''` 时两者一起隐藏）。

| 位置 | 元素 | 条件 |
|---|---|---|
| `App.vue:451` | `🎵 下载BGM / BGM 已下载` 按钮 | `v-if="hasBGM"` |
| `App.vue:462` | BGM 试听条（播放/暂停 + 进度 + 时间） | `v-if="hasBGM"` |

### 2. 播放源策略：已下载的本地文件优先

`采用「本地文件 > 远端抓取」的两级取源，全部由后端完成（WebView 直连社媒音频 CDN 会因防盗链 403）：

```
点击 ▶
  └─ GetBGMAudio(audioURL, store.bgmSaved)
       ├─ ① store.bgmSaved 非空且文件存在 → 直接读本地文件（=「已下载的 BGM 文件」）
       └─ ② 否则把 audioURL 下载到试听缓存 %LOCALAPPDATA%\LaMaWatermarkRemover\bgm-cache\<sha1(url)8>\bgm.<ext>
              （同一地址命中缓存不重复下载），再读回
     └─ 读取结果 → data:<mime>;base64,… → 前端 Audio 实例播放
```

- MIME 由扩展名映射（`.mp3/.m4a/.aac/.wav/.ogg/.flac`），未知扩展名按 `audio/mpeg` 兜底；
- 体积上限 24 MiB，超过返回「音频过大（N MB），暂不支持在线试听」；
- 前端另有会话级 `Map<audioUrl, dataURL>` 缓存，重复播放不再走网桥。

### 3. 交互细节

- 播放/暂停切换、点击进度条跳转、`当前时间 / 总时长` 显示；
- 取源期间按钮显示 ⏳ 并禁用；播放失败在控件下方红字提示 + Toast；
- 离开第三步（开始去水印/换链接）自动暂停但保留取源，返回时秒开；切换帖子则清空取源；
- 组件卸载时停止播放。

## 三、变更文件

| 文件 | 变更 |
|---|---|
| `app.go` | 新增 `GetBGMAudio` + `audioDataURL` / `firstAudioFile` / `shortHash` / `audioExtMime`（+100 行） |
| `app_test.go` | 新增 3 个用例（+101 行） |
| `frontend/src/App.vue` | 试听控件模板 + 播放逻辑 + 样式（+242 行） |
| `frontend/wailsjs/go/main/{App.js,App.d.ts}` | `wails generate module` 重新生成的绑定 |
| `wails.json` / `packaging/installer.iss` / `packaging/build_windows.ps1` / `README.md` | 版本号 1.1.3 → 1.1.4；README 补充「BGM 下载与试听」特性说明 |
| `packaging/build_windows.ps1` | 顺带修复：读 `wails.json` 时补 `-Encoding UTF8`（原按 ANSI 解码中文导致 `ConvertFrom-Json` 失败） |

## 四、验证证据

### 1. 后端单测（`go test ./...` 全绿）

```
--- PASS: TestGetBGMAudioLocalFile          # 本地文件 → data URL，MIME 正确，内容逐字节一致
--- PASS: TestGetBGMAudioNoSource           # 空/空白 audioURL、不存在的 localPath → 友好报错
--- PASS: TestGetBGMAudioDownloadAndCache   # httptest 抓取 + 二次调用命中缓存（只请求 1 次）
```

### 2. 前端构建

`npm run build` 通过（`✓ 19 modules transformed`）；`dist` 中可见 `试听背景音乐` / `GetBGMAudio` / `bgm-player` / `player-fill`。

### 3. 打包产物自检

```
$ grep -a -c "试听背景音乐" build/bin/社媒图文水印抹除工具.exe      → 1
$ grep -a -c "GetBGMAudio"  build/bin/社媒图文水印抹除工具.exe      → 3
$ grep -a -o "bgm-cache"    build/bin/社媒图文水印抹除工具.exe      → bgm-cache
```

### 4. GUI 端到端验证（真实点击真实播放）

用 `qa_workspace/scripts/smoke_gui_capture.py`（零依赖：ctypes + GDI + 手写 PNG）启动**打包后的 exe**，并用 `qa_workspace/scripts/make_e2e_seed.py` 注入一份带 BGM 的第三步状态做验证：

| 步骤 | 结果 |
|---|---|
| 窗口出现 | `title='社媒图文去水印工作台'`，客户区 1064×721，内容非空白 |
| 第三步渲染 | 「下载BGM」按钮与试听条**同屏出现**（`e2e_step3_bgm_player.png`） |
| 真实点击 ▶（客户区 584,355） | 按钮变 ⏸，时间 `0:00/0:00` → `0:03/0:04`，进度条推进（`e2e_step3_after_click.png`） |
| 后端取源 | 本地 HTTP 服务日志 `GET /bgm.wav 200`（**仅 1 次**） |
| 缓存落盘 | `%LOCALAPPDATA%\LaMaWatermarkRemover\bgm-cache\7c691d0791e544d9\bgm.wav`，176,444 字节 |
| 音频一致性 | 缓存文件 SHA256 `a0ce0a00623839512f381ec35c8282d3e5a08fb28a4876831cf95d754e0f1ed0` **= 源文件 SHA256** |
| 退出回收 | 主进程退出后无残留 `lamacore` 进程 |

> E2E 种子构建使用独立输出名 `-o e2e_bgm_step3.exe`，验证期间正式 exe 的 SHA256 前后一致（`344e909f…`），未受污染；种子已还原、临时 exe 与测试缓存均已清理。

## 五、交付产物

| 产物 | 路径 | 体积 | SHA256 |
|---|---|---|---|
| 可执行文件 | `build\bin\社媒图文水印抹除工具.exe` | 13.5 MB | `344e909f176101ace6c555c9ef7d38a1b1408ba7846817ed8fedb7bdc4f4b33f` |
| 安装器 | `build\installer\社媒图文水印抹除工具_Setup_1.1.4.exe` | 300.3 MB | `a0d9d637a3a828eb2a0af2aef5c658021f1a2ed682940fe6d89bf4ca8e81d570` |

安装器为 Inno Setup 6（`lzma2/max`），按用户安装到 `%LOCALAPPDATA%\Programs\LaMaWatermarkRemover`，无需管理员权限；打包内容 = 主程序 + 内嵌 big-lama 的 `lamacore\` 引擎束。

## 六、已知限制

1. 音频以 data URL 经网桥传入前端，>24 MiB 的音频不支持试听（避免一次性占用过多内存）。
2. `GetBGMAudio` 的下载分支依赖社媒音频 CDN 可访问；风控/链接过期时会在控件下方提示失败原因，不影响下载 BGM 与去水印主链路（试听为可选项）。
3. 真实平台的 BGM 试听需用线上链接人工确认（本次用本地 HTTP 音频源完成了等价的端到端链路验证）。
