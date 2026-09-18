# v1.1.7 发布验证报告 —— 修复抖音 BGM 下载 404

- 发布日期：2026-09-15
- 版本号：1.1.7（来源 `wails.json` → `productVersion`）
- 主题：废弃 `ies-music/{music.id}.mp3` 拼接逻辑，改用 `music/detail` 接口取源

---

## 一、故障现象

部分电脑下载抖音 BGM 时报错：

```
BGM 下载失败（已尝试 1 个地址）：HTTP 404 音频下载失败:
https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/7679463410022550307.mp3
```

同时另一批电脑同一链接下载正常。用户已确认：**Cookie 已配置**，网络为**公司网**。

## 二、根因（已通过实时探测确认）

**`ies-music/{music.id}.mp3` 这一构造方式在结构上就是错的**——不是偶发 404，而是永远 404。

| # | 探测对象 | 结果 |
|---|---|---|
| 1 | `sf3-cdn-tos.../obj/ies-music/7679463410022550307.mp3` | `{"Success":-1,"error":{"code":4008,"message":"not found"}}` |
| 2 | 换 `sf1`/`sf11` 节点、去掉 `.mp3` | 同样 `not found` |
| 3 | `GET /aweme/v1/web/music/detail/?music_id=7679463410022550307&aid=6383` | **无需签名**返回完整 `music_info` |
| 4 | 接口返回的 `play_url.url_list` | `.../obj/ies-music-hj/7679463571897502513.mp3` |
| 5 | 上一步 URL 实际拉取 | `Content-Type: audio/mpeg`，真实可播 |

**关键差异**：对象存储的文件名 ID `7679463571897502513` ≠ `music.id` `7679463410022550307`。

对象存储里的文件名是**另一个资源 ID**，与 `music.id` 无映射关系，无法自行推导。

### 为什么"有的电脑能下"

那些机器 SSR 页面解析成功，拿到了页面下发的真实地址（走 ①② 级），压根没用到这条错误构造。
失败机器上 `_ROUTER_DATA` 解析降级、SSR 段为空，候选链只剩这条必然 404 的构造地址 —— 日志里
`已尝试1个地址` 正是这个证据。

## 三、修复内容

### 3.1 删除（`internal/downloader/douyin.go`）

- `const iesMusicHost`
- `func iesMusicURL(music gjson.Result) string`
- 调用点 `add(iesMusicURL(music))`

### 3.2 新增

- `func musicID(music gjson.Result) string` —— 提取数字 ID（id 优先，mid 兜底）
- `func douyinMusicDetailURL(ctx, musicID) []string` —— 走 `music/detail` 接口取真实地址；
  非数字 ID 直接返回 `nil`（不发请求）；`music_info` 为 null 时返回 `nil`

### 3.3 候选链重排

```
① SSR music.play_url.url_list      ← 官方下发，最优
② SSR music.play_url.uri           ← 仅当本身是 http(s) 时收录
③ music/detail 接口（music.id）    ← 新增，免签名主力兜底（仅 ①② 皆空时调用，省一次往返）
④ aweme/detail 接口                ← 末位保留（需 a_bogus）
```

### 3.4 错误提示分流（`internal/downloader/router.go`）

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 候选链为空 | `该帖子没有可下载的 BGM` | `未解析到 BGM 音频地址（可能该帖无 BGM，或页面结构变更导致解析降级）` |
| 候选链非空但全失败 | `BGM 下载失败（已尝试 N 个地址）：<裸错误>` | `BGM 下载失败（已尝试 N 个地址，最后错误：<错误>）` |

目的：让用户能区分「无 BGM」与「解析降级」，不再把解析问题误报成下载失败。

## 四、验证结果

### 4.1 单元测试

```
go vet ./internal/downloader/...     → 通过
go test ./...                        → 全部通过
  ok  lama-watermark-eraser                     1.818s
  ok  lama-watermark-eraser/internal/cli        0.469s
  ok  lama-watermark-eraser/internal/downloader 1.788s
  ok  lama-watermark-eraser/internal/inpaint    0.470s
```

