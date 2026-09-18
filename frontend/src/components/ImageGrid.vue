<script setup>
import { onMounted, onUnmounted, watch } from 'vue'
import { store } from '../store.js'

const props = defineProps({
  images: { type: Array, default: () => [] }, // [{path,thumb,name}]
  selectable: { type: Boolean, default: false }, // 左上角勾选框选中；点击图片=预览
  zoomable: { type: Boolean, default: false }, // 点击放大查看（结果页用）
  activePath: { type: String, default: '' }, // 当前预览中的图片（高亮）
})
const emit = defineEmits(['toggle', 'preview', 'view', 'range', 'deselect', 'deselect-all'])

// 区间选中：按住 fn（或 Shift 作为等效键）点勾选框，批量选中锚点到当前图之间的所有图片
let rangeKeyDown = false
let anchorIndex = -1 // 上次单击勾选的位置

function isFnEvent(e) {
  if (!e) return false
  const k = (e.key || '').toLowerCase()
  const c = (e.code || '').toLowerCase()
  return k === 'fn' || c === 'fn'
}

// fn 属于系统级修饰键：macOS 下 WKWebView/Safari 不会把它上报给网页（getModifierState('Fn') 恒为 false），
// 因此同时支持 Shift 作为等效键，保证该交互在 Mac 上可用。
function rangeMode(e) {
  if (e) {
    try {
      if (typeof e.getModifierState === 'function' && e.getModifierState('Fn')) return true
    } catch {
      /* getModifierState 不支持 Fn 时忽略 */
    }
    if (e.shiftKey) return true
  }
  return rangeKeyDown
}

function onKeyDown(e) {
  if (isFnEvent(e)) rangeKeyDown = true
}
function onKeyUp(e) {
  if (isFnEvent(e)) rangeKeyDown = false
}
function onKeyReset() {
  rangeKeyDown = false
}

function toggle(path) {
  if (!removeFromSelected(path)) store.selected.push(path)
  emit('toggle', path)
}

// 从选中集合移除；已在集合中返回 true，未选中返回 false（幂等，可安全重复调用）
function removeFromSelected(path) {
  const i = store.selected.indexOf(path)
  if (i < 0) return false
  store.selected.splice(i, 1)
  return true
}

// 取消选中单张：重复取消 / 未选中时触发均为空操作，不抛错、不发事件
function deselect(path) {
  if (!removeFromSelected(path)) return false
  emit('deselect', path)
  return true
}

// 取消全部选中：无可取消项时返回 0，同样不做事
function deselectAll() {
  const n = store.selected.length
  if (n === 0) return 0
  store.selected.splice(0, store.selected.length)
  anchorIndex = -1 // 选择清空后锚点失效
  emit('deselect-all', { count: n })
  return n
}

function onDeselect(path, e) {
  if (e) e.stopPropagation()
  deselect(path)
}

// 右键已选中的图片 = 取消选中；未选中时不拦截，保留系统右键菜单
function onTileContext(e, path) {
  if (!props.selectable || !isSelected(path)) return
  e.preventDefault()
  deselect(path)
}

// 勾选结果按图片顺序归一化，保证顺序与网格一致（便于「仅勾选」处理与回看）
function normalizeSelection() {
  const set = new Set(store.selected)
  const ordered = props.images.filter((img) => set.has(img.path)).map((img) => img.path)
  store.selected.splice(0, store.selected.length, ...ordered)
}

function onCheck(path, e) {
  const idx = props.images.findIndex((img) => img.path === path)
  if (idx < 0) return
  // 已有锚点 + 按住 fn → 选中两者之间（含两端）的全部图片，锚点保持不变便于继续扩展
  if (rangeMode(e) && anchorIndex >= 0 && anchorIndex !== idx) {
    const from = Math.min(anchorIndex, idx)
    const to = Math.max(anchorIndex, idx)
    let added = 0
    for (let i = from; i <= to; i++) {
      const p = props.images[i].path
      if (!store.selected.includes(p)) {
        store.selected.push(p)
        added++
      }
    }
    normalizeSelection()
    emit('range', { from, to, count: to - from + 1, added })
    return
  }
  toggle(path)
  anchorIndex = idx
}

function isSelected(path) {
  return props.selectable && store.selected.includes(path)
}
function onTile(path) {
  if (props.selectable) emit('preview', path)
  else if (props.zoomable) emit('view', path)
}

onMounted(() => {
  window.addEventListener('keydown', onKeyDown)
  window.addEventListener('keyup', onKeyUp)
  window.addEventListener('blur', onKeyReset)
})
onUnmounted(() => {
  window.removeEventListener('keydown', onKeyDown)
  window.removeEventListener('keyup', onKeyUp)
  window.removeEventListener('blur', onKeyReset)
})

// 换一批图片后锚点失效，避免跨批次误选
watch(
  () => props.images,
  () => {
    anchorIndex = -1
  }
)
</script>

