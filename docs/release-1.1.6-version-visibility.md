# v1.1.6 需求实现与验证报告 —— 版本信息可见（版本号标注 + 更新明细入口）

## 一、需求

两个界面增强任务，要求**沿用既有的版本方案**并**保持与现有界面风格与结构一致**：

1. **P2 — 底部版本号标注**：在应用底部作者信息文本旁标注当前版本号。
2. **P3 — 更新明细入口**：在「注意事项」按钮旁新增「当前版本更新明细」入口，展示本版本更新内容。

## 二、需求编号说明（重要）

> 用户所称「p2 / p3」**不是** `docs/prd-incremental-iopaint-alignment.md` 里的 P2 需求池
> （那三项是 GPU/CUDA、高清快速模式切换、打包形态优化），因为那三项与「版本号 + 更新明细」
> 完全无关。据用户的实际描述，本报告按**本轮两个 UI 任务的序号**理解（P2 = 版本号标注，
> P3 = 更新明细入口）。实施前已在此显式标注该理解，以便纠偏。

## 三、实现

### 1. 版本号单一来源与注入链路

版本号仍以 `wails.json` 的 `productVersion` 为**唯一来源**，但此前它只流向安装器，
界面无法获知。本轮补上「流向运行时界面」的一段：

```
wails.json: productVersion
        │
        ├──► packaging/build_windows.ps1 读取
        │            │
        │            ├──► ISCC /DMyAppVersion=…  ──► 安装包版本 / 卸载入口版本
        │            └──► wails build -ldflags "-X main.appVersion=…"  ──► 界面版本号
        │
        └──► (macOS) CFBundleShortVersionString
```

- `main.go` 新增包级变量 `appVersion string`，由构建时 `-ldflags -X` 写入。
- `app.go` 新增绑定方法 `GetAppVersion()`：未注入（`go run` / `go test` 场景）时回退
  为 `"dev"`，**不返回空串** —— 前端据此区分「拿到版本号」与「未拿到」。
- `packaging/build_windows.ps1` 的主程序编译命令补上 `-ldflags`。

**PowerShell 引号陷阱（踩过）**：不能写成
`& wails build … -ldflags "-X main.appVersion=$Version"` —— PowerShell 会把整串当作单个
参数传给 wails，Go 侧收到后无法解析。改用变量传参：
`$ldflags = "-s -w -X main.appVersion=$Version"` 再 `-ldflags $ldflags`，
PowerShell 会为含空格的元素自动补引号，语义与命令行一致。

### 2. 底部版本号标注（P2）

`App.vue` 的 `.footer-sign` 由纯文本改为「作者 csy · v1.1.6」结构：

- 版本号来自后端 `GetAppVersion()`，**前端不硬编码**，因此不会出现「界面显示的版本」
  与「安装包版本」不一致。
- 取不到版本号时（Promise 失败或返回空）**整段版本标注不渲染**，只显示「作者 csy」——
  宁可少显示，也不显示一个可能错误的版本号。
- 视觉沿用 footer 既有规格（`font-size: 11.5px` / `var(--text-3)` / `letter-spacing: 0.4px`），
  版本号加一枚胶囊徽标（`border-radius: 999px` + `inset box-shadow` 描边），与顶栏
  「注意事项」按钮同属「胶囊 + 描边」语言；`font-variant-numeric: tabular-nums` 保证
  数字等宽，避免切版本时抖动。

### 3. 更新明细入口与弹窗（P3）

- 入口：顶栏 `.title-row` 内、「注意事项」按钮**右侧**新增一个按钮。
  **结构完全复用** `.notice-btn`（同一 padding / 圆角 / 内描边 / hover 与 active 动效），
  仅通过 `.changelog-btn` 换主色为蓝（`--accent`），与「注意事项」的橙（`--warn`）区分，
  避免两个按钮外观雷同而无法分辨。
- 弹窗：新增 `changelog-panel`，**复用既有** `.notice-mask / .notice-panel / .notice-head /
  .notice-body / .notice-foot` 全套结构与尺寸约束（`min(620px, 94vw)` 放宽到 640px 以容纳
  条目文本），底部操作按钮与「注意事项」一致使用 `.btn.btn-primary`。
- 内容按版本分组，每组含版本号、标题、日期与条目；条目带类型标记（新增/修复/优化/变更），
  标记定宽 34px 使正文左边缘齐平。
- `Esc` 关闭：`onKey` 的优先级链中插入 `showChangelog`（在 `showNotice` 之后），
  与现有弹窗关闭顺序保持一致。

### 4. 更新明细数据源

新增 `frontend/src/changelog.js`：

- 导出 `changelog` 数组（**首项即当前版本**）、`KIND_LABEL` 映射、`kindLabel()` 与
  `latestVersion()` 辅助函数。
- 已收录 v1.1.6 / v1.1.5 / v1.1.4 三个版本，其中后两个据既有 release 报告回溯补录。
- 文件头部注释写明维护约定：每次发版新增一条、首项 `version` 必须与 `wails.json` 的
  `productVersion` 一致、`kind` 枚举需同步补 `KIND_LABEL`。
- `kindLabel()` 对未知 kind 原样返回，界面不会出现空白标记。

## 四、版本号递增（1.1.5 → 1.1.6）