测试变更：
- 删除 `TestIesMusicURL`（被测函数已移除）
- 新增 `TestMusicID`（id/mid 选择与非法值守卫）
- 新增 `TestDouyinMusicDetailURLNoRequest`（非数字 ID 不发网络请求）
- 更新 `TestDouyinMusicCandidates` / `TestDouyinMusicCandidatesRealWorld`（去掉 ies-music 构造断言）
- 更新 `app_test.go` 的 `TestGetBGMAudioNoSource` 断言文案

### 4.2 真实网络验证（`music/detail` 兜底）

注入 SSR 降级场景（`url_list` 空、`uri` 空），`music.id = 7679463410022550307`（用户上报的失败 ID）：

```
name="" candidates=2
  [0] https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/7679463571897502513.mp3
  [1] https://lf9-music-east.douyinstatic.com/obj/ies-music-hj/7679463571897502513.mp3
PASS: 可用地址 = .../ies-music-hj/7679463571897502513.mp3
PASS: 下载成功 bgm.mp3（715672 字节）
```

**这正是修复前必报 404 的曲目。**

### 4.3 端到端（真实链接 + 真实 Cookie）

输入 `https://v.douyin.com/tLyloNrTWmA/`，注入本机已保存的抖音 Cookie（5791 字符）：

```
[OK] 作品解析成功: platform=douyin id=7397543688773094656
     图片 9 张
     BGM 名称="長沙-剪辑版一先贻jUju"
     BGM 候选 2 条:
       [0] https://sf11-cdn-tos.douyinstatic.com/obj/tos-cn-ve-2774/ocO5kAYT3Pn2gtCjQXxtBQFvgDeeZNwDD3ySbz
       [1] https://sf6-cdn-tos.douyinstatic.com/obj/tos-cn-ve-2774/ocO5kAYT3Pn2gtCjQXxtBQFvgDeeZNwDD3ySbz
[probe] OK 可用地址: sf11 节点
[PASS] BGM 下载成功: 長沙-剪辑版一先贻jUju.m4a (520094 字节)
```

### 4.4 GUI 冒烟

- 窗口正常出现（`hwnd=0x630e24`，1064×721），前端渲染非空白（214 种颜色）
- 底部版本号显示 **`v1.1.7`**（`-ldflags` 注入生效，与安装包同源）

## 五、产物

| 产物 | 大小 | SHA256 |
|---|---|---|
| `build/bin/社媒图文水印抹除工具.exe` | 14,207,488 B | `a7d7c55da44b8f92cd9c142380fded87b3e1ca7108ba39071015b414ddb3b112` |
| `build/installer/社媒图文水印抹除工具_Setup_1.1.7.exe` | 314,911,076 B | `48994b81e98a73b70b11141ac6aa9ea2e9d59cf29f3c62998fd4e36dc4147db8` |

构建命令：

```bash
wails.exe build -platform windows/amd64 -webview2 embed \
  -ldflags "-s -w -X main.appVersion=1.1.7"
"/c/Users/Administrator/AppData/Local/Programs/Inno Setup 6/ISCC.exe" \
  "/DMyAppVersion=1.1.7" "packaging/installer.iss"
```

ISCC 编译耗时 175.1 秒。

## 六、已知限制

| 项 | 说明 |
|---|---|
| 接口稳定性 | `music/detail` 免签名是当前实测结论，抖音可能随时加签名校验；已保留 ④ aweme/detail 作末位兜底 |
| 公司网风控 | 本机验证成功不代表公司网下必然成功；失败机器建议回归验证，并确认其是否配置了 Cookie |
| 无法本机复现原故障 | 本机未复现原 404 场景（本机 SSR 解析正常），采用**注入降级场景**方式验证兜底链路 |

## 七、版本号同步位置（5 处）

1. `wails.json` ×2（`productVersion` + `info.productVersion`）
2. `packaging/installer.iss`（默认值 + 注释 + 产物名）
3. `packaging/build_windows.ps1`（注释）
4. `README.md`（当前版本 + 安装包名 + ISCC 命令 + `-Version` 示例）
5. `frontend/src/changelog.js`（首项 `version` 必须与 `productVersion` 一致）
