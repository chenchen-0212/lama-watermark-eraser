# v1.1.5 需求实现与验证报告 —— 抖音 BGM 取源容错

## 一、需求

优化抖音 BGM 下载。此前抖音 BGM 的失败绝大多数发生在「取链」阶段而非下载阶段：
`douyinMusic` 主路径常空转，`douyinDetailMusicURL` 裸调未签名接口必被风控拦截，
结果表现为「该帖子没有 BGM」——用户无法区分「确实没音乐」与「取链失败」。

## 二、实现（P0 四项）

### 1. BGM 取源由单地址改为候选链

`internal/downloader/types.go` 新增 `AudioCandidates []string` 与 `SetAudio()` 统一设置：
去空、去重、保序，首项回写 `AudioURL` 以保持既有前端契约；全部为空时**清空** `AudioURL`
（否则前端 `hasBGM` 会误判为有 BGM 而显示无效按钮）。

`internal/downloader/douyin.go` 由 `douyinMusic` 重构为 `douyinMusicCandidates`，
按实测可靠性排序：

1. SSR 页面 `play_url.url_list` 中的 http(s) 地址（官方下发，实测可用）；
2. `play_url.uri` —— 仅当它本身是 http(s) 地址时收录；
3. 由 `music.id` 构造的 `ies-music` 直链（确定性但实测存在 404，作兜底）；
4. detail API 结果（需 `a_bogus`/`msToken`，最后手段）。

`internal/downloader/router.go` 新增：

- `PickAudioSource` —— 按候选链探测，只读 16 字节（配 `Range` 请求），命中即返回；
- `DownloadAudioWithFallback` —— 按候选链下载，返回首个成功结果；
- `probeAudio` / `usableCandidates`。

### 2. 响应体魔数校验（杜绝坏文件）

`httpclient.go` 新增 `audioMagic` 表与 `looksLikeAudio()`。`SaveAudio` 拆分出
`saveAudioOnce`，落盘前读取头部 16 字节校验容器魔数（ID3 / MPEG 帧同步 / `ftyp` /
`OggS` / `fLaC` / `RIFF` / ADTS）。不匹配即视为风控页或错误 JSON，删除 `.part` 并
返回 `errNotAudio` 明确报错。

**修复的实际缺陷**：原实现只判 `resp.StatusCode == 200` 就落盘，抖音 CDN 限流时返回的
HTML 风控页会被原样存成 `.mp3`，用户得到一个无法播放的文件且无任何提示——属静默数据损坏。

### 3. 补齐音乐 CDN 域

`platformFromURL` 与 `audioRequestMeta` 补入 `douyinstatic` / `snssdk` / `amemv` /
`muscdn`，保证 Referer 与 Cookie 正确注入。

### 4. 试听缓存键稳定性

`app.go` 新增 `audioCacheKey()`：由候选链生成稳定键（去查询串、去主机名、去重后排序）。

**修复的实际缺陷**：原缓存键是原始 URL 的 SHA1，而 CDN 地址带 `x-expires`/`sign` 时效
参数，同一曲目每次解析出的 URL 都不同 → 缓存**永不命中**，且 `bgm-cache` 目录无限增长。

### 5. 附带：单次重试

`SaveAudio` 增加最多 3 次尝试（`errNotAudio` 类错误不重试，重试无意义）。

## 三、接口变更（破坏性）

三个 Wails 绑定方法签名由 `string` 改为 `[]string`：

| 方法 | 旧 | 新 |
|---|---|---|
| `DownloadBGM` | `(audioURL, dir, filename)` | `(candidates, dir, filename)` |
| `DownloadBGMStandalone` | `(audioURL, filename)` | `(candidates, filename)` |
| `GetBGMAudio` | `(audioURL, localPath)` | `(candidates, localPath)` |

`frontend/wailsjs/` 已用 `wails generate module` 重新生成（确认三处均为 `Array<string>`）。
前端 `store.js` 新增导出 `audioCandidates(post)`：优先用 `post.audioCandidates`，
老数据只有 `audioUrl` 时退回单地址，**向后兼容**。

## 四、真实链接验证（2026-09-15）

测试链接：`https://v.douyin.com/tLyloNrTWmA/`
→ 规范化为 `https://www.douyin.com/note/7397543688773094656`

本地 Cookie 已加载（5791 字节，含 `ttwid` / `sessionid`）。

### 候选链实测结果

| 候选 | 地址 | 结果 |
|---|---|---|
| ① | `sf3-cdn-tos.douyinstatic.com/obj/ies-music/6634113815568976654.mp3` | **HTTP 404 不可用** |
| ② | `sf11-cdn-tos.douyinstatic.com/obj/tos-cn-ve-2774/ocO5kAYT3Pn2gtCjQXxtBQFvgDeeZNwDD3ySbz` | **可用，520 094 字节**，`audio/mp4` |

下载结果：`長沙-剪辑版一先贻jUju.m4a`（扩展名由 Content-Type 判为 `.m4a`），
头部 `00 00 00 1c 66 74 79 70 4d 34 41` = `ftypM4A`，魔数校验通过。

### 新旧逻辑对照（同一链接、同一次会话）

