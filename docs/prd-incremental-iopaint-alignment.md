# 增量 PRD：去水印引擎效果对齐 IOPaint（big-lama PyTorch 子进程改造）

> 作者：许清楚（产品经理）｜日期：2026-09-08
> 项目：`lama-watermark-eraser`（本 PRD 仅描述变更部分，既有功能不重复）

## 1. 项目信息
- 技术栈：Go（Wails v2）主程序 + Vue3 前端；本次新增 Python 子进程（PyInstaller 打包为独立 exe）
- 原始需求：将现有 ONNX 版 big-lama 去水印引擎替换为 PyInstaller 打包的 Python 子进程跑 big-lama 原版（PyTorch 原权重），对齐 IOPaint 消除效果，满足「效果 ≥ IOPaint」验收红线；效果优先前提下尽量提速。

## 2. 产品目标
**一句话**：在不破坏现有「下载→框选→批量去水印→预览→导出」流水线的前提下，把引擎从「ONNX 512 画布」换成「IOPaint 同源 big-lama PyTorch 子进程」，使消除效果达到并稳定不低于 IOPaint。

**可衡量目标**：
1. **效果下限（P0 红线）**：同一批测试图、相同 mask，新版输出逐一对比 IOPaint 输出，**无任何一张显著劣于 IOPaint**（通过率 100%）。
2. **速度**：CPU 下 1080p 单框单张平均 ≤ 2s，批量 100 张无崩溃/内存泄漏；效果优先，速度次优。
3. **功能回归**：社媒下载、本地文件夹、多框选、批量/勾选、取消、预览、zip 导出、CLI 全部 100% 回归通过。
4. **交付可用性**：用户零配置单 exe（或明确目录）分发；AI 引擎启动/预热过程对用户可见（不黑屏假死）。

## 3. 用户故事
- 作为社媒运营用户，我希望去水印后边缘自然、纹理不糊、无拼接缝，可直接用导出的图。
- 作为高频批量用户，我希望几十上百张图效果稳定一致且速度不显著变慢。
- 作为效果敏感用户，我希望新版至少和 IOPaint 一致（最好更好），无需在工具间来回切换。

## 4. 需求池

### P0（必须，验收红线）
- **P0-1 效果下限硬约束**：以原 IOPaint（big-lama 默认）为下限，只能等于或优于，绝不能更差。任一测试图显著退化即判失败。
- **P0-2 架构切换**：PyInstaller 打包独立子进程 exe，Go 主程序调用；用户零配置；接受总体积 +300~500MB。替换 `internal/inpaint/model.go` 与 `pipeline.go` 的 512 画布路径，弃用 `resources/lama_fp32.onnx`。
- **P0-3 模型权重对齐**：big-lama 原版 PyTorch 权重，对齐 IOPaint 默认模型；禁止 ONNX 转换版。
- **P0-4 分辨率/缩放策略对齐**：废除 512 固定画布；对齐 IOPaint 原生分辨率 + pad mod 8（big-lama 全卷积）。保留「mask bbox 裁剪 + margin」局部修复思路，但裁剪块内不再强制缩 512。
- **P0-5 mask/图像预处理对齐**：归一化、mask 二值化、pad 语义对齐 IOPaint `norm_img` / `pad_mod=8` / `mask=(mask>0)*1`。
- **P0-6 功能与接口兼容**：`app.go` 的 StartBatch/CancelBatch/进度事件、TaskParams、CLI、多框、取消、输出清理、zip 导出保持不变，仅引擎实现层替换。

### P1（应该）
- **P1-1 速度优化**（效果不劣化为前提）：torch 线程调优、`no_grad()`、单进程常驻、mask bbox 裁剪减少计算、batch 合并；须过 P0-1 下限验证。
- **P1-2 子进程健壮性**：Go↔Python 通信协议；启动/推理超时、崩溃、无响应 kill；取消强制终止回收；单张失败不中断。
- **P1-3 UI 引擎状态提示**：启动中/预热中/就绪/失败状态可见。
- **P1-4 首启动解压/加载耗时提示**：PyInstaller 内嵌 torch 数百 MB，首次解压/加载可达数秒~数十秒，需明确进度文案。

