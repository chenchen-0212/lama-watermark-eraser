import { reactive } from 'vue'
import {
  GetThumb,
  DownloadSocial,
  DownloadBGM,
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
} from '../wailsjs/go/main/App'
import { EventsOn } from '../wailsjs/runtime/runtime'

export const store = reactive({
  // 流水线状态机：idle → downloading → downloaded → inpainting → inpainted
  stage: 'idle',
  inputUrl: '',
  post: null, // {platform,title,author,dir,files,count,audioUrl,audioName}
  bgmSaved: '', // 已保存的 BGM 本地路径（空=未下载）
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
    await StartBatch(inDir, outDir, {
      boxes, relative, dilate: store.dilate, margin: 64, maskPath: '', strategy: store.strategy,
    })
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
  CancelBatch().catch((e) => {
    store.canceling = false
    log('取消失败: ' + e, 'fail')
    showToast('取消失败: ' + e)
  })
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

// 下载 BGM：filename 由用户在弹窗中输入（空则后端兜底 bgm.mp3），保存到帖子图片目录
// （源图打包 zip 会自动包含）。成功后记录 store.bgmSaved 供按钮回显。
export async function saveBGM(filename) {
  const post = store.post
  if (!post || !post.audioUrl) {
    showToast('当前帖子没有可下载的 BGM', 'danger')
    return
  }
  try {
    const saved = await DownloadBGM(post.audioUrl, post.dir, filename || '')
    store.bgmSaved = saved
    log(`BGM 已保存: ${fileName(saved)}（含在源图打包中）`, 'ok')
    showToast('BGM 已保存', 'ok')
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
  EventsOn('batch:progress', (d) => {
    store.progress = d
    log(`[${d.index}/${d.total}] ${d.name} ${d.status === 'ok' ? '✓' : d.status === 'skip' ? '- 已取消' : '✗ ' + d.info}`, d.status === 'ok' ? 'ok' : d.status === 'skip' ? '' : 'fail')
  })
  EventsOn('batch:done', async (d) => {
    store.running = false
    store.canceling = false
    if (d.canceled) {
      log(`已取消：完成 ${d.ok} / 共 ${d.total}`, 'fail')
      showToast('已取消抹除', 'info')
    } else {
      log(`去水印完成: 成功 ${d.ok} / 共 ${d.total}`, 'ok')
    }
    const files = await listDirFiles(d.outDir)
    if (files.length > 0) {
      store.cleanedDir = d.outDir
      await loadCleanedThumbs(d.outDir, files)
      store.stage = 'inpainted'
    } else {
      store.stage = 'downloaded'
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

  // 启动时主动同步一次引擎状态（弥补订阅建立前错过的事件）
  GetEngineStatus()
    .then((d) => {
      if (d && d.state) {
        store.engineStatus = d.state
        store.engineMessage = d.message || ''
      }
    })
    .catch(() => {})
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