| | 取链结果 |
|---|---|
| 旧逻辑（只取 SSR 第一条） | **失败**（首条恰好是 404 的那条） |
| 新逻辑（候选链） | **成功**（自动落到第二条） |

**结论：该链接在旧实现下必定失败，本次改动使其可用。** 这是候选链机制的直接收益。

### 实测推翻的两处假设（重要）

1. **`play_url.uri` 不一定只是资源标识。** 此前判断它是 `v0200fg10000...` 形态的
   资源标识而非 URL；实测该作品的 `uri` 是 **`ies-music` 形态的完整 mp3 直链**。
   代码已修正为「是 http(s) 就收录」，并补 `TestUriAcceptedWhenHTTP` 固化。

2. **`ies-music/{music.id}.mp3` 直链当前已失效。** 该构造方式来自公开资料，
   上一轮标注为「未经实测」；本次实测对应该曲目返回 **404**（带/不带 Referer、
   带/不带 Cookie 均为 404，已排除防盗链因素）。因此它**不能作为依赖**，
   代码注释已改为如实描述，并确认它在候选链中排在页面真实地址之后——排序决策正确，
   不会破坏主路径。

### 另一发现

`uri` 与 `music.id` 构造的直链在实际数据中**完全相同**，会让 failover 白跑一次探测。
已在 `douyinMusicCandidates` 内做本地去重（保留首次出现）。

### 防盗链强度

实测该音频地址**不校验 Referer 与 Cookie**（带/不带均返回 200）。保留 Referer 注入
仍属稳妥做法（其他曲目/CDN 节点可能不同），但说明音频 CDN 的门槛低于预期。

## 五、自动化测试

新增 / 更新用例（`go test . ./internal/...` 全绿）：

| 用例 | 覆盖 |
|---|---|
| `TestDouyinMusicCandidates` | 候选链顺序、uri 非 URL 不收录、无 music 节点不 panic |
| `TestDouyinMusicCandidatesRealWorld` | **真实数据回归**：uri 为 mp3 直链、去重后 2 条、无重复项 |
| `TestUriAcceptedWhenHTTP` | http 形态 uri 必收录 / 资源标识形态不收录 |
| `TestIesMusicURL` | id/mid 兜底、非纯数字拒绝 |
| `TestSetAudioDedup` | 去空去重保序、全空清空 `AudioURL` |
| `TestLooksLikeAudio` | 11 组魔数（含风控 HTML / 错误 JSON / 空响应） |
| `TestAudioCacheKey` | 同曲不同签名同键、不同曲不同键 |
| `TestGetBGMAudioFallback` | 首个候选风控页 → 自动落第二个 |
| `TestGetBGMAudioRejectsHTML` | 全部候选非音频 → **必须报错，不得返回假音频** |

## 六、产物

| 产物 | 大小 | SHA256 |
|---|---|---|
| 主程序 `build/bin/社媒图文水印抹除工具.exe` | 14,196,736 B (13.5 MiB) | `14117e37466d6e27009803cdf995dd4baa4b49758880db690880d441c9a846ec` |
| 安装器 `build/installer/社媒图文水印抹除工具_Setup_1.1.5.exe` | 314,909,665 B (300.3 MiB) | `612757ee5ff576a8e88cb42e34300b9acd373c97ef34b1ce3e1390bca3091b4c` |

ISCC 编译退出码 `0`，耗时 185.3 秒。

产物特征串校验（防止「改了源码但产物是旧的」）——以下 10 项**全部命中**：

| 特征串 | 含义 |
|---|---|
| `ies-music` | 兜底直链构造 |
| `snssdk` / `amemv` / `muscdn` / `douyinstatic` | 新增音乐 CDN 域识别 |
| `audioCandidates` | 候选链字段 |
| `MP3(ID3)` / `M4A/MP4` | 魔数校验表 |
| `不是音频` | `errNotAudio` 报错文案 |
| `该帖子没有可下载的 BGM` | 空候选收敛文案 |

版本号一致性校验：`wails.json` / `packaging/installer.iss` / `packaging/build_windows.ps1` /
`README.md` 均为 `1.1.5`，**无残留 `1.1.4`**。

回归测试：`go test ./internal/downloader/... . -count=1` → `ok` ×2。

## 七、未完成 / 未验证

- **`ies-music` 直链可用率**：单样本实测为 404。它对其他曲目是否有效、是否存在
  时效性或区域性差异，**未做统计**。它在候选链中仅作兜底，不承担主路径。
- **多链接样本量**：仅 1 条真实链接做过完整对照。建议再取若干条图文（带乐 / 无乐 /
  原声各若干）做批量统计校准；批量脚本为一次性验证用途，已在收尾时删除，需要时按
  本报告「候选链实测结果」一节的探测方式重写即可。
- **P1 未实施**：Range 续传落地（当前仅重试，未真正续传）、`bgm-cache` 的 LRU 与容量
  上限清理、下载进度事件。
- **P2 未实施**：纯 Go `a_bogus` 签名、本地 Range 流式试听（替代 base64 与 24 MiB 上限）、
  ID3 元数据嵌入。
- **macOS 包未构建**：本次仅出 Windows 产物。
