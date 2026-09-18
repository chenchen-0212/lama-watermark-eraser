<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  store,
  initEvents,
  startDownload,
  loadLocalFolder,
  startBatch,
  cancelBatch,
  exportZip,
  exportSourceZip,
  saveBGM,
  audioCandidates,
  showToast,
  viewImage,
  viewerNav,
  closeViewer,
  addQueueTasks,
  enqueueInpaint,
  refreshQueue,
  retryQueueTask,
  cancelQueueTask,
  removeQueueTask,
  clearFinishedQueue,
  loadTaskMaterial,
  openTaskResult,
} from './store.js'
import { changelog, kindLabel } from './changelog.js'
import { GetAppVersion, GetBGMAudio, GetDouyinCookie, SetDouyinCookie } from '../wailsjs/go/main/App'
import ImageGrid from './components/ImageGrid.vue'
import WatermarkCanvas from './components/WatermarkCanvas.vue'

const steps = ['输入链接', '下载图片', '框选去水印', '结果预览', '导出 zip']
const stepIdx = computed(
  () =>
    ({ idle: 0, downloading: 1, downloaded: 2, inpainting: 3, inpainted: 4 }[
      store.stage
    ] ?? 0
  )
)

const previewItem = computed(() => {
  if (store.previewPath) {
    const hit = store.sourceThumbs.find((t) => t.path === store.previewPath)
    if (hit && hit.thumb) return hit
  }
  return store.sourceThumbs.find((t) => t.thumb) || null
})
const previewSrc = computed(() => previewItem.value?.thumb || '')

const pct = computed(() =>
  store.progress.total
    ? Math.round((store.progress.index / store.progress.total) * 100)
    : 0
)

// AI 引擎未就绪（starting/error）期间禁用去水印按钮
const engineReady = computed(() => store.engineStatus === 'ready')

const urlRef = ref(null)
const showNotice = ref(false)

// ---------------- 版本信息 ----------------
// 底部签名处的版本号来自后端（构建时注入，与安装包版本同源）。
// 取不到时留空 —— 宁可只显示「作者 csy」，也不显示一个可能错误的版本号。
const appVersion = ref('')
// 更新明细弹窗：默认展开首项（当前版本）
const showChangelog = ref(false)
const changelogList = changelog
const currentVersion = computed(() => changelogList[0] || null)

// 抖音 Cookie 设置弹窗
const showCookie = ref(false)
const cookieInput = ref('')
const cookieSaving = ref(false)

async function openCookie() {
  try {
    cookieInput.value = await GetDouyinCookie()
  } catch {
    cookieInput.value = ''
  }
  showCookie.value = true
}

async function saveCookie() {
  cookieSaving.value = true
  try {
    await SetDouyinCookie(cookieInput.value.trim())
    showToast('抖音 Cookie 已保存', 'ok')
    showCookie.value = false
  } catch (e) {
    showToast('保存失败: ' + e)
  } finally {
    cookieSaving.value = false
  }
}

async function clearCookie() {
  cookieSaving.value = true
  try {
    await SetDouyinCookie('')
    cookieInput.value = ''
    showToast('已清除抖音 Cookie', 'ok')
  } catch (e) {
    showToast('清除失败: ' + e)
  } finally {
    cookieSaving.value = false
  }
}

// ---------------- 任务队列（PRD §2.2 入口二） ----------------
// 面板展示队列任务列表；数据由 queue:updated 事件全量同步，
// 打开时主动拉取一次快照（弥补订阅前错过的事件）。
const showQueue = ref(false)
const queueInput = ref('')

async function openQueue() {
  showQueue.value = true
  await refreshQueue()
}

// 待处理任务数（顶栏角标：pending + running + retry_wait）
const queueActive = computed(
  () => store.queue.filter((t) => ['pending', 'running', 'retry_wait'].includes(t.state)).length
)

// 两类队列独立展示与管理（问题修复 #1）：下载与去水印分组，各自的清空/列表互不干扰
const downloadTasks = computed(() => store.queue.filter((t) => t.type === 'download'))
const inpaintTasks = computed(() => store.queue.filter((t) => t.type === 'inpaint'))
const queueSections = computed(() => [
  { key: 'download', title: '⬇ 下载任务', tasks: downloadTasks.value },
  { key: 'inpaint', title: '🖼 去水印任务', tasks: inpaintTasks.value },
])

// 第四步（处理中页）：本页任务的排队状态提示（问题修复 #3 的等待可感知性）
const pageTask = computed(() => store.queue.find((t) => t.id === store.pageTaskId) || null)
const queuedAhead = computed(() => {
  if (!pageTask.value || pageTask.value.state !== 'pending') return 0
  const idx = store.queue.findIndex((t) => t.id === pageTask.value.id)
  return store.queue.filter(
    (t, j) => j < idx && ['pending', 'running', 'retry_wait'].includes(t.state)
  ).length
})

function batchAdd() {
  if (!queueInput.value.trim()) return
  addQueueTasks(queueInput.value)
  queueInput.value = ''
}

// 队列任务状态/类型的展示元数据
const stateMeta = {
  pending: { label: '排队中', cls: 'st-pending' },
  running: { label: '执行中', cls: 'st-running' },
  retry_wait: { label: '等待重试', cls: 'st-retry' },
  done: { label: '已完成', cls: 'st-done' },
  failed: { label: '失败', cls: 'st-failed' },
  canceled: { label: '已取消', cls: 'st-cancel' },
}
const typeMeta = { download: '下载', inpaint: '去水印' }