按既有约定同步以下位置，**均无残留 `1.1.5`**：

| 文件 | 变更 |
|---|---|
| `wails.json` | `productVersion`（两处）→ 1.1.6 |
| `packaging/installer.iss` | `#define MyAppVersion` 默认值 + 头部注释与产物名 |
| `packaging/build_windows.ps1` | `-Version` 用法注释 |
| `README.md` | 当前版本、两个安装包文件名、ISCC 命令、`-Version` 示例 |

`README.md` 另补特性条目「版本信息可见」。

## 五、验证

### 5.1 静态与单元测试

| 检查 | 结果 |
|---|---|
| `go vet ./...` | 通过 |
| `go test ./internal/downloader/... . -count=1` | 全绿（含新增 `TestGetAppVersion`） |
| 前端 `vite build` | 通过，20 modules，无警告 |

新增 `TestGetAppVersion` 覆盖两种语义：

- 未注入 → `"dev"`（**断言不为空串**，这是前端是否显示版本标注的判据）；
- 注入 `1.1.6` → 原样返回。

### 5.2 产物特征串校验

防止「改了源码但产物是旧的」，在三层各做一次：

| 层 | 命中项 |
|---|---|
| 前端源码 `App.vue` | `GetAppVersion` / `appVersion` / `showChangelog` / `更新明细` / `version-tag` / `changelog-panel` |
| 前端产物 `dist/assets/*` | `更新明细` / `作者 csy` / `1.1.6` / `GetAppVersion`（js）；`version-tag` / `changelog-panel` / `rel-kind`（css） |
| 主程序 exe | `1.1.6` / `GetAppVersion` / `更新明细` / `版本更新明细` / `作者 csy` / `version-tag` / `changelog-panel` / `抖音 BGM 取源容错` / `版本信息可见` |

> 注：exe 里出现 `1.1.5` / `1.1.4` 属**预期**——它们来自 `changelog.js` 的历史版本明细文本，
> 不是版本号残留。版本号残留的判据是「元数据文件内 `1.1.5` 计数为 0」，该判据已通过。

### 5.3 GUI E2E（真实点击 + 截图）

用 `qa_workspace/scripts/smoke_gui_capture.py`（零依赖 PrintWindow 截图 + 真实 mouse_event）：

| 场景 | 结果 |
|---|---|
| 启动 → 初始界面 | 窗口 `1064x721`，非空白（214 种颜色）；底部渲染出「作者 csy · **v1.1.6**」 |
| 顶栏 | 「⚠ 注意事项」（橙）与「🆕 更新明细」（蓝）并列，胶囊样式一致 |
| 点击「更新明细」 | 真实点击后弹窗出现，点击前后像素差异 **97.07%**，颜色种类 214 → 423 |
| 弹窗内容 | 正确渲染 `v1.1.6「当前」版本信息可见 2026-09-15`、`v1.1.5 抖音 BGM 取源容错`、`v1.1.4 BGM 试听`；类型标记（新增/修复/优化）配色正确、定宽对齐 |
| 退出 | 无残留 `lamacore` 子进程 |

**定位修正记录**：首次点击用了估算坐标 `(322,45)`，误命中「注意事项」弹窗（截图为
使用注意事项）。改为**按主色质心精确定位**——扫描 `#0a84ff` 像素得 x 357–419、
质心 `(388,35)`，据此重跑才命中「更新明细」。这说明验证不能靠估算坐标，必须用像素定位。

## 六、产物

| 产物 | 大小 | SHA256 |
|---|---|---|
| 主程序 `build/bin/社媒图文水印抹除工具.exe` | 14,202,880 B (13.5 MiB) | `d5415ff6cc80254ee797c2a00397dfee8dbde5d76b7e50e8b4ed965ffb84e982` |
| 安装器 `build/installer/社媒图文水印抹除工具_Setup_1.1.6.exe` | 314,907,947 B (300.3 MiB) | `5d5661f99397e5a91d7744b9ac01a180f28ad829dc826416c0499cc320b8890b` |

- 主程序构建耗时 42.4 s，LDFlags 确认为 `-s -w -X main.appVersion=1.1.6`。
- 安装器 ISCC 编译耗时 164.3 s，`ISCC_EXIT=0`。

## 七、未完成 / 边界

- **更新明细需手工维护**：`changelog.js` 是手写数据源，发版时需人工补录。若能接受
  从 `docs/release-*.md` 解析，可后续自动化，但会引入构建期依赖与格式耦合，本轮未做。
- **未做「新版本可升级」提示**：当前只展示本地已记录的更新内容，不检查远程新版本。
- **历史明细可能不全**：v1.1.5 / v1.1.4 条目据 release 报告回溯补录，早于 v1.1.4 的版本
  （1.1.0–1.1.3）未收录 —— 其变更记录在既有文档中已不完整，不臆造。
- **仅出 Windows 包**：macOS 的 DMG 需在 macOS 上构建，本轮未构建。
- **macOS 打包链路已补文档但未验证**：`package_macos.sh` 的前置说明已补上版本注入命令
  （`wails build … -ldflags "-s -w -X main.appVersion=$VERSION"`），否则 macOS 包的界面
  会显示 `dev`。该改动是**注释级**的，未在 macOS 上实跑验证。
