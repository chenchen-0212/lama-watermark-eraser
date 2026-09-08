<script setup>
import { computed, onMounted, ref } from 'vue'
import {
  store,
  initEvents,
  startDownload,
  loadLocalFolder,
  startBatch,
  cancelBatch,
  exportZip,
  showToast,
  viewImage,
  closeViewer,
} from './store.js'
import { GetDouyinCookie, SetDouyinCookie } from '../wailsjs/go/main/App'
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

onMounted(() => {
  initEvents()
  showToast('支持公众号文章 / 小红书图文 / 抖音图文链接', 'info')
  window.addEventListener('keydown', onKey)
})

function onKey(e) {
  if (e.key === 'Escape') {
    if (store.viewer.open) closeViewer()
    else if (showNotice.value) showNotice.value = false
    else if (showCookie.value) showCookie.value = false
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
                  <span class="opt-label">边缘外扩</span>
                  <input type="number" min="0" max="80" v-model.number="store.dilate" class="input num" />
                  <span class="opt-unit">px</span>
                </div>
                <div class="opt-row btns">
                  <button
                    class="btn btn-primary"
                    :disabled="store.running || !store.boxes.length"
                    @click="startBatch(true)"
                  >
                    全部去水印
                  </button>
                  <button
                    class="btn"
                    :disabled="store.running || store.selected.length === 0 || !store.boxes.length"
                    @click="startBatch(false)"
                  >
                    仅勾选({{ store.selected.length }})
                  </button>
                  <button class="btn btn-danger" :disabled="!store.running" @click="cancelBatch">取消</button>
                </div>
                <p class="tip">可拖拽框选多个水印区域；点区域右上角 ✕ 可删除单个，点「清空」全部删除。</p>
              </div>
              <div class="log">
                <div v-for="(l, i) in store.logs.slice(-80)" :key="i" :class="l.cls">{{ l.line }}</div>
              </div>
            </div>
          </div>
          <div class="card grid-wrap card-pad">
            <div class="grid-head">
              <span class="grid-title">已下载 {{ store.sourceThumbs.length }} 张（点左上角 ☑ 勾选，点图片预览）</span>
              <button class="btn btn-ghost" @click="store.stage = 'idle'">换一个链接</button>
            </div>
            <ImageGrid :images="store.sourceThumbs" selectable :active-path="previewItem?.path || ''" @preview="onPreview" />
          </div>
        </section>
      </transition>

      <!-- 步骤4：处理中 -->
      <transition name="stage">
        <section v-if="store.stage === 'inpainting'" class="stage-view center" key="inpainting">
          <div class="card progress-card">
            <div class="spinner"></div>
            <h2 class="h-title">LaMa 正在修复…</h2>
            <p class="h-sub">
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
      <p class="footer-sign">作者 csy</p>
    </footer>

    <!-- 放大查看浮层 -->
    <transition name="fade">
      <div v-if="store.viewer.open" class="viewer" @click="closeViewer">
        <div class="viewer-bar">
          <span class="viewer-name">{{ store.viewer.name }}</span>
          <button class="viewer-close" @click.stop="closeViewer">✕</button>
        </div>
        <div v-if="store.viewer.loading" class="spinner"></div>
        <img
          v-else
          :src="store.viewer.src"
          class="viewer-img"
          @click.stop
          draggable="false"
        />
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
}

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
  max-width: 70%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
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
