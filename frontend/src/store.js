import { reactive } from 'vue'
import {
  GetThumb,
  DownloadSocial,
  DownloadBGM,
  DownloadBGMStandalone,
  StartBatch,
  CancelBatch,
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

  // ---------------- 批处理任务队列（PRD §4） ----------------
  queue: [], // 队列任务快照（由 queue:updated 全量同步）
  pageTaskId: null, // 由第三步「开始去水印」发起的任务 ID（用于 batch:* 事件防串扰：
  // 后台队列任务的双路推送不得推进当前页面的流水线状态机，PRD §5.1）
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

export async function startDownload() {
  const url = store.inputUrl.trim()
  if (!url) return
  store.stage = 'downloading'
  store.running = true
  log('开始下载: ' + url)
  try {
    const post = await DownloadSocial(url)
    store.post = post
    store.bgmSaved = ''
    store.bgmMode = 'standalone'
    store.selected = []
    store.previewPath = ''
    store.sourceThumbs = []
    for (const p of post.files) {
      try {
        const t = await GetThumb(p, 420)
        store.sourceThumbs.push({ path: p, thumb: t.thumb, name: fileName(p), width: t.width, height: t.height })
      } catch (e) {
        // 缩略图失败也保留条目，避免图片从网格中消失
        log(`缩略图生成失败: ${fileName(p)}`, 'fail')
        store.sourceThumbs.push({ path: p, thumb: '', name: fileName(p), width: 0, height: 0, failed: true })
      }
    }
    log(`下载完成: ${post.platform} | ${post.title} | ${post.count} 张`)
    store.stage = 'downloaded'
  } catch (e) {
    log('下载失败: ' + e, 'fail')
    showToast(String(e))
    store.stage = 'idle'
  } finally {
    store.running = false
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
  store.running = true
  store.stage = 'downloading'
  log('选择本地文件夹: ' + dir)
  try {
    const files = await ListImages(dir)
    if (!files.length) {
      showToast('该文件夹内没有可处理的图片')
      store.stage = 'idle'
      return
    }
    store.post = {
      platform: '本地文件夹',
      id: '',
      title: dir,
      author: '',
      dir: dir,
      files: files,
      count: files.length,
      audioUrl: '',
      audioName: '',
    }
    store.bgmSaved = ''
    store.bgmMode = 'standalone'
    store.selected = []
    store.previewPath = ''
    store.sourceThumbs = []
    for (const p of files) {
      try {
        const t = await GetThumb(p, 420)
        store.sourceThumbs.push({ path: p, thumb: t.thumb, name: fileName(p), width: t.width, height: t.height })
      } catch (e) {
        log(`缩略图生成失败: ${fileName(p)}`, 'fail')
        store.sourceThumbs.push({ path: p, thumb: '', name: fileName(p), width: 0, height: 0, failed: true })
      }
    }
    log(`已加载 ${files.length} 张本地图片`, 'ok')
    store.stage = 'downloaded'
  } catch (e) {
    log('加载失败: ' + e, 'fail')
    showToast(String(e))
    store.stage = 'idle'
  } finally {
    store.running = false
  }
}

export async function startBatch(scopeAll = true) {
  if (!store.boxes.length) {
    showToast('请先在预览图上拖拽框选水印区域')
    return
  }
  if (store.engineStatus === 'starting') {
    showToast('AI 引擎正在启动，请稍候…', 'info')
    return
  }
  if (store.engineStatus === 'error') {
    showToast('AI 引擎未就绪：' + (store.engineMessage || '启动失败'), 'danger')
    return
  }
  store.running = true
  store.canceling = false
  store.stage = 'inpainting'
  store.cleanedThumbs = []
  const relative = store.mode === 'relative'
  const boxes = relative ? store.ratios : store.boxes
  let inDir = store.post.dir
  try {
    if (!scopeAll && store.selected.length > 0) {
      inDir = await PrepareSubset(store.selected)
      log(`仅处理勾选的 ${store.selected.length} 张`)
    }
    const outDir = inDir + '_去水印'
    store.cleanedDir = outDir
    // 经队列串行执行（StartBatch 内部包装为 inpaint 任务，PRD §3.2）：
    // 记录任务 ID，用于把本页发起的 batch:* 事件与后台队列任务区分开。
    // title 固化帖子标题，队列面板展示用（而非文件路径）。
    const task = await StartBatch(inDir, outDir, {
      boxes, relative, dilate: store.dilate, margin: 64, maskPath: '', strategy: store.strategy,
    }, store.post?.title || store.post?.dir || '')
    store.pageTaskId = task?.id || null
  } catch (e) {
    log('启动失败: ' + e, 'fail')
    showToast(String(e))
    store.running = false
    store.canceling = false
    store.stage = 'downloaded'
  }
}

export function cancelBatch() {
  if (store.canceling) return
  store.canceling = true
  log('已请求取消，正在停止…', 'fail')
  showToast('正在取消…', 'info')
  // 页面发起的任务按 ID 精确取消（可能仍在排队尚未执行）；
  // 兜底 CancelBatch 取消当前运行中的批次（旧行为）。
  const p = store.pageTaskId
    ? CancelQueueTask(store.pageTaskId).catch(() => CancelBatch())
    : Promise.resolve().then(() => CancelBatch())
  p.catch((e) => {
    store.canceling = false
    log('取消失败: ' + e, 'fail')
    showToast('取消失败: ' + e)
  })
}

// ---------------- 批处理任务队列（PRD §2.2/§4.2） ----------------

// 批量解析并加入队列：输入按空白/逗号/分号（含全角）拆分为多条链接
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

// 固化当前框选参数，建去水印任务入队（不打断当前页面；scopeAll 与 startBatch 一致）
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
// 载入结果目录缩略图并切到第五步，直接可执行「导出 zip」。
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
    store.cleanedThumbs = []
    await loadCleanedThumbs(task.resultDir, files)
    store.running = false
    store.canceling = false
    store.stage = 'inpainted'
    log(`已打开任务结果: ${task.resultDir}（${files.length} 张），可导出 zip`, 'ok')
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

// 载入已完成下载任务的素材，进入第三步框选（PRD §2.2 入口二「去框选」）。
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

// loadMaterialIntoStore 把一组本地图片装入第三步框选流程（loadLocalFolder 与
// loadTaskMaterial 共用，避免逻辑重复）。
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
  store.sourceThumbs = []
  store.pageTaskId = null
  for (const p of files) {
    try {
      const t = await GetThumb(p, 420)
      store.sourceThumbs.push({ path: p, thumb: t.thumb, name: fileName(p), width: t.width, height: t.height })
    } catch (e) {
      log(`缩略图生成失败: ${fileName(p)}`, 'fail')
      store.sourceThumbs.push({ path: p, thumb: '', name: fileName(p), width: 0, height: 0, failed: true })
    }
  }
  store.stage = 'downloaded'
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

export async function loadCleanedThumbs(dir, files) {
  store.cleanedThumbs = []
  for (const p of files) {
    try {
      const t = await GetThumb(p, 420)
      store.cleanedThumbs.push({ path: p, thumb: t.thumb, name: fileName(p), width: t.width, height: t.height })
    } catch (e) {
      store.cleanedThumbs.push({ path: p, thumb: '', name: fileName(p), width: 0, height: 0, failed: true })
    }
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

export function initEvents() {
  EventsOn('download:progress', (d) => log(d.msg))
  EventsOn('download:error', (d) => {
    log('下载错误: ' + d.msg, 'fail')
    showToast(d.msg)
  })
  // batch:progress：仅当事件属于本页发起的任务时推进页面进度；
  // 后台队列任务的双路推送只进队列面板（PRD §5.1 防串扰）。
  EventsOn('batch:progress', (d) => {
    if (d.taskId && d.taskId !== store.pageTaskId) return
    store.progress = d
    log(`[${d.index}/${d.total}] ${d.name} ${d.status === 'ok' ? '✓' : d.status === 'skip' ? '- 已取消' : '✗ ' + d.info}`, d.status === 'ok' ? 'ok' : d.status === 'skip' ? '' : 'fail')
  })
  EventsOn('batch:done', async (d) => {
    // 非本页发起的后台任务完成：不推进当前页面的流水线状态机，
    // 仅记日志提示（页面可能正停留在其他帖子的框选/预览视图）。
    if (d.taskId && d.taskId !== store.pageTaskId) {
      if (d.canceled) {
        log(`后台任务已取消：完成 ${d.ok} / 共 ${d.total}`, 'fail')
      } else if (d.err) {
        log(`后台去水印任务失败: ${d.err}`, 'fail')
      } else {
        log(`后台去水印完成: 成功 ${d.ok} / 共 ${d.total}（结果目录 ${d.outDir}）`, 'ok')
      }
      return
    }
    // 本页发起的任务：无论用户是否切走页面，都要释放运行态标志，
    // 否则「下载图片」等按钮会被永久禁用（stuck running）。
    store.pageTaskId = null
    store.running = false
    store.canceling = false
    // 任务级失败（全部图片失败 / 执行出错）：明确报错并退回框选页，不静默
    if (d.err) {
      log(`去水印任务失败: ${d.err}`, 'fail')
      showToast('去水印任务失败: ' + d.err, 'danger')
      if (store.stage === 'inpainting') store.stage = 'downloaded'
      return
    }
    if (d.canceled) {
      log(`已取消：完成 ${d.ok} / 共 ${d.total}`, 'fail')
      showToast('已取消抹除', 'info')
    } else {
      log(`去水印完成: 成功 ${d.ok} / 共 ${d.total}`, 'ok')
    }
    // 仅当用户仍停留在处理中页面时才推进到结果页；若已切走，只提示不打扰
    const files = await listDirFiles(d.outDir)
    if (files.length > 0) {
      store.cleanedDir = d.outDir
      await loadCleanedThumbs(d.outDir, files)
      if (store.stage === 'inpainting') {
        store.stage = 'inpainted'
      } else {
        showToast(`去水印完成（${d.ok}/${d.total}），结果已就绪可导出`, 'ok')
      }
    } else if (store.stage === 'inpainting') {
      store.stage = 'downloaded'
    }
  })
  // 队列事件（PRD §4.3）：queue:updated 全量快照；queue:progress 单张进度；
  // queue:done 后台任务终态提醒（面板数据以 queue:updated 为准，此处只提示）。
  EventsOn('queue:updated', (d) => {
    store.queue = d?.tasks || []
  })
  EventsOn('queue:progress', (d) => {
    const t = store.queue.find((x) => x.id === d.id)
    if (t) t.progress = { index: d.index, total: d.total, name: d.name, status: d.status }
    // 本页发起的任务已由 batch:progress 记录日志，避免双份刷屏
    if (d.id !== store.pageTaskId) {
      log(`[队列 ${d.index}/${d.total}] ${d.name} ${d.status === 'ok' ? '✓' : d.status === 'skip' ? '- 已取消' : '✗ ' + d.info}`, d.status === 'ok' ? 'ok' : d.status === 'skip' ? '' : 'fail')
    }
  })
  EventsOn('queue:done', (d) => {
    const t = store.queue.find((x) => x.id === d.id)
    if (t) {
      t.state = d.state
      t.progress = null
    }
    if (d.id === store.pageTaskId) return // 本页任务的终态由 batch:done 处理
    if (d.state === 'done') {
      showToast(`后台任务完成：${d.summary || d.outDir || ''}`, 'ok')
      log(`后台任务完成: ${d.summary || d.outDir}`, 'ok')
    } else if (d.state === 'failed') {
      showToast(`后台任务失败: ${d.err || ''}`, 'danger')
      log(`后台任务失败: ${d.err}`, 'fail')
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

// ---------------- 绑定辅助（目录列表经 PrepareSubset/zip 之外需要列目录） ----------------

async function listDirFiles(dir) {
  try {
    return await ListImages(dir)
  } catch (e) {
    return []
  }
}

function fileName(p) {
  return p.split(/[\\/]/).pop()
}
