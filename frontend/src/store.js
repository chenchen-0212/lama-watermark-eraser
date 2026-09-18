import { reactive } from 'vue'
import {
  GetThumb,
  DownloadBGM,
  DownloadBGMStandalone,
  StartBatch,
  ZipDirectory,
  PickSaveFile,
  CopyFile,
  PrepareSubset,
  PickDirectory,
  ListImages,
  GetEngineStatus,
  ExportSourceZip,
  AddQueueTasks,
  EnqueueInpaint,
  ListQueueTasks,
  RetryQueueTask,
  CancelQueueTask,
  RemoveQueueTask,
  ClearFinishedTasks,
} from '../wailsjs/go/main/App'
import { EventsOn } from '../wailsjs/runtime/runtime'

export const store = reactive({
  // 流水线状态机：idle → downloading → downloaded → inpainting → inpainted
  stage: 'idle',
  inputUrl: '',
  post: null, // {platform,title,author,dir,files,count,audioUrl,audioName}
  bgmSaved: '', // 已保存的 BGM 本地路径（空=未下载）
  bgmMode: 'standalone', // standalone=另存为独立文件 | bundle=存入源图目录（随 zip 打包）
  sourceThumbs: [], // [{path,thumb,name}]
  cleanedThumbs: [],
  cleanedDir: '',
  selected: [], // 勾选的源图路径（空=全部）
  previewPath: '', // 当前预览/操作的图片路径（空=默认第一张）
  boxes: [], // [[x1,y1,x2,y2], ...] 原图像素坐标（多个水印区域）
  ratios: [], // [[x1,y1,x2,y2], ...] 0..1 比例
  mode: 'relative', // absolute | relative（默认按比例适配，兼容不同尺寸图集）
  strategy: 'auto', // 处理策略: auto(智能)|original(整图)|crop(快速裁剪)
  engineStatus: 'idle', // AI 引擎状态机: idle | starting | ready | error
  engineMessage: '',
  dilate: 12,
  logs: [],
  progress: { index: 0, total: 0, name: '', status: '' },
  running: false,
  canceling: false,
  toast: '', // 非空则显示
  toastKind: 'info',
  viewer: { open: false, name: '', src: '', loading: false },
  viewerList: [], // 放大浮层当前的图片列表（用于左右切换）
  viewerIndex: -1, // 当前查看的图片在列表中的下标（-1=不在列表中）

  // ---------------- 批处理任务队列 ----------------
  queue: [], // 队列任务快照（由 queue:updated 全量同步）
})

let toastTimer = null
export function showToast(msg, kind = 'danger') {
  store.toast = msg
  store.toastKind = kind
  clearTimeout(toastTimer)
  toastTimer = setTimeout(() => (store.toast = ''), 4000)
}

export function log(line, cls = '') {
  store.logs.push({ line: `[${timeStr()}] ${line}`, cls })
  if (store.logs.length > 300) store.logs.shift()
}

