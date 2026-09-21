<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import {
  store,
  initEvents,
  startDownload,
  loadLocalFolder,
  startBatch,
  exportZip,
  exportSourceZip,
  saveBGM,
  audioCandidates,
  showToast,
  viewImage,
  viewerNav,
  closeViewer,
  refreshQueue,
  retryQueueTask,
  cancelQueueTask,
  removeQueueTask,
  clearGroupQueue,
  loadTaskMaterial,
  openTaskResult,
} from './store.js'
import { changelog, kindLabel } from './changelog.js'
import { GetAppVersion, GetBGMAudio, GetDouyinCookie, SetDouyinCookie, GetCacheUsage, ClearWorkspaceCache } from '../wailsjs/go/main/App'
import { BrowserOpenURL } from '../wailsjs/runtime/runtime'
import ImageGrid from './components/ImageGrid.vue'
import WatermarkCanvas from './components/WatermarkCanvas.vue'

// 作品页地址：左下角悬浮图标按钮的跳转目标（用系统默认浏览器打开）
const GITHUB_URL = 'https://github.com/chenchen-0212/lama-watermark-eraser'

// 用系统浏览器打开作品页。Wails 桌面端走运行时 API；
// 非 Wails 环境（如浏览器里跑 vite dev）回退到 window.open，便于本地调试。
function openGithub() {
  try {
    BrowserOpenURL(GITHUB_URL)
  } catch {
    window.open(GITHUB_URL, '_blank', 'noopener')
  }
}

// 自动入队模式（方案 v1.2）：下载与去水印点击即入队，无等待页。
// stage 合法值收敛为 idle / downloaded / inpainted。
const steps = ['输入链接', '框选去水印', '结果导出']
const stepIdx = computed(() => ({ idle: 0, downloaded: 1, inpainted: 2 }[store.stage] ?? 0))

const previewItem = computed(() => {
  if (store.previewPath) {
    const hit = store.sourceThumbs.find((t) => t.path === store.previewPath)
    if (hit && hit.thumb) return hit
  }
  return store.sourceThumbs.find((t) => t.thumb) || null
})
const previewSrc = computed(() => previewItem.value?.thumb || '')

// 引擎 error 态拦截入队（starting 态允许排队，队列即等待）
const engineError = computed(() => store.engineStatus === 'error')

const urlRef = ref(null)
const showNotice = ref(false)

// ---------------- 版本信息 ----------------
// 底部签名处的版本号来自后端（构建时注入，与安装包版本同源）。
// 取不到时留空 —— 宁可只显示「作者 chenchen」，也不显示一个可能错误的版本号。
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

// ---------------- 任务队列面板 ----------------
// 面板展示队列任务列表；数据由 queue:updated 事件全量同步，
// 打开时主动拉取一次快照（弥补订阅前错过的事件）。
const showQueue = ref(false)

async function openQueue() {
  resetGroupCollapse() // 每次打开都回到全展开（见 collapsedGroups 的说明）
  showQueue.value = true
  await refreshQueue()
}

// 待处理任务数（顶栏角标：pending + running + retry_wait）
const queueActive = computed(
  () => store.queue.filter((t) => ['pending', 'running', 'retry_wait'].includes(t.state)).length
)

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

// ---------------- 任务列表按时间分组（借鉴 Windows「下载」文件夹） ----------------
// 分组口径：今天 / 昨天 / 更早。Windows 资源管理器按修改时间切「今天/昨天/
// 上周」等档位，这里沿用同一心智：**只按自然日切分**，不引入「上周」这种
// 跨度不确定的档位——队列任务通常当天产生，硬塞一个「上周」会让分组头
// 长期空着（空分组要么占版面、要么得额外处理，两头不讨好）。
//
// 关键：两件事必须同一口径，否则会出现「卡片写着昨天、却分在今天的组里」
//   - 分组归属时刻：终态用完成时刻，其余用创建时刻（与卡片时间显示一致）
//   - 区间边界：左闭右开，[当天 00:00, 次日 00:00)
// 后端 ClearGroupTasks 收到同一对边界，故「看到几条」与「清掉几条」必然相等。
//
// dayStart 取某时刻所在自然日的 00:00（本地时区）。
function dayStart(ts) {
  const d = new Date(ts)
  d.setHours(0, 0, 0, 0)
  return d.getTime()
}

// taskMoment 任务的分组归属时刻（毫秒）：终态优先完成时刻，否则创建时刻。
// 与 store 的 taskTime 文案语义严格对应；解析失败返回 0（归入「更早」）。
function taskMoment(t) {
  const terminal = ['done', 'failed', 'canceled'].includes(t.state)
  const raw = terminal && t.finishedAt ? t.finishedAt : t.createdAt
  const ms = raw ? new Date(raw).getTime() : 0
  return Number.isNaN(ms) ? 0 : ms
}

// 三个分组档位：[key, 标题, 起始偏移天数, 结束偏移天数)
// 结束用「次日 00:00」实现左闭右开：「昨天」= [昨天00:00, 今天00:00)。
// 「更早」的结束边界为 0（不限），即覆盖今天 00:00 之前的全部任务。
const GROUP_DEFS = [
  ['today', '今天'],
  ['yesterday', '昨天'],
  ['earlier', '更早'],
]