function fmtTs(ts) {
  if (!ts) return ''
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

// 任务展示名：download=帖子摘要；inpaint=固化的标题；旧数据（无 title 字段）
// 用源目录前缀匹配已完成下载任务反查标题，仍找不到才退回文件路径
function taskDisplayName(t) {
  if (t.type === 'download') return t.postRef || t.url
  if (t.title) return t.title
  const hit = store.queue.find(
    (d) => d.type === 'download' && d.resultDir && t.inDir && t.inDir.startsWith(d.resultDir)
  )
  return (hit && (hit.title || hit.postRef)) || t.inDir
}

// BGM 保存弹窗：默认名取自曲目名，确认后按所选方式保存
// （standalone=另存为独立文件，不进源图 zip；bundle=存入源图目录，随 zip 打包）
const hasBGM = computed(() => !!(store.post && store.post.audioUrl))
const showBGM = ref(false)
const bgmName = ref('bgm.mp3')
const bgmMode = ref('standalone')
const bgmSaving = ref(false)

function openBGM() {
  bgmName.value = store.post?.audioName
    ? store.post.audioName.replace(/[\\/:*?"<>|]/g, ' ').trim() || 'bgm'
    : 'bgm'
  bgmMode.value = store.bgmMode || 'standalone'
  showBGM.value = true
}

async function confirmBGM() {
  bgmSaving.value = true
  try {
    await saveBGM(bgmName.value.trim(), bgmMode.value)
    showBGM.value = false
  } finally {
    bgmSaving.value = false
  }
}

// ---------------- BGM 试听 ----------------
// 显示条件与上方「下载BGM」按钮完全同源：同一个 hasBGM（帖子自带 audioUrl），
// 因此二者的出现/消失时机严格一致。
// 播放源优先「已下载的 BGM 文件」（store.bgmSaved）：已下载则播放该本地文件，
// 未下载则把帖子音频抓到本机试听缓存后播放（同帖只抓一次）。
const bgmPlayer = reactive({
  src: '', // 当前音频 data URL（空=尚未取源）
  loading: false, // 取源中（首次点击需下载/编码）
  playing: false,
  cur: 0, // 当前播放秒数
  dur: 0, // 总时长秒数
  error: '',
})
const bgmSrcCache = new Map() // 候选链签名 -> data URL（会话内复用，避免重复拉取）
let bgmAudio = null // 独立 Audio 实例：不占 DOM，切页/切帖也便于统一收停
let bgmLoadedSrc = '' // 已写入 el.src 的地址（避免重复赋值触发重新加载）

const bgmPct = computed(() => (bgmPlayer.dur > 0 ? Math.min(100, (bgmPlayer.cur / bgmPlayer.dur) * 100) : 0))

function ensureBgmAudio() {
  if (bgmAudio) return bgmAudio
  const el = new Audio()
  el.preload = 'none'
  el.addEventListener('timeupdate', () => {
    bgmPlayer.cur = el.currentTime || 0
  })
  const syncDur = () => {
    bgmPlayer.dur = Number.isFinite(el.duration) ? el.duration : 0
  }
  el.addEventListener('loadedmetadata', syncDur)
  el.addEventListener('durationchange', syncDur)
  el.addEventListener('play', () => {
    bgmPlayer.playing = true
  })
  el.addEventListener('pause', () => {
    bgmPlayer.playing = false
  })
  el.addEventListener('ended', () => {
    bgmPlayer.playing = false
    bgmPlayer.cur = 0
  })
  el.addEventListener('error', () => {
    bgmPlayer.playing = false
    if (bgmPlayer.src) bgmPlayer.error = '音频无法播放（格式不支持或文件已失效）'
  })
  bgmAudio = el
  return el
}

// 取源：已下载的本地文件 > 缓存 > 后端按候选链下载
async function ensureBgmSrc() {
  if (bgmPlayer.src) return bgmPlayer.src
  const cands = audioCandidates(store.post)
  // 会话内缓存键用候选链拼接：同一曲目的 CDN 地址带时效签名，
  // 只按首地址缓存会导致切回该帖时反复重新拉取。
  const cacheKey = cands.join('|')
  if (cacheKey && bgmSrcCache.has(cacheKey)) {
    bgmPlayer.src = bgmSrcCache.get(cacheKey)
    return bgmPlayer.src
  }
  bgmPlayer.loading = true
  bgmPlayer.error = ''
  try {
    const data = await GetBGMAudio(cands, store.bgmSaved || '')
    bgmPlayer.src = data
    if (cacheKey) bgmSrcCache.set(cacheKey, data)
    return data
  } catch (e) {
    bgmPlayer.error = '试听加载失败：' + e
    showToast('BGM 试听失败: ' + e)
    return ''
  } finally {
    bgmPlayer.loading = false
  }
}

async function toggleAudition() {
  const el = ensureBgmAudio()
  if (bgmPlayer.playing) {
    el.pause()
    return
  }
  const src = await ensureBgmSrc()
  if (!src) return
  if (bgmLoadedSrc !== src) {
    el.src = src
    bgmLoadedSrc = src
  }
  try {
    await el.play()
  } catch (e) {
    bgmPlayer.playing = false
    bgmPlayer.error = '播放失败：' + (e?.message || e)
  }
}

// 点击进度条跳转（按点击位置比例定位）
function seekAudition(e) {
  const el = bgmAudio
  if (!el || !bgmPlayer.dur) return
  const rect = e.currentTarget.getBoundingClientRect()
  if (!rect.width) return
  const ratio = Math.min(1, Math.max(0, (e.clientX - rect.left) / rect.width))
  el.currentTime = ratio * bgmPlayer.dur
  bgmPlayer.cur = el.currentTime
}

// 停止试听；clearSrc=true 时同时丢弃当前取源（帖子已更换）
function stopAudition(clearSrc = false) {
  if (bgmAudio) {
    try {
      bgmAudio.pause()
    } catch {
      /* 忽略：音频尚未加载时 pause 无副作用 */
    }
  }
  bgmPlayer.playing = false
  bgmPlayer.cur = 0
  if (clearSrc) {
    bgmPlayer.src = ''
    bgmPlayer.dur = 0
    bgmPlayer.error = ''
    bgmLoadedSrc = ''
  }
}

function fmtTime(s) {
  if (!Number.isFinite(s) || s <= 0) return '0:00'
  const total = Math.floor(s)
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`
}

// 换帖：取源作废；离开第三步：暂停但保留取源（返回时秒开）
watch(
  () => store.post,
  () => stopAudition(true)
)
watch(
  () => store.stage,
  (s) => {
    if (s !== 'downloaded') stopAudition(false)
  }
)
onBeforeUnmount(() => stopAudition(true))

function go() {
  if (store.running) return
  startDownload()
}

function backToAdjust() {
  store.stage = 'downloaded'
}

// 点击图片 → 设为上方预览/操作对象
function onPreview(path) {
  store.previewPath = path
}

// fn 区间选中反馈（ImageGrid 已写入 store.selected）
function onRange(e) {
  if (!e || !e.added) return
  showToast(`已选中第 ${e.from + 1}–${e.to + 1} 张，共 ${e.count} 张`, 'ok')
}

// 取消全部选中反馈（单张取消由网格内的状态条实时反映，不弹 toast 避免打扰）
function onDeselectAll(e) {
  if (!e || !e.count) return
  showToast(`已取消全部选中（${e.count} 张）`, 'info')
}

onMounted(() => {
  initEvents()
  // 版本号：失败静默降级为空（仅不显示版本标注，不影响任何功能）
  GetAppVersion()
    .then((v) => {
      appVersion.value = v || ''
    })
    .catch(() => {})
  showToast('支持公众号文章 / 小红书图文 / 抖音图文链接', 'info')
  window.addEventListener('keydown', onKey)
})

function onKey(e) {
  if (store.viewer.open && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) {
    viewerNav(e.key === 'ArrowLeft' ? -1 : 1)
    return
  }
  if (e.key === 'Escape') {
    if (store.viewer.open) closeViewer()
    else if (showNotice.value) showNotice.value = false
    else if (showQueue.value) showQueue.value = false
    else if (showChangelog.value) showChangelog.value = false
    else if (showCookie.value) showCookie.value = false
    else if (showBGM.value) showBGM.value = false
  }
}
</script>

<template>
  <div class="shell">
    <!-- 顶栏 -->
    <header class="topbar card">
      <div class="brand">
        <div class="logo">去水印</div>
        <div class="brand-text">
          <div class="title-row">
            <div class="title">社媒图文去水印工作台</div>
            <button class="notice-btn" @click="showNotice = true">
              <span class="notice-icon">⚠</span> 注意事项
            </button>
            <button class="notice-btn changelog-btn" @click="showChangelog = true">
              <span class="notice-icon">🆕</span> 更新明细
            </button>
            <button class="notice-btn queue-btn" @click="openQueue" title="批处理任务队列：批量下载与去水印后台排队执行">
              <span class="notice-icon">🗂</span> 任务队列
              <span v-if="queueActive" class="queue-badge">{{ queueActive }}</span>
            </button>
          </div>
          <div class="subtitle">下载 → 批量去水印 → 导出 zip</div>
        </div>
      </div>
      <div class="steps">
        <template v-for="(s, i) in steps" :key="s">
          <div class="step" :class="{ active: i === stepIdx, done: i < stepIdx }">
            <span class="dot">{{ i < stepIdx ? '✓' : i + 1 }}</span>
            <span class="step-label">{{ s }}</span>
          </div>
          <span v-if="i < steps.length - 1" class="step-arrow">›</span>
        </template>
      </div>
    </header>

    <!-- 主区域 -->
    <main class="content">
      <!-- 步骤1：输入链接 -->
      <transition name="stage">
        <section v-if="store.stage === 'idle'" class="stage-view center" key="idle">
          <div class="card url-card">
            <h2 class="h-title">粘贴社媒图文链接</h2>
            <p class="h-sub">支持微信公众号文章、小红书图文、抖音图文；视频链接将提示不支持</p>
            <div class="url-row">
              <input
                ref="urlRef"
                v-model="store.inputUrl"
                class="input"
                placeholder="https://www.xiaohongshu.com/explore/… 或 https://mp.weixin.qq.com/s/…"
                @keyup.enter="go"
              />
              <button class="btn btn-primary" :disabled="store.running || !store.inputUrl.trim()" @click="go">
                下载图片
              </button>
            </div>
            <!-- 批量入队（PRD §2.2 入口一）：每行一条链接，逐条预检平台后入队 -->
            <div class="queue-row">
              <textarea
                v-model="queueInput"
                class="input queue-textarea"
                rows="3"
                placeholder="批量模式：每行粘贴一个链接（可混合公众号 / 小红书 / 抖音图文），加入队列后台依次下载"
                spellcheck="false"
              ></textarea>
              <button class="btn" :disabled="!queueInput.trim()" @click="batchAdd">🗂 批量解析并加入队列</button>
            </div>
            <div class="local-row">
              <span class="local-divider"></span>
              <span class="local-label">或</span>
              <span class="local-divider"></span>
            </div>
            <div class="local-row">
              <button class="btn" :disabled="store.running" @click="loadLocalFolder">
                📁 选择本地文件夹
              </button>
              <span class="local-tip">直接识别并去水印本机图片，无需下载</span>
            </div>
            <div class="local-row">
              <button class="btn cookie-btn" :disabled="store.running" @click="openCookie">
                🍪 抖音 Cookie 设置
              </button>
              <span class="local-tip">抖音链接下载失败（风控）时配置，可提高成功率</span>
            </div>
            <div class="platforms">
              <span class="chip">微信公众号</span>
              <span class="chip">小红书图文</span>
              <span class="chip">抖音图文</span>
            </div>
          </div>
        </section>
      </transition>

      <!-- 步骤2：下载中 -->
      <transition name="stage">
        <section v-if="store.stage === 'downloading'" class="stage-view center" key="downloading">
          <div class="card progress-card">
            <div class="spinner"></div>
            <h2 class="h-title">正在下载图片…</h2>
            <p class="h-sub">已识别平台并开始抓取，请稍候</p>
          </div>
        </section>
      </transition>

      <!-- 步骤3：框选去水印 -->
      <transition name="stage">
        <section v-if="store.stage === 'downloaded'" class="stage-view" key="downloaded">
          <div class="wm-layout">
            <div class="wm-left card">
              <WatermarkCanvas :src="previewSrc" :width="previewItem?.width || 0" :height="previewItem?.height || 0" />
            </div>
            <div class="wm-right">
              <div class="card opts card-pad">
                <div class="opt-row">
                  <span class="opt-label">定位模式</span>
                  <label class="radio">
                    <input type="radio" value="absolute" v-model="store.mode" />
                    绝对位置
                  </label>
                  <label class="radio">
                    <input type="radio" value="relative" v-model="store.mode" />
                    按比例适配
                  </label>
                </div>
                <div class="opt-row">
                  <span class="opt-label">处理模式</span>
                  <label class="radio">
                    <input type="radio" value="auto" v-model="store.strategy" />
                    智能
                  </label>
                  <label class="radio">
                    <input type="radio" value="original" v-model="store.strategy" />
                    整图
                  </label>
                  <label class="radio">
                    <input type="radio" value="crop" v-model="store.strategy" />
                    快速
                  </label>
                </div>
                <p class="tip">智能=自动在整图修复与框周边裁剪间选择，兼顾速度与效果（默认）；整图=整图单次修复，最稳但大图慢；快速=只计算框选周边，大图最快。</p>
                <div class="opt-row">
                  <span class="opt-label">边缘外扩</span>
                  <input type="number" min="0" max="80" v-model.number="store.dilate" class="input num" />
                  <span class="opt-unit">px</span>
                </div>
                <div class="opt-row btns">
                  <button
                    class="btn btn-primary"
                    :disabled="store.running || !store.boxes.length || !engineReady"
                    @click="startBatch(true)"
                  >
                    全部去水印
                  </button>
                  <button
                    class="btn"
                    :disabled="store.selected.length === 0 || !store.boxes.length || !engineReady"
                    @click="startBatch(false)"
                  >
                    仅勾选({{ store.selected.length }})
                  </button>
                  <button
                    class="btn"
                    :disabled="!store.boxes.length || !engineReady"
                    @click="enqueueInpaint(store.selected.length === 0)"
                    title="固化当前框选参数，加入任务队列后台执行（不打断当前页面）"
                  >
                    + 加入队列
                  </button>
                  <button class="btn btn-danger" :disabled="!store.running" @click="cancelBatch">取消</button>
                  <button class="btn" :disabled="store.running" @click="exportSourceZip" title="把未去水印的原图打包为 zip 导出">
                    ⬇ 源图打包
                  </button>
                  <button
                    v-if="hasBGM"
                    class="btn"
                    :class="{ 'bgm-done': !!store.bgmSaved }"
                    :disabled="store.running"
                    @click="openBGM"
                    :title="store.post.audioName ? `曲目：${store.post.audioName}（可单独下载，也可放入源图 zip）` : '下载该图文的背景音乐'"
                  >
                    {{ store.bgmSaved ? '🎵 BGM 已下载' : '🎵 下载BGM' }}
                  </button>
                </div>
                <!-- BGM 试听：显示条件与上方「下载BGM」按钮完全一致（同一个 hasBGM） -->
                <div v-if="hasBGM" class="bgm-player">
                  <button
                    class="play-btn"
                    :disabled="store.running || bgmPlayer.loading"
                    :title="bgmPlayer.playing ? '暂停试听' : '试听背景音乐'"
                    @click="toggleAudition"
                  >
                    {{ bgmPlayer.loading ? '⏳' : bgmPlayer.playing ? '❚❚' : '▶' }}
                  </button>
                  <div class="player-mid">
                    <div class="player-meta">
                      <span class="player-name">{{ store.post.audioName || '背景音乐' }}</span>
                      <span class="player-time">{{ fmtTime(bgmPlayer.cur) }} / {{ fmtTime(bgmPlayer.dur) }}</span>
                    </div>
                    <div
                      class="player-track"
                      :class="{ 'is-live': bgmPlayer.dur > 0 }"
                      title="点击跳转播放位置"
                      @click="seekAudition"
                    >
                      <div class="player-fill" :style="{ width: bgmPct + '%' }"></div>
                    </div>
                  </div>
                  <span v-if="bgmPlayer.error" class="player-err">{{ bgmPlayer.error }}</span>
                </div>
                <p class="tip">可拖拽框选多个水印区域；框内按住拖动可整体移动，框边贴到图像边缘会自动吸附对齐；点区域 ✕ 删除单个，点「清空」全部删除。</p>
                <p v-if="!engineReady" class="tip engine-status">
                  {{ store.engineStatus === 'error' ? '⚠️' : '⏳' }}
                  {{ store.engineMessage || 'AI 引擎启动中…' }}
                </p>
              </div>
              <div class="log">
                <div v-for="(l, i) in store.logs.slice(-80)" :key="i" :class="l.cls">{{ l.line }}</div>
              </div>
            </div>
          </div>
          <div class="card grid-wrap card-pad">
            <div class="grid-head">
              <span class="grid-title">已下载 {{ store.sourceThumbs.length }} 张（点左上角 ☑ 勾选；按住 fn / Shift 再点一张＝选中区间；已选中的图可点「✕ 取消」或右键取消；点图片预览）</span>
              <button class="btn btn-ghost" @click="store.stage = 'idle'">换一个链接</button>
            </div>
            <ImageGrid
              :images="store.sourceThumbs"
              selectable
              :active-path="previewItem?.path || ''"
              @preview="onPreview"
              @range="onRange"
              @deselect-all="onDeselectAll"
            />
          </div>
        </section>
      </transition>

      <!-- 步骤4：处理中 -->
      <transition name="stage">
        <section v-if="store.stage === 'inpainting'" class="stage-view center" key="inpainting">
          <div class="card progress-card">
            <div class="spinner"></div>
            <h2 class="h-title">LaMa 正在修复…</h2>
            <p v-if="pageTask && pageTask.state === 'pending'" class="h-sub">
              当前任务排队中，前方还有 {{ queuedAhead }} 个任务，完成后自动开始…
            </p>
            <p v-else class="h-sub">
              {{ store.progress.index }}/{{ store.progress.total }} · {{ store.progress.name }}
            </p>
            <div class="progress wide">
              <div class="bar" :style="{ width: pct + '%' }"></div>
            </div>
          </div>
        </section>
      </transition>

      <!-- 步骤5：结果与导出 -->
      <transition name="stage">
        <section v-if="store.stage === 'inpainted'" class="stage-view" key="inpainted">
          <div class="card grid-wrap card-pad">
            <div class="grid-head">
              <span class="grid-title">去水印结果（左右滑动查看）</span>
              <div class="grid-actions">
                <button class="btn btn-ghost" @click="backToAdjust">返回调整</button>
                <button class="btn btn-primary" @click="exportZip">导出 zip 压缩包</button>
              </div>
            </div>
            <ImageGrid :images="store.cleanedThumbs" zoomable @view="viewImage" />
          </div>
        </section>
      </transition>
    </main>

    <!-- 底部签名 + 免责声明 -->
    <footer class="footer">
      <p class="disclaimer">
        免责声明：本工具仅用于处理您拥有合法权利的内容。使用者需自行承担因使用本工具所产生的一切风险、责任与法律后果；开发者不对任何侵权、违规或不当使用行为承担责任。
      </p>
      <p class="footer-sign">
        <span>作者 csy</span>
        <template v-if="appVersion">
          <span class="sign-sep">·</span>
          <span class="version-tag" :title="`当前版本 v${appVersion}（点「更新明细」查看本次更新内容）`">v{{ appVersion }}</span>
        </template>
      </p>
    </footer>

    <!-- 放大查看浮层 -->
    <transition name="fade">
      <div v-if="store.viewer.open" class="viewer" @click="closeViewer">
        <div class="viewer-bar">
          <span class="viewer-name">{{ store.viewer.name }}</span>
          <span v-if="store.viewerList.length > 1 && store.viewerIndex >= 0" class="viewer-counter">
            {{ store.viewerIndex + 1 }} / {{ store.viewerList.length }}
          </span>
          <button class="viewer-close" @click.stop="closeViewer">✕</button>
        </div>
        <button
          v-if="store.viewerList.length > 1 && store.viewerIndex >= 0"
          class="viewer-nav prev"
          title="上一张（←）"
          @click.stop="viewerNav(-1)"
        >‹</button>
        <div v-if="store.viewer.loading" class="spinner"></div>
        <img
          v-else
          :src="store.viewer.src"
          class="viewer-img"
          @click.stop
          draggable="false"
        />
        <button
          v-if="store.viewerList.length > 1 && store.viewerIndex >= 0"
          class="viewer-nav next"
          title="下一张（→）"
          @click.stop="viewerNav(1)"
        >›</button>
      </div>
    </transition>

    <!-- 注意事项弹窗 -->
    <transition name="fade">
      <div v-if="showNotice" class="notice-mask" @click.self="showNotice = false">
        <div class="card notice-panel">
          <div class="notice-head">
            <span class="notice-title">⚠️ 使用注意事项</span>
            <button class="viewer-close" @click="showNotice = false">✕</button>
          </div>
          <div class="notice-body">
            <h4>一、版权与知识产权</h4>
            <p>本工具仅限用于处理您本人拥有著作权、或已获权利人明确授权的图片。水印往往承载署名与权利标识，未经授权去除他人图片水印，可能构成对署名权、修改权、复制权等著作权的侵害；对结果图片进行传播或商用，侵权风险更直接。</p>

            <h4>二、平台用户协议</h4>
            <p>抓取并去除微信公众号、小红书、抖音等平台图片的水印，通常违反相应平台的《用户协议》与《社区规范》，平台可能据此限制账号功能、下架内容或追究相关责任。</p>

            <h4>三、肖像权、商标权与隐私</h4>
            <p>图片可能涉及他人肖像、商标或个人信息，处理与传播前请确认已取得必要授权，避免侵犯他人合法权益。</p>

            <h4>四、技术措施与法律责任</h4>
            <p>在部分情形下，去除数字水印或技术标识可能触发关于"规避技术措施"的法律规定，需自行评估并承担相应风险。</p>

            <h4>五、风险自担</h4>
            <p class="notice-strong">使用者需自行承担因使用本工具所产生的一切风险、责任与法律后果；开发者不对任何侵权、违规或不当使用行为承担责任。</p>
          </div>
          <div class="notice-foot">
            <button class="btn btn-primary" @click="showNotice = false">我已了解并同意</button>
          </div>
        </div>
      </div>
    </transition>

    <!-- 版本更新明细弹窗 -->
    <transition name="fade">
      <div v-if="showChangelog" class="notice-mask" @click.self="showChangelog = false">
        <div class="card notice-panel changelog-panel">
          <div class="notice-head">
            <span class="notice-title">🆕 版本更新明细</span>
            <button class="viewer-close" @click="showChangelog = false">✕</button>
          </div>
          <div class="notice-body changelog-body">
            <p v-if="appVersion" class="changelog-current">
              当前版本 <b>v{{ appVersion }}</b>
            </p>
            <div v-for="(rel, i) in changelogList" :key="rel.version" class="rel">
              <div class="rel-head">
                <span class="rel-ver">v{{ rel.version }}</span>
                <span v-if="i === 0" class="rel-badge">当前</span>
                <span class="rel-title">{{ rel.title }}</span>
                <span v-if="rel.date" class="rel-date">{{ rel.date }}</span>
              </div>
              <ul class="rel-items">
                <li v-for="(it, j) in rel.items" :key="j">
                  <span class="rel-kind" :class="`kind-${it.kind}`">{{ kindLabel(it.kind) }}</span>
                  <span class="rel-text">{{ it.text }}</span>
                </li>
              </ul>
            </div>
          </div>
          <div class="notice-foot">
            <button class="btn btn-primary" @click="showChangelog = false">知道了</button>
          </div>
        </div>
      </div>
    </transition>

    <!-- 抖音 Cookie 设置弹窗 -->
    <transition name="fade">
      <div v-if="showCookie" class="notice-mask" @click.self="showCookie = false">
        <div class="card notice-panel cookie-panel">
          <div class="notice-head">
            <span class="notice-title">🍪 抖音 Cookie 设置</span>
            <button class="viewer-close" @click="showCookie = false">✕</button>
          </div>
          <div class="notice-body">
            <p>抖音风控较强，未登录状态下解析经常被拦截。配置浏览器 Cookie 可显著提高成功率（仅保存在本机）：</p>
            <ol class="cookie-steps">
              <li>用浏览器打开 <b>www.douyin.com</b> 并登录账号</li>
              <li>按 <b>F12</b> 打开开发者工具，切到 <b>Network（网络）</b> 标签</li>
              <li>刷新页面，点击列表中第一个请求，查看 <b>Headers</b></li>
              <li>在 <b>Request Headers</b> 里找到 <b>Cookie</b> 一行，右键复制完整值，粘贴到下方</li>
            </ol>
            <textarea
              v-model="cookieInput"
              class="cookie-input"
              rows="5"
              placeholder="粘贴 Cookie：ttwid=xxx; msToken=xxx; ..."
              spellcheck="false"
            ></textarea>
          </div>
          <div class="notice-foot cookie-foot">
            <button class="btn cookie-clear" :disabled="cookieSaving" @click="clearCookie">清除</button>
            <span class="cookie-spacer"></span>
            <button class="btn" :disabled="cookieSaving" @click="showCookie = false">取消</button>
            <button class="btn btn-primary" :disabled="cookieSaving" @click="saveCookie">
              {{ cookieSaving ? '保存中…' : '保存' }}
            </button>
          </div>
        </div>
      </div>
    </transition>

    <!-- BGM 保存弹窗 -->
    <transition name="fade">
      <div v-if="showBGM" class="notice-mask" @click.self="showBGM = false">
        <div class="card notice-panel bgm-panel">
          <div class="notice-head">
            <span class="notice-title">🎵 保存背景音乐</span>
            <button class="viewer-close" @click="showBGM = false">✕</button>
          </div>
          <div class="notice-body">
            <p v-if="store.post?.audioName" class="bgm-track">曲目：{{ store.post.audioName }}</p>
            <p>保存文件名（不输入即默认 <b>bgm</b>，按实际音频格式补扩展名）：</p>
            <input
              v-model="bgmName"
              class="input bgm-input"
              placeholder="bgm"
              spellcheck="false"
              @keyup.enter="confirmBGM"
            />
            <p class="bgm-label">保存方式：</p>
            <label class="bgm-mode">
              <input type="radio" value="standalone" v-model="bgmMode" />
              <span>单独下载（另存为…）<em>独立文件，不进入源图 zip</em></span>
            </label>
            <label class="bgm-mode">
              <input type="radio" value="bundle" v-model="bgmMode" />
              <span>放入源图目录<em>随「源图打包」一起压缩进 zip</em></span>
            </label>
            <p v-if="store.bgmSaved" class="bgm-saved-tip">
              已保存：{{ store.bgmSaved }}（{{ store.bgmMode === 'bundle' ? '在源图目录内，会随 zip 打包' : '独立文件，不在 zip 内' }}；重复确定将覆盖）
            </p>
          </div>
          <div class="notice-foot">
            <button class="btn" :disabled="bgmSaving" @click="showBGM = false">取消</button>
            <button class="btn btn-primary" :disabled="bgmSaving" @click="confirmBGM">
              {{ bgmSaving ? '下载中…' : '确定' }}
            </button>
          </div>
        </div>
      </div>
    </transition>

    <!-- 任务队列弹窗（复用 notice-* 弹窗结构，PRD §2.2 入口二） -->
    <transition name="fade">
      <div v-if="showQueue" class="notice-mask" @click.self="showQueue = false">
        <div class="card notice-panel queue-panel">
          <div class="notice-head">
            <span class="notice-title">🗂 任务队列</span>
            <button class="viewer-close" @click="showQueue = false">✕</button>
          </div>
          <div class="notice-body queue-body">
            <!-- 空队列：只渲染一个全宽空态，分组头/清空按钮整体隐藏，避免空分组挤占布局 -->
            <div v-if="!store.queue.length" class="queue-empty">
              <div class="qe-icon">🗂</div>
              <p>队列为空</p>
              <p class="qe-tip">可在首页批量粘贴链接加入下载队列，<br>或在框选页点「+ 加入队列」后台执行去水印</p>
            </div>
            <template v-else>
              <div v-for="sec in queueSections" :key="sec.key" class="queue-section">
                <div class="qs-head">
                  <span class="qs-title">{{ sec.title }}（{{ sec.tasks.length }}）</span>
                  <button class="btn btn-mini" @click="clearFinishedQueue(sec.key)">清空已完成</button>
                </div>
                <p v-if="!sec.tasks.length" class="queue-empty qs-empty">暂无任务</p>
                <div v-for="t in sec.tasks" :key="t.id" class="queue-item">
              <div class="qi-head">
                <span class="qi-type" :class="'tp-' + t.type">{{ typeMeta[t.type] || t.type }}</span>
                <span class="qi-title" :title="t.type === 'download' ? t.url : `${t.inDir} → ${t.outDir || '…'}`">
                  {{ taskDisplayName(t) }}
                </span>
                <span class="qi-state" :class="stateMeta[t.state]?.cls">{{ stateMeta[t.state]?.label || t.state }}</span>
              </div>
              <div v-if="t.state === 'running' && t.progress" class="qi-progress">
                <div class="qi-track">
                  <div
                    class="qi-fill"
                    :style="{ width: (t.progress.total ? Math.round((t.progress.index / t.progress.total) * 100) : 0) + '%' }"
                  ></div>
                </div>
                <span class="qi-pinfo">{{ t.progress.index }}/{{ t.progress.total }} · {{ t.progress.name }}</span>
              </div>
              <div v-if="t.summary" class="qi-summary">{{ t.summary }}</div>
              <div v-if="t.err" class="qi-err" :title="t.err">✗ {{ t.err }}</div>
              <ul v-if="t.failList && t.failList.length" class="qi-fails">
                <li v-for="(f, i) in t.failList.slice(0, 5)" :key="i">{{ f }}</li>
                <li v-if="t.failList.length > 5" class="qi-more">… 共 {{ t.failList.length }} 条</li>
              </ul>
              <div v-if="t.attempts > 0 || t.nextRetryAt" class="qi-meta">
                <span v-if="t.attempts > 0">已自动重试 {{ t.attempts }} 次</span>
                <span v-if="t.nextRetryAt">将于 {{ fmtTs(t.nextRetryAt) }} 自动重试</span>
              </div>
              <div class="qi-actions">
                <button v-if="t.state === 'done' && t.type === 'download'" class="btn btn-mini" @click="loadTaskMaterial(t)">
                  去框选
                </button>
                <button v-if="t.state === 'done' && t.type === 'inpaint'" class="btn btn-mini" @click="openTaskResult(t)">
                  查看结果
                </button>
                <button
                  v-if="t.state === 'running' || t.state === 'pending' || t.state === 'retry_wait'"
                  class="btn btn-mini"
                  @click="cancelQueueTask(t.id)"
                >
                  取消
                </button>
                <button v-if="t.state === 'failed' || t.state === 'canceled'" class="btn btn-mini" @click="retryQueueTask(t.id)">
                  重试
                </button>
                <button
                  v-if="t.state === 'done' || t.state === 'failed' || t.state === 'canceled'"
                  class="btn btn-mini"
                  @click="removeQueueTask(t.id)"
                >
                  移除
                </button>
              </div>
              </div>
            </div>
            </template>
          </div>
          <div class="notice-foot queue-foot">
            <span class="qi-spacer"></span>
            <button class="btn btn-primary" @click="showQueue = false">关闭</button>
          </div>
        </div>
      </div>
    </transition>

    <!-- Toast -->
    <transition name="fade">
      <div v-if="store.toast" class="toast" :class="store.toastKind">{{ store.toast }}</div>
    </transition>
  </div>
</template>

<style scoped>
.shell {
  height: 100%;
  display: flex;
  flex-direction: column;
  padding: 14px 16px;
  gap: 14px;
}

.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 10px 18px;
  flex-shrink: 0;
  gap: 16px;
  flex-wrap: wrap;
}
.brand { display: flex; align-items: center; gap: 12px; }
.logo {
  width: 40px; height: 40px;
  border-radius: 11px;
  background: linear-gradient(135deg, #0a84ff, #5e5ce6);
  color: #fff;
  font-size: 12px; font-weight: 700;
  display: flex; align-items: center; justify-content: center;
  letter-spacing: 0.5px;
  box-shadow: 0 3px 10px rgba(10, 132, 255, 0.35);
}
.title-row { display: flex; align-items: center; gap: 10px; }
.title { font-size: 15.5px; font-weight: 650; letter-spacing: 0.2px; }
.subtitle { font-size: 11.5px; color: var(--text-2); margin-top: 2px; }

.notice-btn {
  appearance: none;
  border: none;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 11.5px;
  font-weight: 500;
  font-family: inherit;
  color: var(--warn);
  background: transparent;
  padding: 3px 10px;
  border-radius: 999px;
  cursor: pointer;
  box-shadow: inset 0 0 0 1px rgba(255, 159, 10, 0.45);
  transition: background 0.2s var(--spring), transform 0.15s var(--spring);
  white-space: nowrap;
}
.notice-btn:hover { background: rgba(255, 159, 10, 0.14); }
.notice-btn:active { transform: scale(0.96); }
.notice-icon { font-size: 12px; line-height: 1; }

/* 「更新明细」按钮：结构复用 .notice-btn，仅换主色（蓝）以区别于「注意事项」（橙） */
.changelog-btn {
  color: var(--accent);
  box-shadow: inset 0 0 0 1px rgba(10, 132, 255, 0.45);
}
.changelog-btn:hover { background: rgba(10, 132, 255, 0.14); }

/* 「任务队列」按钮：结构复用 .notice-btn，主色用青绿以区别于注意事项（橙）/更新明细（蓝） */
.queue-btn {
  color: #2bb3a3;
  box-shadow: inset 0 0 0 1px rgba(43, 179, 163, 0.45);
}
.queue-btn:hover { background: rgba(43, 179, 163, 0.14); }
.queue-badge {
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  margin-left: 2px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 10px;
  font-weight: 700;
  color: #fff;
  background: #2bb3a3;
  border-radius: 999px;
  line-height: 1;
}

/* 首页批量链接入口 */
.queue-row { display: flex; gap: 10px; margin-top: 12px; align-items: stretch; }
.queue-textarea {
  flex: 1;
  resize: vertical;
  min-height: 64px;
  font-size: 12px;
  line-height: 1.6;
  font-family: inherit;
  word-break: break-all;
}

/* 队列弹窗：左右双列（下载 | 去水印），窄窗口回退上下堆叠 */
.queue-panel { width: min(980px, 96vw); }
.queue-body {
  display: flex;
  gap: 14px;
  align-items: flex-start;
}
.queue-empty {
  color: var(--text-3);
  text-align: center;
  padding: 24px 0;
}
/* 空队列全宽空态 */
.queue-body > .queue-empty { flex: 1; padding: 48px 0; }
.queue-empty .qe-icon { font-size: 30px; margin-bottom: 8px; }
.queue-empty p { margin: 0; color: var(--text-2); font-size: 13.5px; }
.queue-empty .qe-tip { margin-top: 6px; font-size: 12px; color: var(--text-3); line-height: 1.8; }
@media (max-width: 880px) {
  .queue-body { flex-direction: column; }
}

/* 两类队列分组（下载 / 去水印独立展示与管理） */
.queue-section {
  flex: 1 1 0;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.qs-empty { padding: 12px 0; font-size: 12px; }
.qs-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 2px 2px 0;
}
.qs-title { font-size: 12.5px; font-weight: 650; color: var(--text-2); }

.queue-item {
  border: 1px solid var(--card-border);
  border-radius: 10px;
  padding: 10px 12px;
  background: rgba(128, 128, 128, 0.05);
}
.qi-head { display: flex; align-items: center; gap: 8px; min-width: 0; }
.qi-type {
  flex-shrink: 0;
  font-size: 10.5px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 999px;
  color: var(--accent);
  background: var(--accent-soft);
}
.qi-type.tp-inpaint { color: #9d6bff; background: rgba(157, 107, 255, 0.14); }
.qi-title {
  flex: 1;
  min-width: 0;
  font-size: 12.5px;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.qi-state {
  flex-shrink: 0;
  font-size: 11px;
  font-weight: 600;
  padding: 1px 9px;
  border-radius: 999px;
}
.st-pending { color: var(--text-2); background: rgba(128, 128, 128, 0.16); }
.st-running { color: var(--accent); background: var(--accent-soft); }
.st-retry { color: var(--warn); background: rgba(255, 159, 10, 0.14); }
.st-done { color: var(--ok, #30d158); background: rgba(48, 209, 88, 0.14); }
.st-failed { color: var(--danger); background: var(--danger-soft); }
.st-cancel { color: var(--text-3); background: rgba(128, 128, 128, 0.12); }
.qi-progress { display: flex; align-items: center; gap: 10px; margin-top: 8px; }
.qi-track {
  flex: 1;
  height: 5px;
  border-radius: 999px;
  background: rgba(128, 128, 128, 0.22);
  overflow: hidden;
}
.qi-fill { height: 100%; border-radius: 999px; background: var(--accent); transition: width 0.15s linear; }
.qi-pinfo {
  flex-shrink: 0;
  font-size: 11px;
  color: var(--text-3);
  max-width: 55%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-variant-numeric: tabular-nums;
}
.qi-summary { margin-top: 6px; font-size: 11.5px; color: var(--text-2); }
.qi-err {
  margin-top: 6px;
  font-size: 11.5px;
  color: var(--danger);
  word-break: break-all;
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.qi-fails {
  margin: 6px 0 0;
  padding-left: 18px;
  font-size: 11px;
  color: var(--text-3);
  line-height: 1.6;
}
.qi-meta { margin-top: 6px; display: flex; gap: 12px; font-size: 11px; color: var(--text-3); }
.qi-actions { margin-top: 8px; display: flex; gap: 8px; justify-content: flex-end; }
.btn-mini { padding: 3px 12px; font-size: 11.5px; }
.queue-foot { justify-content: flex-start; gap: 8px; }
.queue-foot .qi-spacer { flex: 1; }

.content { flex: 1; position: relative; overflow: hidden; }
.stage-view { height: 100%; overflow-y: auto; display: flex; flex-direction: column; gap: 14px; }
.stage-view.center { align-items: center; justify-content: center; }

.footer {
  flex-shrink: 0;
  text-align: center;
  font-size: 11.5px;
  color: var(--text-3);
  letter-spacing: 0.4px;
  padding: 2px 0 0;
  user-select: none;
}
.footer .disclaimer {
  font-size: 11px;
  color: var(--text-3);
  line-height: 1.6;
  max-width: 820px;
  margin: 0 auto 2px;
}
.footer .footer-sign {
  font-size: 11.5px;
  color: var(--text-3);
  letter-spacing: 0.4px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
}
.footer .sign-sep { color: var(--text-3); opacity: 0.7; }
/* 版本号徽标：与签名同色系、略作区分，不抢免责声明的视觉重心 */
.footer .version-tag {
  font-variant-numeric: tabular-nums;
  padding: 1px 8px;
  border-radius: 999px;
  color: var(--text-2);
  box-shadow: inset 0 0 0 1px var(--card-border);
  cursor: default;
}

/* 版本更新明细弹窗 */
.changelog-panel { width: min(640px, 94vw); }
.changelog-current {
  margin-bottom: 14px;
  color: var(--text-2);
}
.changelog-current b { color: var(--accent); }
.changelog-body .rel { margin-bottom: 18px; }
.changelog-body .rel:last-child { margin-bottom: 0; }
.changelog-body .rel-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 7px;
}
.changelog-body .rel-ver {
  font-size: 13px;
  font-weight: 650;
  color: var(--text);
  font-variant-numeric: tabular-nums;
}
.changelog-body .rel-badge {
  font-size: 10.5px;
  font-weight: 600;
  color: #fff;
  background: var(--accent);
  padding: 1px 7px;
  border-radius: 999px;
}
.changelog-body .rel-title { font-size: 12.5px; color: var(--text-2); }
.changelog-body .rel-date {
  margin-left: auto;
  font-size: 11.5px;
  color: var(--text-3);
  font-variant-numeric: tabular-nums;
}
.changelog-body .rel-items {
  list-style: none;
  margin: 0;
  padding: 0;
}
.changelog-body .rel-items li {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  padding: 5px 0;
  font-size: 12.5px;
  line-height: 1.65;
}
/* 类型标记：定宽对齐，使多条更新条目的正文左边缘齐平 */
.changelog-body .rel-kind {
  flex-shrink: 0;
  width: 34px;
  text-align: center;
  font-size: 11px;
  font-weight: 600;
  line-height: 1.55;
  padding: 1px 0;
  border-radius: 6px;
  color: var(--text-2);
  background: rgba(128, 128, 128, 0.14);
}
.changelog-body .kind-new { color: var(--accent); background: var(--accent-soft); }
.changelog-body .kind-fix { color: var(--danger); background: var(--danger-soft); }
.changelog-body .kind-opt { color: var(--ok, #30d158); background: rgba(48, 209, 88, 0.14); }
.changelog-body .kind-change { color: var(--warn); background: rgba(255, 159, 10, 0.14); }
.changelog-body .rel-text { flex: 1; min-width: 0; }

/* 注意事项弹窗 */
.notice-mask {
  position: fixed;
  inset: 0;
  z-index: 300;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.55);
  backdrop-filter: blur(10px);
  -webkit-backdrop-filter: blur(10px);
  padding: 20px;
}
.notice-panel {
  width: min(620px, 94vw);
  max-height: 82vh;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  background: var(--card-solid);
  border: 1px solid var(--card-border);
  box-shadow: var(--shadow-lg);
}
.notice-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 20px;
  border-bottom: 1px solid var(--card-border);
}
.notice-title { font-size: 15px; font-weight: 650; color: var(--text); }
.notice-body {
  padding: 16px 20px;
  overflow-y: auto;
  font-size: 13px;
  line-height: 1.7;
  color: var(--text-2);
}
.notice-body h4 { font-size: 13px; font-weight: 650; color: var(--text); margin: 12px 0 4px; }
.notice-body h4:first-child { margin-top: 0; }
.notice-body p { margin: 0; }
.notice-body .notice-strong { color: var(--danger); font-weight: 600; }
.notice-foot {
  padding: 14px 20px;
  border-top: 1px solid var(--card-border);
  display: flex;
  justify-content: flex-end;
}

.h-title { font-size: 19px; font-weight: 650; margin-bottom: 6px; }
.h-sub { font-size: 13px; color: var(--text-2); margin-bottom: 16px; }

.url-card { width: min(680px, 92%); padding: 30px 34px; text-align: center; }
.url-row { display: flex; gap: 10px; }
.url-row .input { flex: 1; }
.local-row { display: flex; align-items: center; justify-content: center; gap: 10px; margin-top: 16px; }
.local-divider { height: 1px; width: 72px; background: var(--card-border); }
.local-label { font-size: 12px; color: var(--text-3); }
.local-tip { font-size: 12px; color: var(--text-3); }

/* 抖音 Cookie 设置 */
.cookie-btn {
  border: none;
  font-family: inherit;
  box-shadow: inset 0 0 0 1px rgba(10, 132, 255, 0.45);
  color: var(--accent);
  background: transparent;
}
.cookie-btn:hover { background: rgba(10, 132, 255, 0.14); }
.cookie-panel { width: min(560px, 94vw); }
.cookie-steps {
  margin: 10px 0 0;
  padding-left: 22px;
  color: var(--text-3);
  font-size: 12.5px;
  line-height: 1.9;
}
.cookie-input {
  width: 100%;
  margin-top: 12px;
  padding: 10px 12px;
  border-radius: 10px;
  border: 1px solid var(--card-border);
  background: rgba(128, 128, 128, 0.08);
  color: var(--text);
  font-size: 12px;
  font-family: ui-monospace, Consolas, monospace;
  line-height: 1.5;
  resize: vertical;
  box-sizing: border-box;
  word-break: break-all;
}
.cookie-input:focus { outline: none; border-color: var(--accent); }
.cookie-foot { justify-content: flex-start; gap: 8px; }
.cookie-foot .cookie-clear { color: var(--danger); }
.cookie-foot .cookie-spacer { flex: 1; }

/* BGM 保存弹窗 */
.bgm-panel { width: min(440px, 94vw); }
.bgm-panel .notice-body p { margin: 8px 0 0; color: var(--text-2); font-size: 12.5px; }
.bgm-track { color: var(--text) !important; font-weight: 600; }
.bgm-input { margin-top: 8px; }
.bgm-panel .notice-body .bgm-label { margin-top: 14px; }
.bgm-mode {
  display: flex;
  align-items: flex-start;
  gap: 7px;
  margin-top: 7px;
  font-size: 12.5px;
  color: var(--text);
  cursor: pointer;
}
.bgm-mode input { margin-top: 2px; accent-color: var(--accent); flex-shrink: 0; }
.bgm-mode em {
  display: block;
  font-style: normal;
  font-size: 11.5px;
  color: var(--text-3);
  margin-top: 1px;
}
.bgm-saved-tip { color: var(--text-3) !important; font-size: 11.5px !important; word-break: break-all; }
.bgm-done { color: var(--ok, #30d158); }

/* 第三步 BGM 试听条 */
.bgm-player {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin: 4px 0 10px;
  padding: 8px 10px;
  border-radius: var(--radius-sm);
  background: rgba(128, 128, 128, 0.08);
  box-shadow: inset 0 0 0 1px var(--card-border);
}
.play-btn {
  appearance: none;
  border: none;
  flex-shrink: 0;
  width: 30px;
  height: 30px;
  border-radius: 50%;
  background: var(--accent);
  color: #fff;
  font-size: 11px;
  line-height: 1;
  font-family: inherit;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  transition: transform 0.15s var(--spring), opacity 0.2s var(--spring);
}
.play-btn:hover:not(:disabled) { transform: scale(1.06); }
.play-btn:active:not(:disabled) { transform: scale(0.94); }
.play-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.player-mid { flex: 1; min-width: 150px; display: flex; flex-direction: column; gap: 5px; }
.player-meta { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.player-name {
  font-size: 12px;
  color: var(--text-2);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.player-time {
  flex-shrink: 0;
  font-size: 11.5px;
  color: var(--text-3);
  font-variant-numeric: tabular-nums;
}
.player-track {
  height: 5px;
  border-radius: 999px;
  background: rgba(128, 128, 128, 0.22);
  overflow: hidden;
  cursor: default;
}
.player-track.is-live { cursor: pointer; }
.player-fill {
  height: 100%;
  width: 0;
  border-radius: 999px;
  background: var(--accent);
  transition: width 0.12s linear;
}
.player-err {
  width: 100%;
  font-size: 11.5px;
  color: var(--danger);
  word-break: break-all;
}
.platforms { margin-top: 18px; display: flex; gap: 8px; justify-content: center; }
.chip {
  font-size: 12px; color: var(--text-2);
  padding: 5px 12px; border-radius: 999px;
  background: var(--card-solid);
  box-shadow: inset 0 0 0 1px var(--card-border);
}

.progress-card { padding: 34px 46px; text-align: center; display: flex; flex-direction: column; align-items: center; }
.progress-card .btn-danger { margin-top: 18px; }
.spinner {
  width: 34px; height: 34px;
  border-radius: 50%;
  border: 3px solid var(--accent-soft);
  border-top-color: var(--accent);
  animation: spin 0.9s linear infinite;
  margin-bottom: 14px;
}
@keyframes spin { to { transform: rotate(360deg); } }
.progress.wide { width: 300px; margin-top: 14px; }

.wm-layout { display: flex; gap: 14px; align-items: flex-start; }
.wm-left { padding: 16px; flex-shrink: 0; }
.wm-right { flex: 1; display: flex; flex-direction: column; gap: 12px; min-width: 0; }
.opts { padding: 14px 16px; }
.opt-row { display: flex; align-items: center; gap: 10px; margin-bottom: 10px; flex-wrap: wrap; }
.opt-row.btns { margin-bottom: 4px; }
.opt-label { font-size: 13px; color: var(--text-2); width: 62px; flex-shrink: 0; }
.opt-unit { font-size: 12px; color: var(--text-3); }
.radio { font-size: 13px; display: flex; align-items: center; gap: 5px; cursor: pointer; }
.radio input { accent-color: var(--accent); }
.num { width: 74px; padding: 7px 10px; }
.tip { font-size: 11.5px; color: var(--text-3); }
.engine-status {
  margin-top: 6px;
  color: var(--warn);
  font-weight: 500;
}

.grid-wrap { padding: 16px; }
.grid-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 12px; }
.grid-title { font-size: 13.5px; font-weight: 600; color: var(--text-2); }
.grid-actions { display: flex; gap: 8px; }

/* 放大查看浮层 */
.viewer {
  position: fixed;
  inset: 0;
  z-index: 200;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.78);
  backdrop-filter: blur(8px);
  -webkit-backdrop-filter: blur(8px);
  cursor: zoom-out;
  animation: viewer-in 0.25s var(--spring);
}
.viewer-bar {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 18px;
  color: #fff;
}
.viewer-name {
  font-size: 13px;
  opacity: 0.85;
  max-width: 60%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.viewer-counter {
  font-size: 12.5px;
  opacity: 0.75;
  margin-left: 12px;
  font-variant-numeric: tabular-nums;
}
.viewer-nav {
  position: absolute;
  top: 50%;
  transform: translateY(-50%);
  z-index: 2;
  width: 44px;
  height: 44px;
  border: none;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.16);
  color: #fff;
  font-size: 28px;
  line-height: 0.9;
  cursor: pointer;
  transition: background 0.2s var(--spring), transform 0.2s var(--spring);
}
.viewer-nav:hover { background: rgba(255, 255, 255, 0.3); transform: translateY(-50%) scale(1.08); }
.viewer-nav.prev { left: 18px; }
.viewer-nav.next { right: 18px; }
.viewer-close {
  appearance: none;
  border: none;
  background: rgba(255, 255, 255, 0.16);
  color: #fff;
  width: 34px;
  height: 34px;
  border-radius: 50%;
  font-size: 15px;
  cursor: pointer;
  transition: background 0.2s var(--spring), transform 0.2s var(--spring);
}
.viewer-close:hover { background: rgba(255, 255, 255, 0.3); transform: scale(1.06); }
.viewer-img {
  max-width: 92vw;
  max-height: 88vh;
  object-fit: contain;
  border-radius: 8px;
  box-shadow: 0 12px 50px rgba(0, 0, 0, 0.5);
  cursor: default;
}
@keyframes viewer-in {
  from { opacity: 0; }
  to { opacity: 1; }
}
</style>