<template>
  <div class="ig-root">
    <!-- 选中状态条：数量 + 一键取消全部 -->
    <transition name="selbar">
      <div v-if="selectable && store.selected.length" class="sel-bar">
        <span class="sel-count">
          已选中 <b>{{ store.selected.length }}</b> / {{ images.length }} 张
        </span>
        <span class="sel-hint">悬停图片点「✕ 取消」或右键图片可取消单张</span>
        <button class="sel-clear" title="取消所有已选中的图片" @click="deselectAll">
          取消全部选中
        </button>
      </div>
    </transition>
    <div class="grid">
      <div
        v-for="(img, i) in images"
        :key="img.path"
        class="tile"
        :class="{ selected: isSelected(img.path), active: props.activePath === img.path }"
        :style="{ animationDelay: Math.min(i * 40, 400) + 'ms' }"
        @click="onTile(img.path)"
        @contextmenu="onTileContext($event, img.path)"
      >
        <img v-if="img.thumb" :src="img.thumb" :alt="img.name" loading="lazy" draggable="false" />
        <div v-else class="ph" :title="img.name">{{ img.loading ? '加载中…' : '无法预览' }}</div>
        <button
          v-if="selectable"
          class="check"
          :class="{ checked: isSelected(img.path) }"
          :title="isSelected(img.path) ? '取消勾选' : '勾选（用于仅勾选处理）；按住 fn / Shift 再点另一张可选中区间全部图片'"
          @click.stop="onCheck(img.path, $event)"
        >
          <span v-if="isSelected(img.path)">✓</span>
        </button>
        <!-- 已选中的图片：显式取消入口（重复点击/未选中时为空操作） -->
        <button
          v-if="selectable && isSelected(img.path)"
          class="deselect"
          title="取消选中这张图片"
          @click="onDeselect(img.path, $event)"
          @mousedown.stop
        >
          ✕ 取消
        </button>
        <div v-if="zoomable" class="zoom-hint">🔍</div>
        <div class="name">{{ img.name }}</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 选中状态条 */
.sel-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 10px;
  padding: 7px 12px;
  border-radius: 10px;
  background: var(--accent-soft);
  font-size: 12px;
  color: var(--text-2);
  flex-wrap: wrap;
}
.sel-count b { color: var(--accent); font-size: 13px; }
.sel-hint { font-size: 11.5px; color: var(--text-3); }
.sel-clear {
  margin-left: auto;
  appearance: none;
  border: none;
  font-family: inherit;
  font-size: 12px;
  color: var(--danger);
  background: transparent;
  padding: 4px 10px;
  border-radius: 999px;
  cursor: pointer;
  box-shadow: inset 0 0 0 1px rgba(255, 69, 58, 0.4);
  transition: background 0.2s var(--spring), transform 0.15s var(--spring);
}
.sel-clear:hover { background: var(--danger-soft); }
.sel-clear:active { transform: scale(0.96); }
.selbar-enter-active, .selbar-leave-active { transition: opacity 0.2s var(--spring), transform 0.2s var(--spring); }
.selbar-enter-from, .selbar-leave-to { opacity: 0; transform: translateY(-6px); }

/* 已选中的图片：高亮描边（active 预览态叠加更粗） */
.tile.selected { box-shadow: 0 0 0 2.5px var(--accent), var(--shadow); }
.tile.selected.active { box-shadow: 0 0 0 3.5px var(--accent), var(--shadow-lg); }

/* 已选中的图片：显式取消入口 */
.deselect {
  position: absolute;
  top: 8px;
  right: 8px;
  display: inline-flex;
  align-items: center;
  gap: 2px;
  padding: 3px 8px;
  border: none;
  border-radius: 999px;
  background: rgba(0, 0, 0, 0.55);
  color: #fff;
  font-family: inherit;
  font-size: 11px;
  line-height: 1.5;
  cursor: pointer;
  opacity: 0.88;
  transition: background 0.2s var(--spring), opacity 0.2s var(--spring), transform 0.15s var(--spring-bounce);
}
.deselect:hover { background: var(--danger); opacity: 1; transform: scale(1.05); }
.deselect:active { transform: scale(0.96); }

.ph {
  width: 100%;
  height: 130px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  color: var(--text-3);
  background: repeating-linear-gradient(
    45deg,
    var(--card-solid),
    var(--card-solid) 12px,
    var(--card-border) 12px,
    var(--card-border) 13px
  );
}
.check {
  position: absolute;
  top: 8px;
  left: 8px;
  width: 22px;
  height: 22px;
  border-radius: 6px;
  border: 1.5px solid rgba(255, 255, 255, 0.9);
  background: rgba(0, 0, 0, 0.28);
  color: #fff;
  font-size: 14px;
  line-height: 1;
  display: flex;
  align-items: center;
  justify-content: center;
  cursor: pointer;
  padding: 0;
  transition: background 0.2s var(--spring), border-color 0.2s var(--spring),
    transform 0.15s var(--spring-bounce);
}
.check:hover { transform: scale(1.1); }
.check.checked {
  background: var(--accent);
  border-color: var(--accent);
}
.tile.active {
  box-shadow: 0 0 0 2.5px var(--accent), var(--shadow);
}
.zoom-hint {
  position: absolute;
  top: 8px;
  right: 8px;
  width: 24px;
  height: 24px;
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 13px;
  color: #fff;
  background: rgba(0, 0, 0, 0.42);
  opacity: 0;
  transform: scale(0.7);
  transition: all 0.2s var(--spring-bounce);
  pointer-events: none;
}
.tile:hover .zoom-hint { opacity: 1; transform: scale(1); }
</style>