// 分组结果同时供模板渲染与清空按钮使用（含区间边界）
const queueGroups = computed(() => {
  const todayStart = dayStart(nowTs.value)
  const DAY = 86400000
  // 区间（毫秒）：today 与 yesterday 是确定边界；earlier 无起点
  const ranges = {
    today: [todayStart, todayStart + DAY],
    yesterday: [todayStart - DAY, todayStart],
    earlier: [0, todayStart],
  }
  const buckets = { today: [], yesterday: [], earlier: [] }
  // 先按作品合并，再按「分组归属时刻」入桶 —— 合并后的卡片只出现一次，
  // 不会因为两条任务时刻不同而同时落进两个分组。
  for (const c of mergeTasksByPost(store.queue)) {
    const at = c.order
    // 极少数脏数据（无任何时刻）归入「更早」，不让任务凭空消失
    if (!at || at < todayStart - DAY) buckets.earlier.push(c)
    else if (at < todayStart) buckets.yesterday.push(c)
    else buckets.today.push(c)
  }
  // 分组内按作品最新活跃时刻倒序（合并时已排过，这里再兜一层）
  for (const k of Object.keys(buckets)) {
    buckets[k].sort((a, b) => b.order - a.order)
  }
  return GROUP_DEFS.map(([key, title]) => {
    const [since, until] = ranges[key]
    return {
      key,
      title,
      tasks: buckets[key],
      count: buckets[key].length,
      // 秒级时间戳传给后端清空（0 = 不限）；earlier 的 until 是今天 00:00
      sinceSec: Math.floor(since / 1000),
      untilSec: Math.floor(until / 1000),
    }
  })
})

// 有任务的分组才渲染（Windows 也是只显示非空档位）；「更早」为空时不显示
const visibleGroups = computed(() => queueGroups.value.filter((g) => g.count > 0))

// 分组收起/展开：**每次打开面板都默认全部展开**，不再跨会话记忆收起状态。
// 理由：收起态是「临时聚焦」的产物，而非用户的长期偏好；记忆它会让下次打开
// 面板时看不到任何任务，误以为队列被清空了 —— 默认展开更符合「打开就想看到全貌」。
// 收起能力本身保留（见 toggleGroup），只是不再持久化。
const collapsedGroups = ref(new Set())

// 打开队列面板时把收起态清零，保证「默认展开」
function resetGroupCollapse() {
  if (collapsedGroups.value.size) collapsedGroups.value = new Set()
}

function toggleGroup(key) {
  const s = new Set(collapsedGroups.value)
  if (s.has(key)) s.delete(key)
  else s.add(key)
  collapsedGroups.value = s
}

// 一键清空某分组：清空后该组任务数为 0，分组自动隐藏
// （后端会同步删除这些任务在工作区中的本地文件）
function clearGroup(g) {
  clearGroupQueue(g.sinceSec, g.untilSec)
}

// ---------------- 清除本地缓存 ----------------
// 语义：删除缓存目录中「未被任何任务记录引用」的文件（旧版本遗留的素材）。
// 仍被任务引用的目录不动——历史任务的「查看结果 / 去框选」还要读它们。
// 想连记录带文件一起清，走队列面板各分组的「清空」。
const showCacheClear = ref(false)
const clearingCache = ref(false)
const cacheUsage = reactive({
  loading: false,
  totalBytes: 0,
  freeBytes: 0,
  keptBytes: 0,
  keptDir: 0,
})

// 无可释放空间：目录为空/不存在，或全部文件都被任务记录引用
const cacheNothing = computed(() => !cacheUsage.loading && cacheUsage.freeBytes === 0)