### P2（可选/增强）
- **P2-1 可选 GPU/CUDA**（默认 CPU 零配置）。
- **P2-2 高清 vs 快速模式切换**（默认对齐 IOPaint）。
- **P2-3 打包形态优化**（--onefile vs --onedir 权衡）。

### 效果对齐关键点
1. 权重同源：big-lama 原权重，禁止 ONNX 转换版。
2. 缩放策略同源：原生分辨率 + pad mod 8，废除 512 画布。
3. 预处理同源：`norm_img` + mask 二值化 + pad 语义，逐项对齐 IOPaint `iopaint/model/lama.py` 的 `forward()` 与 HD 策略。
4. 后处理同源：`clip(cur_res*255,0,255)` → RGB/BGR 转换，贴回时非 mask 区原像素不变（与现有 `pasteInto` 语义一致）。

## 5. UI/交互变更
- **新增「AI 引擎状态」提示**：去水印按钮旁显示「启动中…/预热中…/就绪/启动失败」。
- **首启动加载等待态**：首次启动与模型加载期间禁用去水印按钮并显示进度，加载完自动解除。
- 无新增用户可配置项；现有 dilate/margin 参数保持不变；其余界面无变更。

## 6. 验收标准
### 6.1 效果下限（P0，红线）
- 测试集：固定 ≥20 张，覆盖文字水印/图标 logo/纯色/复杂纹理/≤512 小图/>1080 大图/长图竖图。
- 对照：相同 mask，分别跑 ① IOPaint（big-lama、CPU、默认 HD 策略）② 新版子进程；并排对比。
- 判定：人眼 A/B（最终裁决）+ 客观指标参考（PSNR/SSIM 以 IOPaint 输出为参照，非唯一标准）。任一单张显著纹理糊化/伪影/色偏/拼接缝/边缘残留 → 不通过。

### 6.2 速度指标
- CPU：1080p 单框单张平均 ≤ 2s；批量 100 张无崩溃/内存增长/子进程残留；任何提速不得牺牲 6.1。

### 6.3 功能回归范围
下载（微信/小红书/抖音+Cookie）、本地文件夹、多框选（绝对/比例/dilate/margin）、批量/勾选、取消、预览、zip 导出、CLI（--url/--input/--relative-boxes/--dilate/--margin/--mask/--recursive/--zip）。

## 7. 待确认问题（已由主理人拍板，见「主理人决策纪要」）
1. 对齐基准（HD 策略 ORIGINAL vs CROP）
2. 权重来源（Sanster/models big-lama.pt vs 自导出）
3. 设备范围（仅 CPU？半精度？）
4. mask 参数口径（dilate/margin 保留与验收对比口径）
5. 子进程通信与生命周期（常驻 vs 每图拉起）
6. 效果量化阈值（PSNR/SSIM）
7. 体积与分发（--onefile vs --onedir）
8. 取消与超时

---

# 主理人决策纪要（齐活林，2026-09-08）

针对 PRD 第 7 节待确认问题，主理人拍板如下（架构师按此设计，无需再问）：

1. **对齐基准 → ORIGINAL**：对齐 IOPaint 默认 HD 策略（整图原生分辨率推理 + pad mod 8）。理由：用户要求「以 IOPaint 为下限」，最稳做法是复刻 IOPaint 默认行为；CROP 虽快但可能因上下文减少而不达下限。
2. **权重来源 → Sanster/models 的 big-lama.pt**：IOPaint 官方默认同源权重（TorchScript JIT，Apache-2.0），此前项目已成功下载使用过（`D:\clear_mask\lama` 曾用）。禁止 ONNX。
3. **设备范围 → 仅 CPU + fp32**：保持零配置；效果优先，禁用可能在 CPU 上降精度的半精度。
4. **mask 口径 → 验收用「相同最终 mask」对比**：dilate/margin 保留为 App 现有参数（默认 dilate=12/margin=64）；验收对比时两版使用完全相同 mask 保证公平。
5. **子进程生命周期 → 单进程常驻**：模型加载一次、批量复用；由架构师设计通信协议。
6. **量化阈值 → 交 QA 定标准**。
7. **体积/分发 → 交架构师权衡 --onefile/--onedir**，倾向兼顾启动速度。
8. **取消/超时 → 交架构师设计**：倾向「推理中取消立即 kill 子进程」+ 合理超时。
