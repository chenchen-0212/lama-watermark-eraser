// 版本更新明细数据源。
//
// 约定：数组首项即「当前版本」，界面上的「更新明细」入口默认展开这一项。
// 每次发版在此处新增一条记录（不要删旧记录，历史版本明细对用户有价值），
// 并保证首项的 version 与 wails.json 的 productVersion 一致。
//
// 字段：
//   version  版本号（与 wails.json 的 productVersion 一致，不含 v 前缀）
//   date     发布日期 YYYY-MM-DD
//   title    该版本的一句话主题
//   items    更新条目，每条 { kind, text }；kind 决定标记样式：
//            'new' 新增 | 'fix' 修复 | 'opt' 优化 | 'change' 变更
//
// kind 到中文标签的映射见 KIND_LABEL；新增 kind 时需同步补充，
// 否则会回退为原样输出（见 kindLabel 的兜底逻辑）。

export const KIND_LABEL = {
  new: '新增',
  fix: '修复',
  opt: '优化',
  change: '变更',
}

export const changelog = [
  {
    version: '1.1.8',
    date: '2026-09-18',
    title: '任务队列：批量下载与去水印后台排队',
    items: [
      {
        kind: 'new',
        text: '新增任务队列：首页支持批量粘贴链接后台依次下载素材；框选好的去水印任务可「加入队列」串行执行，无需等上一轮完成（排队时显示前方任务数）',
      },
      {
        kind: 'new',
        text: '顶栏新增「任务队列」面板：下载与去水印左右分栏独立管理，支持失败自动重试、手动重试、取消、移除、分类清空；任务列表本机持久化，重启后未完成任务自动续跑',
      },
      {
        kind: 'new',
        text: '队列任务直达操作：已完成的下载任务可一键「去框选」，去水印任务可「查看结果」直达导出页',
      },
      {
        kind: 'fix',
        text: '修复「加入队列」点击报错（函数引用缺失）导致去水印任务无法入队',
      },
      {
        kind: 'fix',
        text: '修复同一帖子重复下载后框选页出现重复图片：下载前清理旧图、解析层链接去重、按内容哈希去重三重防护，且同一链接不允许重复入队',
      },
      {
        kind: 'fix',
        text: '修复后台任务完成后页面停留在「处理中」、导出按钮被禁用的问题；任务失败现在明确报错，不再静默退回',
      },
      {
        kind: 'fix',
        text: '修复提示条遮挡顶栏按钮：提示改为底部居中显示，不再压住「任务队列」等入口',
      },
      {
        kind: 'opt',
        text: '抖音 BGM 取源加固：music/detail 兜底地址始终并入候选链尾（应对页面下发地址整链时效性失效），失败自动重试一次并给出风控处理指引',
      },
      {
        kind: 'opt',
        text: '抖音短链解析加固：桌面 UA 被重定向到首页（丢作品 ID）时自动换移动 UA 重试',
      },
      {
        kind: 'change',
        text: '队列面板空态重设计：居中图标与引导说明，不再出现布局错乱',
      },
    ],
  },
  {
    version: '1.1.7',
    date: '2026-09-15',
    title: '修复抖音 BGM 下载 404',
    items: [
      {
        kind: 'fix',
        text: '修复部分抖音作品 BGM 下载报「HTTP 404 音频下载失败」：废弃由 music.id 自行拼接对象存储直链的旧逻辑（该路径对绝大多数曲目必然 404），改为通过 music/detail 接口获取真实音频地址',
      },
      {
        kind: 'opt',
        text: 'BGM 取源候选链重排：页面下发地址 → 页面 uri → music/detail 接口（免签名，主力兜底）→ aweme/detail 接口，任一级可用即停止，显著提升解析降级场景下的成功率',
      },
      {
        kind: 'opt',
        text: '无 BGM 与「地址解析失败」的错误提示分开，避免把「未解析到地址」误报成下载失败',
      },
    ],
  },
  {
    version: '1.1.6',
    date: '2026-09-15',
    title: '版本信息可见',
    items: [
      {
        kind: 'new',
        text: '界面底部作者签名处标注当前版本号，与安装包版本同源（构建时注入，不会出现「界面版本」与「安装包版本」不一致）',
      },
      {
        kind: 'new',
        text: '顶栏「注意事项」旁新增「更新明细」入口，弹窗内按版本列出更新内容',
      },
      {
        kind: 'opt',
        text: '版本号改为构建时经 -ldflags 注入，单一来源仍是 wails.json 的 productVersion',
      },
    ],
  },
  {
    version: '1.1.5',
    date: '2026-09-15',
    title: '抖音 BGM 取源容错',
    items: [
      {
        kind: 'opt',
        text: '抖音 BGM 取源由单地址改为候选链：页面地址 → 由 music.id 构造的直链 → detail 接口，逐条尝试，单条 CDN 失效不再导致整体失败',
      },
      {
        kind: 'fix',
        text: '落盘前校验响应体魔数，风控页 / 错误 JSON 会被拒绝，不再存成无法播放的假音频（此前的静默数据损坏）',
      },
      {
        kind: 'fix',
        text: 'BGM 试听缓存键改用候选链稳定标识，此前 CDN 地址带时效签名导致缓存永不命中、缓存目录无限增长',
      },
      {
        kind: 'new',
        text: '补齐音乐 CDN 域识别（douyinstatic / snssdk / amemv / muscdn）',
      },
    ],
  },
  {
    version: '1.1.4',
    date: '2026-09-14',
    title: 'BGM 试听',
    items: [
      {
        kind: 'new',
        text: '第三步新增 BGM 试听：播放/暂停、点击进度条跳转、时间显示；显示条件与「下载BGM」按钮完全一致',
      },
      {
        kind: 'fix',
        text: 'Windows 一键打包脚本读取 wails.json 未指定 UTF-8，PowerShell 5.1 按 ANSI 解码中文导致 JSON 解析失败',
      },
    ],
  },
]

// kindLabel 把 kind 映射为中文标签；未知 kind 原样返回，避免界面出现空白标记。
export function kindLabel(kind) {
  return KIND_LABEL[kind] || kind || ''
}

// latestVersion 返回更新明细中的最新版本号（空数组时返回空串）。
export function latestVersion() {
  return changelog.length ? changelog[0].version : ''
}