// 字节数人类可读化。超过 1 GB 保留 1 位小数（928.4 MB 比 928 MB 更有信息量），
// 小体积取整避免出现「2.07 KB」这类无意义的精度。
function fmtSize(n) {
  const v = Number(n) || 0
  if (v <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let x = v
  while (x >= 1024 && i < units.length - 1) {
    x /= 1024
    i++
  }
  return `${i >= 2 ? x.toFixed(1) : Math.round(x)} ${units[i]}`
}

// 打开确认弹窗：先把后端统计拉到再显示，避免弹窗先弹出来再「跳数字」
async function openCacheClear() {
  if (clearingCache.value) return
  showCacheClear.value = true
  cacheUsage.loading = true
  Object.assign(cacheUsage, { totalBytes: 0, freeBytes: 0, keptBytes: 0, keptDir: 0 })
  try {
    const u = await GetCacheUsage()
    Object.assign(cacheUsage, {
      totalBytes: u?.totalBytes || 0,
      freeBytes: u?.freeBytes || 0,
      keptBytes: u?.keptBytes || 0,
      keptDir: u?.keptDir || 0,
    })
  } catch (e) {
    showToast('读取缓存信息失败：' + (e?.message || e), 'danger')
  } finally {
    cacheUsage.loading = false
  }
}

function closeCacheClear() {
  // 清除进行中不允许关闭：中途关掉会让人误以为操作被取消了
  if (clearingCache.value) return
  showCacheClear.value = false
}

// 执行清除：按结果分三种反馈
//   ① 有释放空间     → 成功提示带释放量
//   ② 无释放空间     → 中性提示「已是干净状态」，不报错
//   ③ 目录不存在/异常 → 失败提示带原因
async function doCacheClear() {
  if (clearingCache.value) return
  clearingCache.value = true
  try {
    const r = await ClearWorkspaceCache()
    if (r?.nothing) {
      showToast('缓存已是干净状态，无需清理', 'info')
    } else {
      const extra = r?.kept > 0 ? `，另有 ${r.kept} 项受任务记录保护已保留` : ''
      showToast(`已清除缓存，释放 ${fmtSize(r?.freedBytes)}（${r?.files || 0} 个文件）${extra}`, 'info')
    }
    showCacheClear.value = false
  } catch (e) {
    showToast('清除缓存失败：' + (e?.message || e), 'danger')
  } finally {
    clearingCache.value = false
  }
}

function fmtTs(ts) {
  if (!ts) return ''
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

// ---------------- 任务卡片时间展示 ----------------
// 卡片左下角显示「该任务的时间」，随状态切换语义（用户最关心的是「什么时候
// 完成的」而非「什么时候排的队」，故终态优先展示完成时刻）：
//   done/failed/canceled → 完成时刻
//   running              → 已执行时长（每秒刷新，给出「还要等多久」的实感）
//   pending / retry_wait → 创建时刻
// 时刻统一格式 YY-MM-DD HH:mm（24 小时制，零填充）——队列常跨会话，
// 固定带日期才不会把昨天的任务误读成今天的；等宽数字保证多卡片纵向对齐。
// 完整时刻（含秒与创建/开始时间）放在 title 悬浮提示里，不占版面。
const nowTs = ref(Date.now())
let nowTimer = null

// 1s 心跳驱动「已执行时长」刷新；组件卸载时清理
onMounted(() => {
  nowTimer = setInterval(() => {
    nowTs.value = Date.now()
  }, 1000)
})
onBeforeUnmount(() => {
  if (nowTimer) clearInterval(nowTimer)
})

// fmtDateTime 任务时刻：固定 YY-MM-DD HH:mm（24 小时制）
function fmtDateTime(ts) {
  if (!ts) return ''
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${String(d.getFullYear() % 100).padStart(2, '0')}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

// fmtDur 时长：秒级以下不显示，超过 1 小时进位到「x小时y分」
function fmtDur(ms) {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 60) return `${s}秒`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}分${String(s % 60).padStart(2, '0')}秒`
  return `${Math.floor(m / 60)}小时${String(m % 60).padStart(2, '0')}分`
}

// taskTime 返回该任务左下角应显示的时间文案（含图标语义前缀）
function taskTime(t) {
  const created = t.createdAt ? new Date(t.createdAt).getTime() : 0
  if (t.state === 'running') {
    const from = t.startedAt ? new Date(t.startedAt).getTime() : created
    if (!from) return ''
    return `⏱ 已执行 ${fmtDur(nowTs.value - from)}`
  }
  if (t.state === 'done' || t.state === 'failed' || t.state === 'canceled') {
    const fin = t.finishedAt ? fmtDateTime(t.finishedAt) : ''
    if (fin) return `✓ ${fin} 完成`
    return created ? `＋ ${fmtDateTime(created)} 创建` : ''
  }
  return created ? `＋ ${fmtDateTime(created)} 创建` : ''
}

// taskTimeTip 悬浮提示：列出全部可用时刻，便于精确核对
function taskTimeTip(t) {
  const rows = []
  if (t.createdAt) rows.push(`创建 ${fmtFull(t.createdAt)}`)
  if (t.startedAt) rows.push(`开始 ${fmtFull(t.startedAt)}`)
  if (t.finishedAt) rows.push(`完成 ${fmtFull(t.finishedAt)}`)
  return rows.join('\n')
}

function fmtFull(ts) {
  const d = new Date(ts)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
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

// ---------------- 同一作品的任务合并 ----------------
// 一个作品通常产生两条任务：下载（download）与去水印（inpaint）。它们此前
// 各占一张卡片，同一个作品在列表里出现两次，扫视时要在两条记录间来回对照。
// 现在合并为一张卡片，两个身份由卡片上的「tag 组」承载。
//
// 合并键的设计（按可靠性排序，任一命中即视为同一作品）：
//   ① 作品标题 —— inpaint.title 在入队时由「来源下载任务标题」固化，最可靠
//   ② 目录包含关系 —— 未做子集筛选时 inpaint.inDir === download.resultDir；
//      做了框选子集后 inDir 会变成 selected/<时间戳>，这条兜底就失效了，
//      所以它只能是辅助判据
//   ③ download 自身始终自成一键（URL/resultDir），避免无标题的旧数据被误合并
//
// 注意 ①② 都要跳过空值：空标题把所有无标题任务粘成一条，是比不合并更糟的错误。
function postKeyOf(t) {
  if (t.type === 'download') {
    // 同一 URL 重复下载会被队列去重，故 URL 是稳定的作品标识
    if (t.url) return 'u:' + t.url
    if (t.resultDir) return 'd:' + t.resultDir
    return 'id:' + t.id
  }
  // inpaint：优先用固化标题；标题缺失时退到「源目录」
  const title = t.title || ''
  if (title) return 't:' + title
  if (t.inDir) return 'd:' + t.inDir
  return 'id:' + t.id
}

// 把同作品任务并成一条卡片记录：
//   { key, name, tasks: {download?, inpaint?}, active, order }
// active 是「卡片主体」显示的任务 —— 取两者中最新的一条（创建时刻晚者），
// 这样刚操作过的那条一定在眼前；另一条仍可通过 tag 切换查看。
function mergeTasksByPost(tasks) {
  const groups = new Map()
  // 先按「显式关联」把 inpaint 挂到它所属的 download 上
  const byTitle = new Map()
  const byDir = new Map()
  for (const t of tasks) {
    if (t.type !== 'download') continue
    const title = t.title || t.postRef || ''
    if (title && !byTitle.has(title)) byTitle.set(title, t)
    if (t.resultDir && !byDir.has(t.resultDir)) byDir.set(t.resultDir, t)
  }

  for (const t of tasks) {
    let key = null
    if (t.type === 'inpaint') {
      const title = t.title || ''
      const hitTitle = title ? byTitle.get(title) : null
      const hitDir = t.inDir ? byDir.get(t.inDir) : null
      const owner = hitTitle || hitDir
      if (owner) key = postKeyOf(owner)
    }
    if (!key) key = postKeyOf(t)

    let g = groups.get(key)
    if (!g) {
      g = { key, name: '', tasks: {}, order: 0 }
      groups.set(key, g)
    }
    // 同类型多任务（同一作品重复下载/重复去水印）只保留最新的一条，避免卡片爆炸
    const prev = g.tasks[t.type]
    if (!prev || taskMoment(t) > taskMoment(prev)) g.tasks[t.type] = t
    g.order = Math.max(g.order, taskMoment(t))
  }

  const out = []
  for (const g of groups.values()) {
    const dl = g.tasks.download
    const ip = g.tasks.inpaint
    // 卡片名：优先取名更完整的那个（download 有 postRef、inpaint 有 title）
    g.name = (dl && taskDisplayName(dl)) || (ip && taskDisplayName(ip)) || ''
    // 主体：创建时刻较晚者；没有的一方直接当主体
    if (dl && ip) g.active = taskMoment(ip) > taskMoment(dl) ? ip : dl
    else g.active = dl || ip
    // 是否有去水印任务 —— 决定「去水印」tag 是否灰显
    g.hasInpaint = !!ip
    g.hasDownload = !!dl
    out.push(g)
  }
  // 组间按最新活跃时刻倒序
  out.sort((a, b) => b.order - a.order)
  return out
}

// 卡片内 tag 组当前选中的类型（点 tag 切换卡片主体）。
// 用 ref 而非持久化：这是「当下想看哪一条」的临时选择，不需要记住。
// 键是合并后的作品键，值为 'download' | 'inpaint'。
const cardTab = ref({})

function setCardTab(key, type) {
  cardTab.value = { ...cardTab.value, [key]: type }
}

// 卡片当前展示的任务类型：用户点过 tag 就用他的选择（若那条任务还在），
// 否则回落到「合并时算出的主体」（最新的一条）。
function activeTabOf(c) {
  const picked = cardTab.value[c.key]
  if (picked && c.tasks[picked]) return picked
  return c.active?.type || 'download'
}

// 卡片当前展示的任务对象（随 tag 切换而变化）—— 模板里所有详情都读它
function shownTask(c) {
  return c.tasks[activeTabOf(c)] || c.active
}

// 标题悬浮提示：补充该任务的关键路径/来源，方便区分同标题的不同作品
function taskTitleTip(t) {
  if (!t) return ''
  if (t.type === 'download') return t.url || t.resultDir || ''
  return `${t.inDir || ''}${t.outDir && t.outDir !== t.inDir ? ' → ' + t.outDir : ''}`
}

// 双任务卡片上，去水印 tag 是否灰显：该作品压根没有去水印任务记录
function inpaintTagDim(g) {
  return !g.hasInpaint
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
onBeforeUnmount(() => {
  stopAudition(true)
  window.removeEventListener('focus', onWindowFocus)
})

function go() {
  startDownload()
}

// ---------------- 剪贴板自动读取链接 ----------------
// 窗口聚焦 / 回到首页时自动探测剪贴板：若含社媒图文链接且输入框为空则自动填入。
// WebView2 下 readText 可能因权限被拒——静默失败不提示；「📋」按钮作为用户手势
// 兜底（手势触发时权限通过率高）。
const SOCIAL_LINK_RE = /(douyin|iesdouyin|xiaohongshu|xhslink|mp\.weixin\.qq\.com)/i
const URL_RE = /https?:\/\/[^\s"'<>，。；、）】]+/i
let lastClipLink = ''

async function readClipboardLink(force = false) {
  if (store.stage !== 'idle') return
  let text = ''
  try {
    text = await navigator.clipboard.readText()
  } catch {
    return // 权限拒绝 / 非安全上下文：静默
  }
  const m = String(text || '').match(URL_RE)
  if (!m || !SOCIAL_LINK_RE.test(m[0])) return
  const link = m[0]
  const isNew = link !== lastClipLink
  lastClipLink = link
  if (!force && store.inputUrl.trim()) return // 已有输入不覆盖
  store.inputUrl = link
  if (isNew || force) {
    showToast('已从剪贴板读取链接，点「下载图片」加入队列', 'info')
  }
}

function onWindowFocus() {
  readClipboardLink(false)
}

watch(
  () => store.stage,
  (s) => {
    if (s === 'idle') readClipboardLink(false)
  }
)

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
  showToast('支持公众号 / 小红书 / 抖音图文；下载与去水印将自动加入任务队列', 'info')
  window.addEventListener('keydown', onKey)
  window.addEventListener('focus', onWindowFocus)
  readClipboardLink(false) // 首次进入探测一次剪贴板
})

function onKey(e) {
  if (store.viewer.open && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) {
    viewerNav(e.key === 'ArrowLeft' ? -1 : 1)
    return
  }
  if (e.key === 'Escape') {
    if (store.viewer.open) closeViewer()
    else if (showCacheClear.value) closeCacheClear()
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
            <button
              class="notice-btn cache-btn"
              :class="{ busy: clearingCache }"
              :disabled="clearingCache"
              title="清除本地缓存：删除未被任务记录引用的素材文件，不影响任务记录"
              @click="openCacheClear"
            >
              <span class="notice-icon">🧹</span>
              {{ clearingCache ? '清除中…' : '清除缓存' }}
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
              <button class="btn" title="从剪贴板读取链接" @click="readClipboardLink(true)">📋</button>
              <button class="btn btn-primary" :disabled="!store.inputUrl.trim()" @click="go">
                下载图片
              </button>
            </div>
            <div class="local-row">
              <span class="local-divider"></span>
              <span class="local-label">或</span>
              <span class="local-divider"></span>
            </div>
            <div class="local-row">
              <button class="btn" @click="loadLocalFolder">
                📁 选择本地文件夹
              </button>
              <span class="local-tip">直接识别并去水印本机图片，无需下载</span>
            </div>
            <div class="local-row">
              <button class="btn cookie-btn" @click="openCookie">
                🍪 抖音 Cookie 设置
              </button>
              <span class="local-tip">抖音链接下载失败（风控）时配置，可提高成功率</span>
            </div>
            <div class="platforms">
              <span class="chip">微信公众号</span>
              <span class="chip">小红书图文</span>
              <span class="chip">抖音图文</span>
            </div>
            <p class="auto-queue-tip">下载与去水印均自动加入任务队列后台执行，点击顶栏「任务队列」查看进度与结果</p>
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
                    :disabled="!store.boxes.length || engineError"
                    @click="startBatch(true)"
                    title="加入任务队列后台执行，完成后可在任务队列面板查看结果"
                  >
                    全部去水印
                  </button>
                  <button
                    class="btn"
                    :disabled="store.selected.length === 0 || !store.boxes.length || engineError"
                    @click="startBatch(false)"
                  >
                    仅勾选({{ store.selected.length }})
                  </button>
                  <button class="btn" @click="exportSourceZip" title="把未去水印的原图打包为 zip 导出">
                    ⬇ 源图打包
                  </button>
                  <button
                    v-if="hasBGM"
                    class="btn"
                    :class="{ 'bgm-done': !!store.bgmSaved }"
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
                    :disabled="bgmPlayer.loading"
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
                <p v-if="engineError" class="tip engine-status">
                  ⚠️ {{ store.engineMessage || 'AI 引擎启动失败，请重启应用' }}
                </p>
                <p v-else-if="store.engineStatus === 'starting'" class="tip engine-status">
                  ⏳ {{ store.engineMessage || 'AI 引擎启动中…（已入队的去水印任务会在就绪后自动执行）' }}
                </p>
              </div>
              <div class="log">
                <div v-for="(l, i) in store.logs.slice(-80)" :key="i" :class="l.cls">{{ l.line }}</div>
              </div>
            </div>
          </div>
          <div class="card grid-wrap card-pad">
            <div class="grid-head">
              <span class="grid-title">
                <template v-if="store.materialLoading">正在载入素材…</template>
                <template v-else>已下载 {{ store.sourceThumbs.length }} 张（点左上角 ☑ 勾选；按住 fn / Shift 再点一张＝选中区间；已选中的图可点「✕ 取消」或右键取消；点图片预览）</template>
              </span>
              <button class="btn btn-ghost" @click="store.stage = 'idle'">换一个链接</button>
            </div>
            <!-- 占位期无图可渲染：给出明确的载入态，避免用户面对空白网格 -->
            <div v-if="store.materialLoading && !store.sourceThumbs.length" class="grid-loading">
              <span class="spinner sm"></span>
              <span>正在读取图片清单…</span>
            </div>
            <ImageGrid
              v-else
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

    <!-- 左下角悬浮：GitHub 作品页入口 -->
    <a
      class="gh-float"
      :href="GITHUB_URL"
      :title="`在浏览器中打开 GitHub 作品页\n${GITHUB_URL}`"
      aria-label="GitHub 作品页"
      @click.prevent="openGithub"
    >
      <svg class="gh-icon" viewBox="0 0 16 16" width="20" height="20" aria-hidden="true">
        <path
          fill="currentColor"
          d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z"
        />
      </svg>
      <span class="gh-tip">作品页</span>
    </a>

    <!-- 底部签名 + 免责声明 -->
    <footer class="footer">
      <p class="disclaimer">
        免责声明：本工具仅用于处理您拥有合法权利的内容。使用者需自行承担因使用本工具所产生的一切风险、责任与法律后果；开发者不对任何侵权、违规或不当使用行为承担责任。
      </p>
      <p class="footer-sign">
        <span>作者 chenchen</span>
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
              <p class="qe-tip">在首页粘贴链接点「下载图片」即自动入队；<br>框选后点「全部去水印」自动后台执行</p>
            </div>
            <template v-else>
              <!-- 按时间分组（今天 / 昨天 / 更早）：组头显示数量，可收起展开，
                   右侧「清空」只删该组的已完成任务（执行中/排队中永不受影响）。 -->
              <div v-for="g in visibleGroups" :key="g.key" class="queue-group">
                <div class="qg-head" :class="{ collapsed: collapsedGroups.has(g.key) }">
                  <button
                    class="qg-toggle"
                    :title="collapsedGroups.has(g.key) ? '展开该分组' : '收起该分组'"
                    @click="toggleGroup(g.key)"
                  >
                    <span class="qg-arrow" :class="{ open: !collapsedGroups.has(g.key) }">▶</span>
                    <span class="qg-title">{{ g.title }}</span>
                    <span class="qg-count">{{ g.count }}</span>
                  </button>
                  <button
                    class="btn btn-mini btn-clear-group"
                    :title="`清空「${g.title}」分组内已结束的任务（${g.count} 条）`"
                    @click="clearGroup(g)"
                  >
                    清空
                  </button>
                </div>
                <div v-show="!collapsedGroups.has(g.key)" class="qg-body">
                  <div v-for="c in g.tasks" :key="c.key" class="queue-item">
              <!-- 作品名一行：tag 组（左「去水印」右「下载」）+ 标题 + 当前态 -->
              <div class="qi-head">
                <!-- tag 组：两枚 tag 左右分布，中间一条滑动指示条随选中项移动。
                     点 tag 即切到该任务，卡片主体（进度/按钮/时间）随之更换。 -->
                <div
                  class="qi-tabs"
                  :class="{ 'is-single': !(c.hasDownload && c.hasInpaint) }"
                  :data-active="activeTabOf(c)"
                >
                  <span class="qi-tab-ind" aria-hidden="true"></span>
                  <button
                    class="qi-tab tp-inpaint"
                    :class="{
                      active: activeTabOf(c) === 'inpaint',
                      dim: inpaintTagDim(c),
                    }"
                    :disabled="!c.hasInpaint"
                    :title="c.hasInpaint
                      ? '查看该作品的去水印任务'
                      : '该作品尚未去水印'"
                    @click="setCardTab(c.key, 'inpaint')"
                  >
                    去水印
                  </button>
                  <button
                    class="qi-tab tp-download"
                    :class="{ active: activeTabOf(c) === 'download' }"
                    :disabled="!c.hasDownload"
                    :title="c.hasDownload ? '查看该作品的下载任务' : '该作品没有下载任务记录'"
                    @click="setCardTab(c.key, 'download')"
                  >
                    下载
                  </button>
                </div>
                <span class="qi-title" :title="taskTitleTip(shownTask(c))">
                  {{ taskDisplayName(shownTask(c)) }}
                </span>
                <span
                  class="qi-state"
                  :class="stateMeta[shownTask(c).state]?.cls"
                >{{ stateMeta[shownTask(c).state]?.label || shownTask(c).state }}</span>
              </div>
              <div v-if="shownTask(c).state === 'running' && shownTask(c).progress" class="qi-progress">
                <div class="qi-track">
                  <div
                    class="qi-fill"
                    :style="{ width: (shownTask(c).progress.total ? Math.round((shownTask(c).progress.index / shownTask(c).progress.total) * 100) : 0) + '%' }"
                  ></div>
                </div>
                <span class="qi-pinfo">{{ shownTask(c).progress.index }}/{{ shownTask(c).progress.total }} · {{ shownTask(c).progress.name }}</span>
              </div>
              <div v-if="shownTask(c).summary" class="qi-summary">{{ shownTask(c).summary }}</div>
              <div v-if="shownTask(c).err" class="qi-err" :title="shownTask(c).err">✗ {{ shownTask(c).err }}</div>
              <ul v-if="shownTask(c).failList && shownTask(c).failList.length" class="qi-fails">
                <li v-for="(f, i) in shownTask(c).failList.slice(0, 5)" :key="i">{{ f }}</li>
                <li v-if="shownTask(c).failList.length > 5" class="qi-more">… 共 {{ shownTask(c).failList.length }} 条</li>
              </ul>
              <div v-if="shownTask(c).attempts > 0 || shownTask(c).nextRetryAt" class="qi-meta">
                <span v-if="shownTask(c).attempts > 0">已自动重试 {{ shownTask(c).attempts }} 次</span>
                <span v-if="shownTask(c).nextRetryAt">将于 {{ fmtTs(shownTask(c).nextRetryAt) }} 自动重试</span>
              </div>
              <!-- 底部行：左侧任务时间，右侧操作按钮。
                   时间随状态切换语义（完成时刻／已执行时长／创建时刻），
                   完整时刻在悬浮提示里，不挤占版面。 -->
              <div class="qi-foot">
                <span v-if="taskTime(shownTask(c))" class="qi-time" :title="taskTimeTip(shownTask(c))">
                  {{ taskTime(shownTask(c)) }}
                </span>
                <span v-else class="qi-time qi-time-empty"></span>
                <div class="qi-actions">
                  <button v-if="shownTask(c).state === 'done' && shownTask(c).type === 'download'" class="btn btn-mini" @click="loadTaskMaterial(shownTask(c))">
                    去框选
                  </button>
                  <button v-if="shownTask(c).state === 'done' && shownTask(c).type === 'inpaint'" class="btn btn-mini" @click="openTaskResult(shownTask(c))">
                    查看结果
                  </button>
                  <button
                    v-if="shownTask(c).state === 'running' || shownTask(c).state === 'pending' || shownTask(c).state === 'retry_wait'"
                    class="btn btn-mini"
                    @click="cancelQueueTask(shownTask(c).id)"
                  >
                    取消
                  </button>
                  <button
                    v-if="shownTask(c).state === 'failed' || shownTask(c).state === 'canceled'"
                    class="btn btn-mini"
                    @click="retryQueueTask(shownTask(c).id)"
                  >
                    重试
                  </button>
                  <button
                    v-if="shownTask(c).state === 'done' || shownTask(c).state === 'failed' || shownTask(c).state === 'canceled'"
                    class="btn btn-mini"
                    :title="`移除当前显示的「${typeMeta[shownTask(c).type]}」任务` + (c.hasDownload && c.hasInpaint ? '（另一条任务保留）' : '')"
                    @click="removeQueueTask(shownTask(c).id)"
                  >
                    移除
                  </button>
                </div>
              </div>
                  </div>
                </div>
              </div>
            </template>
          </div>
          <div class="notice-foot queue-foot">
            <span class="qi-spacer"></span>
            <button class="btn btn-mini" title="清除缓存目录中未被任务记录引用的文件" @click="openCacheClear">
              🧹 清除缓存
            </button>
            <button class="btn btn-primary" @click="showQueue = false">关闭</button>
          </div>
        </div>
      </div>
    </transition>

    <!-- 清除缓存：二次确认弹窗（不可逆操作，必须先确认） -->
    <transition name="fade">
      <div v-if="showCacheClear" class="notice-mask" @click.self="closeCacheClear">
        <div class="card notice-panel cache-panel">
          <div class="notice-head">
            <span class="notice-title">🧹 清除本地缓存</span>
            <button class="viewer-close" @click="closeCacheClear">✕</button>
          </div>
          <div class="notice-body cache-body">
            <div v-if="cacheUsage.loading" class="cache-loading">正在统计缓存占用…</div>
            <template v-else>
              <p class="cache-lead">将删除缓存目录中<b>未被任务记录引用</b>的素材文件。</p>
              <div class="cache-stats">
                <div class="cache-stat">
                  <span class="cs-label">可释放</span>
                  <span class="cs-value cs-free">{{ fmtSize(cacheUsage.freeBytes) }}</span>
                </div>
                <div class="cache-stat">
                  <span class="cs-label">目录总计</span>
                  <span class="cs-value">{{ fmtSize(cacheUsage.totalBytes) }}</span>
                </div>
                <div v-if="cacheUsage.keptBytes > 0" class="cache-stat">
                  <span class="cs-label">受保护</span>
                  <span class="cs-value cs-kept">{{ fmtSize(cacheUsage.keptBytes) }}</span>
                </div>
              </div>
              <p v-if="cacheNothing" class="cache-note cache-note-ok">
                缓存已经干净了，没有可释放的文件。
              </p>
              <template v-else>
                <p v-if="cacheUsage.keptBytes > 0" class="cache-note">
                  另有 <b>{{ fmtSize(cacheUsage.keptBytes) }}</b> 仍被
                  {{ cacheUsage.keptDir }} 个任务目录引用（历史任务的「查看结果 / 去框选」
                  还要用），本次<b>不会</b>删除。想连这些一起清，请在「任务队列」里按分组「清空」。
                </p>
                <p class="cache-note cache-note-warn">
                  此操作不可撤销，已下载的素材将被永久删除。
                </p>
              </template>
            </template>
          </div>
          <div class="notice-foot">
            <span class="qi-spacer"></span>
            <button class="btn" :disabled="clearingCache" @click="closeCacheClear">取消</button>
            <button
              class="btn btn-danger"
              :disabled="clearingCache || cacheNothing || cacheUsage.loading"
              @click="doCacheClear"
            >
              {{ clearingCache ? '清除中…' : '确认清除' }}
            </button>
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

/* 「清除缓存」按钮：结构复用 .notice-btn，主色用玫红以区别于前三个（橙/蓝/青绿）。
   清除是不可逆操作，配色上刻意与「查看类」按钮区隔。 */
.cache-btn {
  color: #e5484d;
  box-shadow: inset 0 0 0 1px rgba(229, 72, 77, 0.45);
}
.cache-btn:hover { background: rgba(229, 72, 77, 0.14); }
.cache-btn:disabled { cursor: default; }
/* 清除进行中：呼吸态提示「正在工作」，并压制 hover 高亮避免误以为可再点 */
.cache-btn.busy {
  opacity: 0.72;
  animation: cache-pulse 1.1s ease-in-out infinite;
}
.cache-btn.busy:hover { background: transparent; }
@keyframes cache-pulse {
  0%, 100% { opacity: 0.72; }
  50% { opacity: 0.42; }
}

/* 首页自动入队提示 */
.auto-queue-tip {
  margin-top: 16px;
  font-size: 11.5px;
  color: var(--text-3);
}

/* 队列弹窗：左右双列（下载 | 去水印），窄窗口回退上下堆叠 */
.queue-panel { width: min(980px, 96vw); }
/* 任务列表容器：按时间分组后改为**纵向堆叠**（今天在上、更早在下），
   不再沿用旧的「下载/去水印」左右两栏布局——分组是按时间切的，
   左右并排会让「今天」和「昨天」并列，与时间轴的心智相反。 */
.queue-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
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

/* 按时间分组（今天 / 昨天 / 更早）：组头可点击收起，组内为任务卡片栈 */
.queue-group {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.qg-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 2px 2px 0;
  /* 组头吸顶：任务多时滚动中始终知道当前在哪一档（Windows 分组头同款手感） */
  position: sticky;
  top: 0;
  z-index: 2;
  background: var(--card-solid);
  border-radius: 8px;
}
.qg-head.collapsed { padding-bottom: 2px; }
/* 组头主体是按钮：整行可点，箭头/标题/计数一起响应 */
.qg-toggle {
  display: flex;
  align-items: center;
  gap: 7px;
  flex: 1 1 auto;
  min-width: 0;
  appearance: none;
  border: none;
  background: transparent;
  font-family: inherit;
  font-size: 12.5px;
  font-weight: 650;
  color: var(--text-2);
  padding: 4px 6px;
  border-radius: 8px;
  cursor: pointer;
  text-align: left;
  transition: background 0.2s var(--spring);
}
.qg-toggle:hover { background: var(--accent-soft); }
.qg-toggle:active { transform: scale(0.995); }
/* 三角箭头：收起时指向右，展开时旋转 90° 指向下 */
.qg-arrow {
  display: inline-block;
  font-size: 9px;
  line-height: 1;
  color: var(--text-3);
  transition: transform 0.22s var(--spring);
}
.qg-arrow.open { transform: rotate(90deg); }
.qg-count {
  min-width: 20px;
  padding: 1px 7px;
  border-radius: 999px;
  background: var(--accent-soft);
  color: var(--accent);
  font-size: 11px;
  font-weight: 700;
  text-align: center;
  font-variant-numeric: tabular-nums;
}
.qg-body {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
/* 分组内的「清空」：着色为危险操作，避免与卡片上的普通按钮混淆 */
.btn-clear-group {
  flex: 0 0 auto;
  color: var(--danger);
  box-shadow: inset 0 0 0 1px rgba(255, 69, 58, 0.35);
  background: transparent;
}
.btn-clear-group:hover { background: var(--danger-soft); }

/* 作品卡片：一个作品一条，下载/去水印两个身份收在卡内的 tag 组里 */
.queue-item {
  border: 1px solid var(--card-border);
  border-radius: 12px;
  padding: 10px 12px;
  background: var(--card-solid);
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.04);
  transition: box-shadow 0.22s var(--spring), border-color 0.22s var(--spring);
}
/* 悬浮时轻微抬升 —— 呼应 macOS 列表项的指针反馈，幅度克制不喧宾夺主 */
.queue-item:hover {
  border-color: rgba(10, 132, 255, 0.28);
  box-shadow: 0 3px 10px rgba(0, 0, 0, 0.08);
}
.qi-head { display: flex; align-items: center; gap: 8px; min-width: 0; }

/* ---------- tag 组：两枚标签 + 一条滑动指示条 ---------- */
/* 交互即「分段控件」（Segmented Control）：两枚 tag 左右分布，
   指示条按弹簧曲线滑到选中项下方，点击即切换卡片主体。 */
.qi-tabs {
  --tab-w: 52px;               /* 单枚 tag 宽度基准 */
  position: relative;
  flex: 0 0 auto;
  display: grid;
  grid-template-columns: repeat(2, var(--tab-w));
  align-items: center;
  padding: 2px;
  border-radius: 8px;
  background: rgba(128, 128, 128, 0.12);
}
/* 滑动指示条：translateX 在 0 / 100% 之间切换。
   用临界阻尼弹簧（无回弹）—— 分段控件的位移是「归类」而非「投掷」，
   过冲会让人误以为选错了位置。 */
.qi-tab-ind {
  position: absolute;
  top: 2px;
  left: 2px;
  width: var(--tab-w);
  height: calc(100% - 4px);
  border-radius: 6px;
  background: var(--card-solid);
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.14);
  transition: transform 0.34s var(--spring);
  will-change: transform;
}
.qi-tabs[data-active='download'] .qi-tab-ind { transform: translateX(0); }
.qi-tabs[data-active='inpaint'] .qi-tab-ind { transform: translateX(var(--tab-w)); }

.qi-tab {
  position: relative;   /* 压在指示条之上 */
  z-index: 1;
  appearance: none;
  border: 0;
  background: transparent;
  font: inherit;
  font-size: 10.5px;
  font-weight: 600;
  letter-spacing: 0.2px;
  padding: 3px 0;
  border-radius: 6px;
  cursor: pointer;
  color: var(--text-3);
  transition: color 0.18s ease, opacity 0.18s ease;
  -webkit-tap-highlight-color: transparent;
}
/* 选中态：着色为各自的身份色（去水印紫 / 下载蓝） */
.qi-tab.active.tp-download { color: var(--accent); }
.qi-tab.active.tp-inpaint { color: #9d6bff; }
.qi-tab:not(.active):hover { color: var(--text-2); }

/* 置灰：该作品尚未去水印 —— 用降透明度 + 去色表达「这条路还没走过」，
   仍可点击（点了不会切到不存在的任务，由 disabled 兜住） */
.qi-tab.dim {
  opacity: 0.38;
  cursor: default;
}
.qi-tab:disabled { cursor: default; }
/* 单任务卡片（只有一个身份）：不画滑动指示条 —— 只有一个选项还画分段控件
   会让人误以为有得选。但**选中态仍保留身份色**（去水印紫 / 下载蓝）：
   tag 在这里承担的是「这是什么任务」的标识，去色会丢掉这个信息。
   置灰（.dim）只用于「该作品尚未去水印」，与单/双任务无关。 */
.qi-tabs.is-single .qi-tab-ind { display: none; }

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
/* 底部行：时间贴左下角，操作按钮靠右；时间过长时先让位给按钮（不换行挤压 */
.qi-foot {
  margin-top: 8px;
  display: flex;
  align-items: center;
  gap: 10px;
}
.qi-time {
  flex: 1 1 auto;
  min-width: 0;
  font-size: 11px;
  color: var(--text-3);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  cursor: default;
}
.qi-time-empty { flex: 1 1 auto; }
.qi-actions { flex: 0 0 auto; display: flex; gap: 8px; justify-content: flex-end; }
.btn-mini { padding: 3px 12px; font-size: 11.5px; }
.queue-foot { justify-content: flex-start; gap: 8px; }
.queue-foot .qi-spacer { flex: 1; }

/* 清除缓存确认弹窗：比通用弹窗窄一档，内容本就不多，宽了显得空旷 */
.cache-panel { width: min(460px, 94vw); }
.cache-body { font-size: 12.5px; line-height: 1.65; }
.cache-loading { color: var(--text-3); text-align: center; padding: 22px 0; }
.cache-lead { margin: 0 0 12px; color: var(--text); }
.cache-stats {
  display: flex;
  gap: 10px;
  margin-bottom: 12px;
}
.cache-stat {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 3px;
  padding: 9px 11px;
  border-radius: 10px;
  background: rgba(128, 128, 128, 0.08);
  border: 1px solid var(--card-border);
}
.cs-label { font-size: 11px; color: var(--text-3); }
.cs-value { font-size: 16px; font-weight: 700; color: var(--text); font-variant-numeric: tabular-nums; }
/* 「可释放」用danger色强化——这是用户点确认后真正会消失的量 */
.cs-free { color: var(--danger); }
/* 「受保护」用中性偏绿，明确传递「这些不会动」 */
.cs-kept { color: #2bb3a3; }
.cache-note {
  margin: 0 0 8px;
  font-size: 11.5px;
  color: var(--text-3);
  line-height: 1.6;
}
.cache-note b { color: var(--text-2); }
.cache-note-warn { color: var(--danger); }
.cache-note-ok { color: #2bb3a3; }

.content { flex: 1; position: relative; overflow: hidden; }
.stage-view { height: 100%; overflow-y: auto; display: flex; flex-direction: column; gap: 14px; }
.stage-view.center { align-items: center; justify-content: center; }

/* 左下角悬浮 GitHub 作品页入口：固定在视口左下，不参与 shell 的 flex 流 */
.gh-float {
  position: fixed;
  left: 16px;
  bottom: 16px;
  z-index: 30;
  display: flex;
  align-items: center;
  gap: 0;
  padding: 9px;
  border-radius: 999px;
  color: var(--text-3);
  background: var(--card-bg);
  box-shadow: inset 0 0 0 1px var(--card-border);
  text-decoration: none;
  cursor: pointer;
  transition: color 0.18s ease, box-shadow 0.18s ease, transform 0.18s ease;
}
/* 「作品页」文字默认折叠成 0 宽，hover 时展开 —— 常态只占一个圆形图标的位置 */
.gh-float .gh-tip {
  max-width: 0;
  overflow: hidden;
  white-space: nowrap;
  font-size: 11.5px;
  letter-spacing: 0.4px;
  opacity: 0;
  transition: max-width 0.22s ease, opacity 0.18s ease, margin-left 0.22s ease;
}
.gh-float:hover {
  color: var(--text);
  transform: translateY(-2px);
  box-shadow: inset 0 0 0 1px var(--accent), var(--shadow-lg);
}
.gh-float:hover .gh-tip { max-width: 64px; opacity: 1; margin-left: 6px; }
.gh-float:active { transform: translateY(0); }
.gh-float .gh-icon { display: block; flex: 0 0 auto; }

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
/* 行内小号转圈：用于网格载入态等需要与文字同行的位置（不占块级外边距） */
.spinner.sm { width: 15px; height: 15px; border-width: 2px; margin-bottom: 0; }
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
/* 占位期载入态：网格暂无图可渲染时的居中提示（缩略图本身由 ImageGrid 占位） */
.grid-loading {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 34px 0;
  font-size: 12.5px;
  color: var(--text-3);
}

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

/* ---------- 无障碍：减弱动效 ----------
   系统开启「减弱动态效果」时，把滑动指示条的位移改为瞬时完成（保留状态区分，
   去掉运动过程）。界面不因此失去可读性：选中项仍靠颜色与指示条区分，
   只是「滑过去」变成「直接出现」——这是 Apple 对 reduce motion 的一致处理。 */
@media (prefers-reduced-motion: reduce) {
  .qi-tab-ind,
  .queue-item,
  .qi-tab,
  .gh-float,
  .gh-float .gh-tip {
    transition-duration: 0.01ms !important;
  }
  /* 呼吸态改为静态弱化：动效对前庭敏感用户不友好，但「正在清除」这个
     状态本身必须可见，故保留 opacity 只是不再闪烁 */
  .cache-btn.busy { animation: none; opacity: 0.55; }
}
</style>