function timeStr() {
  const d = new Date()
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}`
}

// ---------------- 流水线动作 ----------------
// 自动入队模式（方案 v1.2）：下载与去水印点击后立即入队并留在当前页，
// 不切换到等待页；进度与终态在「任务队列」面板可视，完成后 toast 提醒。

export async function startDownload() {
  const url = store.inputUrl.trim()
  if (!url) return
  log('提交下载: ' + url)
  try {
    // 复用批量入队（内部按空白拆分，单/多链接通用）；直连下载已移除（方案 O7）
    const report = await AddQueueTasks([url])
    const okN = report?.tasks?.length || 0
    if (okN > 0) {
      showToast('已加入队列，可在任务队列面板查看', 'ok')
      log(`已加入下载队列（${okN} 个任务）`, 'ok')
    }
    const fails = report?.failures || []
    for (const f of fails) log(`链接未入队: ${f.input} — ${f.reason}`, 'fail')
    if (!okN && fails.length) showToast('未入队：' + fails[0].reason)
  } catch (e) {
    log('入队失败: ' + e, 'fail')
    showToast(String(e))
  }
}

// 本地文件夹模式：选择目录 → 列出图片 → 进入框选去水印流程
export async function loadLocalFolder() {
  let dir
  try {
    dir = await PickDirectory()
  } catch (e) {
    return // 用户取消或对话框失败，保持原状
  }
  if (!dir) return
  log('选择本地文件夹: ' + dir)
  try {
    const files = await ListImages(dir)
    if (!files.length) {
      showToast('该文件夹内没有可处理的图片')
      return
    }
    await loadMaterialIntoStore('本地文件夹', dir, dir, files)
    log(`已加载 ${files.length} 张本地图片`, 'ok')
  } catch (e) {
    log('加载失败: ' + e, 'fail')
    showToast(String(e))
  }
}

export async function startBatch(scopeAll = true) {
  if (!store.boxes.length) {
    showToast('请先在预览图上拖拽框选水印区域')
    return
  }
  if (store.engineStatus === 'error') {
    showToast('AI 引擎未就绪：' + (store.engineMessage || '启动失败'), 'danger')
    return
  }
  const relative = store.mode === 'relative'
  const boxes = relative ? store.ratios : store.boxes
  let inDir = store.post.dir
  try {
    if (!scopeAll && store.selected.length > 0) {
      inDir = await PrepareSubset(store.selected)
      log(`仅处理勾选的 ${store.selected.length} 张`)
    }
    const outDir = inDir + '_去水印'
    // 点击即入队（方案 O2）：不切页、不置 running，留在框选页可继续操作；
    // 引擎 starting 态允许入队（排队即等待），error 态已在上方拦截。
    // title 固化帖子标题，队列面板展示用（而非文件路径）。
    const task = await StartBatch(inDir, outDir, {
      boxes, relative, dilate: store.dilate, margin: 64, maskPath: '', strategy: store.strategy,
    }, store.post?.title || store.post?.dir || '')
    log(`去水印任务已入队: ${task?.title || inDir}`, 'ok')
    showToast('已加入队列，可在任务队列面板查看', 'ok')
  } catch (e) {
    log('入队失败: ' + e, 'fail')
    showToast(String(e))
  }
}

// ---------------- 批处理任务队列（面板辅助动作） ----------------

// 批量解析并加入队列：输入按空白/逗号/分号（含全角）拆分为多条链接。
// 已无独立 UI 入口（批量 textarea 已隐藏），其拆分逻辑由 startDownload 复用；
// 保留本函数供「任务队列」面板未来扩展与开关回滚。
export async function addQueueTasks(rawInput) {
  const urls = String(rawInput || '')
    .split(/[\s,;，；]+/)
    .map((s) => s.trim())
    .filter(Boolean)
  if (!urls.length) {
    showToast('请先粘贴链接（可每行一条）', 'danger')
    return
  }
  try {
    const report = await AddQueueTasks(urls)
    const okN = report?.tasks?.length || 0
    const fails = report?.failures || []
    for (const f of fails) log(`链接未入队: ${f.input} — ${f.reason}`, 'fail')
    if (okN > 0) {
      log(`已加入队列 ${okN} 个下载任务` + (fails.length ? `，${fails.length} 条未入队` : ''), 'ok')
      showToast(`已加入队列 ${okN} 个任务${fails.length ? `（${fails.length} 条链接无效未入队）` : ''}`, 'ok')
    } else if (fails.length) {
      showToast('没有链接入队：' + fails[0].reason, 'danger')
    }
  } catch (e) {
    log('批量入队失败: ' + e, 'fail')
    showToast(String(e))
  }
}

// 固化当前框选参数，建去水印任务入队。
// 【已无 UI 入口】「+ 加入队列」按钮已随自动入队改造移除（startBatch 与其实现
// 完全同构）；保留本函数仅作开关回滚（SHOW_MANUAL_QUEUE_ENTRY）之用。
export async function enqueueInpaint(scopeAll = true) {
  if (!store.boxes.length) {
    showToast('请先在预览图上拖拽框选水印区域')
    return
  }
  const relative = store.mode === 'relative'
  const boxes = relative ? store.ratios : store.boxes
  let inDir = store.post.dir
  try {
    if (!scopeAll && store.selected.length > 0) {
      inDir = await PrepareSubset(store.selected)
    }
    const outDir = inDir + '_去水印'
    await EnqueueInpaint(inDir, outDir, {
      boxes, relative, dilate: store.dilate, margin: 64, maskPath: '', strategy: store.strategy,
    }, store.post?.title || store.post?.dir || '')
    log('已加入队列：' + (scopeAll && store.selected.length === 0 ? '全部图片' : `勾选的 ${store.selected.length} 张`), 'ok')
    showToast('去水印任务已加入队列', 'ok')
  } catch (e) {
    showToast(String(e))
  }
}

// 打开已完成去水印任务的结果页（队列面板「查看结果」入口）：
// 立即切到结果页，缩略图后台渐进加载（点「查看结果」不再卡顿），可直接导出 zip。
export async function openTaskResult(task) {
  if (!task?.resultDir) {
    showToast('该任务没有结果目录', 'danger')
    return
  }
  try {
    const files = await ListImages(task.resultDir)
    if (!files.length) {
      showToast('结果目录中没有可导出的图片（可能已被清理）', 'danger')
      return
    }
    store.cleanedDir = task.resultDir
    store.cleanedThumbs = files.map((p) => ({
      path: p, thumb: '', name: fileName(p), width: 0, height: 0, loading: true,
    }))
    store.running = false
    store.canceling = false
    store.stage = 'inpainted'
    log(`已打开任务结果: ${task.resultDir}（${files.length} 张），缩略图加载中…`)
    await fillThumbs(store.cleanedThumbs)
  } catch (e) {
    log('打开任务结果失败: ' + e, 'fail')
    showToast('打开结果失败: ' + e)
  }
}

// 打开队列面板前拉取一次全量快照（queue:updated 事件负责后续同步）
export async function refreshQueue() {
  try {
    store.queue = (await ListQueueTasks()) || []
  } catch {
    /* 忽略：面板保持现有快照 */
  }
}

export async function retryQueueTask(id) {
  try {
    await RetryQueueTask(id)
  } catch (e) {
    showToast('重试失败: ' + e)
  }
}

export async function cancelQueueTask(id) {
  try {
    await CancelQueueTask(id)
    showToast('已请求取消', 'info')
  } catch (e) {
    showToast('取消失败: ' + e)
  }
}

export async function removeQueueTask(id) {
  try {
    await RemoveQueueTask(id)
  } catch (e) {
    showToast('移除失败: ' + e)
  }
}

// 按类型清空终态任务（两类队列独立管理）：type 为空清全部
export async function clearFinishedQueue(type = '') {
  try {
    await ClearFinishedTasks(type)
    showToast('已清空已完成任务', 'ok')
  } catch (e) {
    showToast('清空失败: ' + e)
  }
}

// 载入已完成下载任务的素材，进入框选页（队列面板「去框选」）。
// 优先使用任务记录的图片清单（task.files，本次下载的确切文件、已按内容去重），
// 仅在清单缺失时回退列目录——避免把目录里的历史残留文件混入造成图片重复。
export async function loadTaskMaterial(task) {
  if (!task?.resultDir) {
    showToast('该任务没有可载入的素材目录', 'danger')
    return
  }
  try {
    let files = Array.isArray(task.files) ? task.files.filter(Boolean) : []
    if (!files.length) {
      files = await ListImages(task.resultDir)
    }
    if (!files.length) {
      showToast('该素材目录中没有可处理的图片', 'danger')
      return
    }
    await loadMaterialIntoStore(task.platform || '队列任务', task.postRef || task.resultDir, task.resultDir, files)
    log(`已载入队列素材: ${task.postRef || task.resultDir}（${files.length} 张）`, 'ok')
  } catch (e) {
    log('载入素材失败: ' + e, 'fail')
    showToast(String(e))
  }
}

// loadMaterialIntoStore 把一组本地图片装入框选流程（loadLocalFolder 与
// loadTaskMaterial 共用）。
// 性能关键：先铺占位条目并立即切页（点击秒进），缩略图后台逐张生成、
// 逐个替换——点「去框选」不再卡在加载上。
async function loadMaterialIntoStore(platform, title, dir, files) {
  store.post = {
    platform,
    id: '',
    title,
    author: '',
    dir,
    files,
    count: files.length,
    audioUrl: '',
    audioName: '',
  }
  store.bgmSaved = ''
  store.bgmMode = 'standalone'
  store.selected = []
  store.previewPath = ''
  store.sourceThumbs = files.map((p) => ({
    path: p, thumb: '', name: fileName(p), width: 0, height: 0, loading: true,
  }))
  store.stage = 'downloaded'
  await fillThumbs(store.sourceThumbs)
}

// fillThumbs 逐张生成缩略图并就地替换占位条目（渐进出现在网格中）。
async function fillThumbs(items) {
  for (const item of items) {
    try {
      const t = await GetThumb(item.path, 420)
      item.thumb = t.thumb
      item.width = t.width
      item.height = t.height
    } catch (e) {
      item.failed = true
      log(`缩略图生成失败: ${item.name}`, 'fail')
    }
    item.loading = false
  }
}

export async function exportZip() {
  try {
    const zipPath = await ZipDirectory(store.cleanedDir)
    const saveTo = await PickSaveFile(fileName(zipPath))
    if (saveTo) {
      await CopyFile(zipPath, saveTo)
      log('已保存: ' + saveTo, 'ok')
      showToast('zip 已保存', 'ok')
    }
  } catch (e) {
    log('导出失败: ' + e, 'fail')
    showToast(String(e))
  }
}

// 源图打包：把当前源图目录（未去水印原图）打包 zip，交付链路与结果页导出一致
export async function exportSourceZip() {
  if (!store.post || !store.post.dir) {
    showToast('没有可打包的源图目录', 'danger')
    return
  }
  try {
    const zipPath = await ExportSourceZip(store.post.dir)
    const saveTo = await PickSaveFile(fileName(zipPath))
    if (saveTo) {
      await CopyFile(zipPath, saveTo)
      log('已保存源图: ' + saveTo, 'ok')
      showToast('源图 zip 已保存', 'ok')
    }
  } catch (e) {
    log('源图打包失败: ' + e, 'fail')
    showToast(String(e))
  }
}

// 下载 BGM：filename 由用户在弹窗中输入（空则后端兜底 bgm），两种保存方式：
//   mode='standalone'（默认）→ 弹「另存为」对话框，独立保存到用户指定位置，不写入源图目录，
//                             因此不会随「源图打包」进 zip；
//   mode='bundle'            → 存到帖子图片目录（源图打包 zip 会自动包含）。
// 成功后记录 store.bgmSaved / store.bgmMode 供按钮与弹窗回显。
// 候选链：优先用后端返回的 audioCandidates（多 CDN 备用地址，逐条尝试），
// 老数据只有 audioUrl 时退回单地址，保持兼容。
export function audioCandidates(post) {
  if (!post) return []
  const list = Array.isArray(post.audioCandidates) ? post.audioCandidates : []
  const cleaned = list.filter((u) => typeof u === 'string' && u.trim())
  if (cleaned.length) return cleaned
  return post.audioUrl ? [post.audioUrl] : []
}

export async function saveBGM(filename, mode = 'standalone') {
  const post = store.post
  const cands = audioCandidates(post)
  if (!cands.length) {
    showToast('当前帖子没有可下载的 BGM', 'danger')
    return
  }
  try {
    if (mode === 'bundle') {
      const saved = await DownloadBGM(cands, post.dir, filename || '')
      store.bgmSaved = saved
      store.bgmMode = 'bundle'
      log(`BGM 已存入源图目录: ${fileName(saved)}（会随源图打包进 zip）`, 'ok')
      showToast('BGM 已存入源图目录', 'ok')
      return
    }
    const saved = await DownloadBGMStandalone(cands, filename || '')
    if (!saved) {
      // 用户在「另存为」对话框点了取消：不是失败，不改动已有状态
      showToast('已取消保存 BGM', 'info')
      return
    }
    store.bgmSaved = saved
    store.bgmMode = 'standalone'
    log(`BGM 已单独保存: ${saved}（不在源图 zip 内）`, 'ok')
    showToast('BGM 已单独保存', 'ok')
  } catch (e) {
    log('BGM 下载失败: ' + e, 'fail')
    showToast('BGM 下载失败: ' + e)
  }
}

// 放大查看：加载原图（maxW=0 不缩放）到全屏浮层；记录所在列表用于左右切换
export async function viewImage(path, list) {
  const items = Array.isArray(list) && list.length ? list : store.cleanedThumbs
  store.viewerList = items
  store.viewerIndex = items.findIndex((t) => t.path === path)
  await loadViewer(path)
}

async function loadViewer(path) {
  store.viewer = { open: true, name: fileName(path), src: '', loading: true }
  try {
    const t = await GetThumb(path, 0)
    store.viewer.src = t.thumb
  } catch (e) {
    showToast('无法加载原图预览: ' + e)
    store.viewer.open = false
  } finally {
    store.viewer.loading = false
  }
}

// 放大浮层内左右切换：dir=-1 上一张 / +1 下一张（首尾循环）
export function viewerNav(dir) {
  const items = store.viewerList
  if (!items.length || store.viewerIndex < 0) return
  store.viewerIndex = (store.viewerIndex + dir + items.length) % items.length
  loadViewer(items[store.viewerIndex].path)
}

export function closeViewer() {
  store.viewer = { open: false, name: '', src: '', loading: false }
}

// ---------------- 事件订阅 ----------------
// 自动入队模式：无前台等待页，队列执行反馈统一经 queue:* 事件 + toast。

export function initEvents() {
  // 队列内下载的进度日志（RunDownload 内部发出的过程消息）
  EventsOn('download:progress', (d) => log(d.msg))
  // 队列事件：queue:updated 全量快照；queue:progress 单张进度；queue:done 终态提醒。
  EventsOn('queue:updated', (d) => {
    store.queue = d?.tasks || []
  })
  EventsOn('queue:progress', (d) => {
    const t = store.queue.find((x) => x.id === d.id)
    if (t) t.progress = { index: d.index, total: d.total, name: d.name, status: d.status }
    log(`[队列 ${d.index}/${d.total}] ${d.name} ${d.status === 'ok' ? '✓' : d.status === 'skip' ? '- 已取消' : '✗ ' + d.info}`, d.status === 'ok' ? 'ok' : d.status === 'skip' ? '' : 'fail')
  })
  EventsOn('queue:done', (d) => {
    const t = store.queue.find((x) => x.id === d.id)
    if (t) {
      t.state = d.state
      t.progress = null
    }
    if (d.state === 'done') {
      if (d.type === 'download') {
        showToast(`素材下载完成：${d.summary || ''}，可在任务队列点「去框选」`, 'ok')
        log(`下载任务完成: ${d.summary || d.outDir}`, 'ok')
      } else {
        showToast(`去水印完成（${d.summary || ''}），可在任务队列点「查看结果」导出`, 'ok')
        log(`去水印任务完成: ${d.summary || d.outDir}`, 'ok')
      }
    } else if (d.state === 'failed') {
      showToast(`任务失败: ${d.err || ''}（可在任务队列重试）`, 'danger')
      log(`任务失败: ${d.err}`, 'fail')
    }
  })
  // 引擎状态事件 -> 全局状态 + 日志（error 弹提示，ready/starting 记录日志）
  EventsOn('engine:status', (d) => {
    store.engineStatus = d.state
    store.engineMessage = d.message || ''
    if (d.state === 'ready') {
      log(d.message || 'AI 引擎已就绪', 'ok')
    } else if (d.state === 'error') {
      log('AI 引擎错误: ' + d.message, 'fail')
      showToast('AI 引擎错误: ' + d.message, 'danger')
    } else {
      log(d.message || 'AI 引擎启动中…')
    }
  })

  // 启动时主动同步一次引擎状态与队列快照（弥补订阅建立前错过的事件）
  GetEngineStatus()
    .then((d) => {
      if (d && d.state) {
        store.engineStatus = d.state
        store.engineMessage = d.message || ''
      }
    })
    .catch(() => {})
  refreshQueue().catch(() => {})
}

function fileName(p) {
  return p.split(/[\\/]/).pop()
}
