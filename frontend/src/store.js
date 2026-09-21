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
  GetImageRaw,
  AddQueueTasks,
  EnqueueInpaint,
  ListQueueTasks,
  RetryQueueTask,
  CancelQueueTask,
  RemoveQueueTask,
  ClearFinishedTasks,
  ClearGroupTasks,
  ResolveTaskAudio,
} from '../wailsjs/go/main/App'
import { EventsOn } from '../wailsjs/runtime/runtime'

export const store = reactive({
  // 流水线状态机：idle → downloading → downloaded → inpainting → inpainted
  stage: 'idle',
  inputUrl: '',
  post: null, // {platform,title,author,dir,files,count,audioUrl,audioCandidates,audioName}
  //                       ↑ BGM 三字段在「先占位后执行」下可能后置补写：
  //                         进框选页时先为空，解析完成后就地赋值，按钮随之浮现
  materialLoading: false, // 框选页正处占位期（清单/BGM 仍在后台就绪）
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
  strategy: 'crop', // 处理策略: auto(智能)|original(整图)|crop(快速裁剪)；默认快速（用户偏好）
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

// 本地文件夹模式：选择目录 → 列出图片 → 进入框选去水印流程。
// 目录已由 PickDirectory 选定，列目录必须在切页前完成（没有清单就无占位可铺），
// 但缩略图同样转入后台：切页本身不等任何图片解码。
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
    loadMaterialIntoStore('本地文件夹', dir, dir, files)
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
// 【先占位、后执行】切页不等缩略图，也不等 ListImages——清单本身在后台补：
// 先用已知的张数铺满占位格子，清单到达后用真实文件替换，导出按钮在占位期
// 保持可用（导出只依赖 cleanedDir）。这样点「查看结果」也是瞬时的。
export async function openTaskResult(task) {
  if (!task?.resultDir) {
    showToast('该任务没有结果目录', 'danger')
    return
  }
  store.cleanedDir = task.resultDir
  store.cleanedThumbs = []
  store.running = false
  store.canceling = false
  store.stage = 'inpainted'
  log(`已打开任务结果: ${task.resultDir}，正在载入结果…`)
  try {
    const files = await ListImages(task.resultDir)
    if (!files.length) {
      showToast('结果目录中没有可导出的图片（可能已被清理）', 'danger')
      return
    }
    if (store.stage !== 'inpainted' || store.cleanedDir !== task.resultDir) return // 已切走
    store.cleanedThumbs = files.map((p) => ({
      path: p, thumb: '', name: fileName(p), width: 0, height: 0, loading: true,
    }))
    log(`结果已就绪：${files.length} 张，缩略图加载中…`, 'ok')
    fillThumbs(store.cleanedThumbs)
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

// 按类型清空全部终态任务（type 为空清全部）。
// 【已无 UI 入口】队列面板改为按时间分组后，每组走 clearGroupQueue；
// 保留本函数作为「一键清空全部」的回滚入口（分组展示若需要退回按类型展示）。
export async function clearFinishedQueue(type = '') {
  try {
    await ClearFinishedTasks(type)
    showToast('已清空已完成任务', 'ok')
  } catch (e) {
    showToast('清空失败: ' + e)
  }
}

// 清空「某一时间分组」内的终态任务（队列面板每组的一键清空）。
// since / until 为秒级时间戳（0 = 该侧不限），与后端 ClearGroupTasks 契约一致。
// 仅终态任务会被删除——排队中/执行中的任务后端硬性保底不删，故本操作
// 对正在跑的任务绝对安全；用户看到的数量与实际删除数量一致。
export async function clearGroupQueue(since, until, type = '') {
  try {
    await ClearGroupTasks(type, since || 0, until || 0)
    showToast('已清空该分组的已完成任务', 'ok')
  } catch (e) {
    showToast('清空失败: ' + e)
  }
}

// 载入已完成下载任务的素材，进入框选页（队列面板「去框选」）。
// 优先使用任务记录的图片清单（task.files，本次下载的确切文件、已按内容去重），
// 仅在清单缺失时回退列目录——避免把目录里的历史残留文件混入造成图片重复。
//
// 【先占位、后执行】点击必须秒进：本函数只做「用任务记录里的数据铺占位 + 切页」，
// 不含任何网络/解码等待。两条慢路径全部转入后台：
//   1) 图片清单缺失时列目录（ListImages 要遍历磁盘）；
//   2) BGM 取源解析（taskAudio 可能回源请求后端，最慢的一环）。
// 二者完成后各自静默补写 store（图片清单变化 → 重建占位；BGM 解析出结果 →
// 按钮与试听条自然浮现），不使用户停留在「点了没反应」的状态。
export async function loadTaskMaterial(task) {
  if (!task?.resultDir) {
    showToast('该任务没有可载入的素材目录', 'danger')
    return
  }
  // ① 立即切页：有清单就用清单，没有先用空占位，绝不等 ListImages
  const files = Array.isArray(task.files) ? task.files.filter(Boolean) : []
  const title = task.postRef || task.resultDir
  loadMaterialIntoStore(task.platform || '队列任务', title, task.resultDir, files)
  if (!files.length) {
    store.materialLoading = true // 占位期：网格显示「正在载入素材」
    log('素材清单缺失，正在扫描目录…')
    backfillTaskFiles(task, title)
  }
  // ② BGM 取源后台解析（可能与 ① 的列目录并行；BGM 是可选增强，不阻塞页面）
  resolveTaskAudioIntoStore(task)
  log(`已载入队列素材: ${title}${files.length ? `（${files.length} 张）` : ''}` +
    (task.audioCandidates?.length ? `｜含 BGM：${task.audioName || '背景音乐'}` : ''), 'ok')
}

// backfillTaskFiles 后台补齐缺失的图片清单（任务记录无 files 的旧数据）。
// 只在当前仍是同一任务时才写入：用户可能已切换素材，过期结果必须丢弃。
async function backfillTaskFiles(task, title) {
  try {
    const files = await ListImages(task.resultDir)
    if (!files.length) {
      store.materialLoading = false
      showToast('该素材目录中没有可处理的图片', 'danger')
      return
    }
    if (!isCurrentPost(task.resultDir)) return // 用户已切走，丢弃过期结果
    applyFilesIntoStore(title, task.resultDir, files)
    store.materialLoading = false
    log(`目录扫描完成：${files.length} 张`, 'ok')
  } catch (e) {
    store.materialLoading = false
    log('素材目录扫描失败: ' + e, 'fail')
    showToast(String(e))
  }
}

// resolveTaskAudioIntoStore 后台解析 BGM 取源，就绪后就地补写当前帖子。
// 写入前校验「当前帖子仍是该任务」：解析期间用户可能已切到别的素材，
// 此时补写会把 A 帖的 BGM 挂到 B 帖上（错配比缺失更糟）。
async function resolveTaskAudioIntoStore(task) {
  const audio = await taskAudio(task)
  if (!audio.candidates.length) return // 无 BGM：按钮保持隐藏，符合该帖实情
  if (!isCurrentPost(task.resultDir)) return // 已切走，丢弃
  // 用户已在弹窗里改过保存方式/已下过 BGM 时不覆盖，只补取源
  store.post.audioUrl = audio.candidates[0]
  store.post.audioCandidates = audio.candidates
  store.post.audioName = audio.name || ''
  log(`BGM 已就绪：${audio.name || '背景音乐'}（可下载或试听）`, 'ok')
}

// isCurrentPost 当前框选页的素材是否仍是该目录
function isCurrentPost(dir) {
  return store.stage === 'downloaded' && store.post?.dir === dir
}

// taskAudio 解析任务的 BGM 取源。
//
// 三级来源，优先级从高到低：
//  1. 任务自带的候选链（本次改造后新任务都有）；
//  2. audioUrl 单地址（更早版本落库的数据，兼容）；
//  3. 回源补取——任务既无候选链、audioChecked 也未置位，说明该任务是在 BGM
//     落库能力上线前完成的，向后端请求重新解析一次帖子元数据并写回任务。
//     补取失败不阻断素材载入：BGM 是可选项，图片照常进入框选。
// 前两级是纯内存操作（同步返回），只有第三级才会发起网络请求——这正是
// 「先占位后执行」得以秒开的原因：绝大多数任务根本不需要等这里。
async function taskAudio(task) {
  const empty = { candidates: [], name: '' }
  const own = Array.isArray(task?.audioCandidates) ? task.audioCandidates.filter(Boolean) : []
  if (own.length) return { candidates: own, name: task.audioName || '' }
  if (task?.audioUrl) return { candidates: [task.audioUrl], name: task.audioName || '' }
  if (!task?.id || task.audioChecked) return empty // 已确认过没有 BGM，不再打扰后端
  if (!isCurrentPost(task.resultDir)) return empty // 用户已切走，没必要再补取

  try {
    log('该任务缺少 BGM 取源，正在补取…')
    const r = await ResolveTaskAudio(task.id)
    const list = Array.isArray(r?.candidates) ? r.candidates.filter(Boolean) : []
    if (list.length) log(`已补取 BGM 取源（${list.length} 条候选）`, 'ok')
    return { candidates: list, name: r?.name || '' }
  } catch (e) {
    // 补取失败按「无 BGM」处理：不阻断框选流程
    log('BGM 取源补取失败（不影响图片处理）: ' + e, 'fail')
    return empty
  }
}

// loadMaterialIntoStore 把一组本地图片装入框选流程并**立即切页**（loadLocalFolder
// 与 loadTaskMaterial 共用）。
//
// 【先占位、后执行】本函数不含任何 await：同步铺好占位条目、同步把 stage 置为
// downloaded（此刻界面已可交互），缩略图生成作为后台任务推进。原实现在末尾
// await 了整个 fillThumbs，调用方因此要等全部缩略图就绪才切页——这正是
// 「点去框选半天没反应」的根因。现在 fillThumbs 只被启动、不被等待，
// 它逐张把占位替换成真实缩略图（网格渐进点亮），期间用户可以照常框选。
function loadMaterialIntoStore(platform, title, dir, files, audio = null) {
  const cands = Array.isArray(audio?.candidates) ? audio.candidates.filter(Boolean) : []
  store.post = {
    platform,
    id: '',
    title,
    author: '',
    dir,
    files,
    count: files.length,
    // 三者与后端 downloader.Post 的前端契约一一对应：
    // audioUrl 是 hasBGM 的判据（=候选链首项），audioCandidates 供下载逐条重试
    audioUrl: cands[0] || '',
    audioCandidates: cands,
    audioName: audio?.name || '',
  }
  store.bgmSaved = ''
  store.bgmMode = 'standalone'
  store.selected = []
  store.previewPath = ''
  store.sourceThumbs = files.map((p) => ({
    path: p, thumb: '', name: fileName(p), width: 0, height: 0, loading: true,
  }))
  // 立即切页：随后发生的缩略图填充是纯后台增强，不再阻塞进页面
  store.stage = 'downloaded'
  fillThumbs(store.sourceThumbs)
}

// applyFilesIntoStore 清单异步补齐后就地替换占位（保持当前帖子对象引用稳定，
// 避免触发 App.vue 里 watch(() => store.post) 的 BGM 停播副作用）。
function applyFilesIntoStore(title, dir, files) {
  const post = store.post
  if (!post) return
  post.dir = dir
  post.title = title
  post.files = files
  post.count = files.length
  store.selected = []
  store.previewPath = ''
  store.sourceThumbs = files.map((p) => ({
    path: p, thumb: '', name: fileName(p), width: 0, height: 0, loading: true,
  }))
  fillThumbs(store.sourceThumbs)
}

// THUMB_BATCH 缩略图并发批大小：串行 await 每张都要一次 IPC 往返 + 解码，
// 大图集（100 张）下总耗时明显；分批并发在「首图出现速度」与「后端压力」
// 之间取平衡，同时保持渐进感（每批完成就点亮一批占位）。
const THUMB_BATCH = 4

// fillThumbs 分批并发地生成缩略图并就地替换占位条目（渐进出现在网格中）。
// 兜底链（方案：缩略图失败显示原图）：缩略图失败重试一次 → 仍失败改用
// GetImageRaw 原图直显（不缩放，浏览器解码更宽容，标记 raw 供角标展示）
// → 原图也失败才置 failed（网格显示「无法预览」）。
async function fillThumbs(items) {
  for (let i = 0; i < items.length; i += THUMB_BATCH) {
    const batch = items.slice(i, i + THUMB_BATCH)
    await Promise.all(batch.map(fillOneThumb))
  }
}

async function fillOneThumb(item) {
  try {
    const t = await GetThumb(item.path, 420)
    item.thumb = t.thumb
    item.width = t.width
    item.height = t.height
  } catch (e) {
    // 重试一次（瞬态读取/解码错误）
    try {
      const t = await GetThumb(item.path, 420)
      item.thumb = t.thumb
      item.width = t.width
      item.height = t.height
    } catch (e2) {
      try {
        item.thumb = await GetImageRaw(item.path)
        item.raw = true // 原图兜底（未缩放），网格加角标提示
      } catch (e3) {
        item.failed = true
        log(`缩略图与原图均无法加载: ${item.name}`, 'fail')
      }
    }
  }
  item.loading = false
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
