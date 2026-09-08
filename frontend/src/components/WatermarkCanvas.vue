<script setup>
import { ref, computed, watch } from 'vue'
import { store } from '../store.js'

const props = defineProps({
  src: { type: String, default: '' },
  width: { type: Number, default: 0 }, // 原图宽（用于把显示坐标换算回原图像素）
  height: { type: Number, default: 0 }, // 原图高
})

const MAX_W = 620
const MAX_H = 400

const stageEl = ref(null)
const dispW = ref(0)
const dispH = ref(0)
const natW = ref(0)
const natH = ref(0)
const rects = ref([]) // 已确认的框选（显示坐标） [{x,y,w,h}]
const dragRect = ref(null) // 拖拽新建中的框
const resizing = ref(null) // { index, handle, sx, sy, startRect }
let dragging = false
let startX = 0
let startY = 0

const HANDLES = ['nw', 'n', 'ne', 'e', 'se', 's', 'sw', 'w']
const CURSORS = {
  nw: 'nwse-resize', se: 'nwse-resize',
  ne: 'nesw-resize', sw: 'nesw-resize',
  n: 'ns-resize', s: 'ns-resize',
  e: 'ew-resize', w: 'ew-resize',
}

function stageXY(e) {
  const r = stageEl.value.getBoundingClientRect()
  return { x: e.clientX - r.left, y: e.clientY - r.top }
}

function onImgLoad(e) {
  const img = e.target
  natW.value = img.naturalWidth
  natH.value = img.naturalHeight
  const s = Math.min(MAX_W / natW.value, MAX_H / natH.value, 1)
  dispW.value = Math.max(1, Math.round(natW.value * s))
  dispH.value = Math.max(1, Math.round(natH.value * s))
  rects.value = []
  dragRect.value = null
  resizing.value = null
  store.boxes = []
  store.ratios = []
}

function origDims() {
  return {
    w: props.width || natW.value,
    h: props.height || natH.value,
  }
}

// 显示坐标 → 原图像素坐标
function toPixel(r) {
  const { w, h } = origDims()
  return {
    x1: Math.max(0, Math.round((r.x * w) / dispW.value)),
    y1: Math.max(0, Math.round((r.y * h) / dispH.value)),
    x2: Math.min(w, Math.round(((r.x + r.w) * w) / dispW.value)),
    y2: Math.min(h, Math.round(((r.y + r.h) * h) / dispH.value)),
  }
}

function syncStore() {
  const { w, h } = origDims()
  const boxes = []
  const ratios = []
  for (const r of rects.value) {
    const b = toPixel(r)
    if (b.x2 - b.x1 < 2 || b.y2 - b.y1 < 2) continue
    boxes.push([b.x1, b.y1, b.x2, b.y2])
    ratios.push([b.x1 / w, b.y1 / h, b.x2 / w, b.y2 / h])
  }
  store.boxes = boxes
  store.ratios = ratios
}

function onDown(e) {
  if (!dispW.value) return
  dragging = true
  const p = stageXY(e)
  startX = p.x
  startY = p.y
  dragRect.value = { x: p.x, y: p.y, w: 0, h: 0 }
}

function onMove(e) {
  const p = stageXY(e)
  if (resizing.value) {
    const rs = resizing.value
    const dx = p.x - rs.sx
    const dy = p.y - rs.sy
    const r = resizeRect(rs.startRect, rs.handle, dx, dy)
    rects.value[rs.index] = r
    return
  }
  if (!dragging) return
  dragRect.value = {
    x: Math.min(startX, p.x),
    y: Math.min(startY, p.y),
    w: Math.abs(p.x - startX),
    h: Math.abs(p.y - startY),
  }
}

function onUp() {
  if (resizing.value) {
    resizing.value = null
    syncStore()
    return
  }
  if (!dragging) return
  dragging = false
  const r = dragRect.value
  dragRect.value = null
  if (!r || r.w < 4 || r.h < 4) return
  const b = toPixel(r)
  if (b.x2 - b.x1 < 2 || b.y2 - b.y1 < 2) return
  rects.value.push(r)
  syncStore()
}

function startResize(i, handle, e) {
  const p = stageXY(e)
  resizing.value = {
    index: i,
    handle,
    sx: p.x,
    sy: p.y,
    startRect: { ...rects.value[i] },
  }
}

// 按手柄方向调整矩形；固定对边，移动该边/角，夹在画布内并保证最小 2px
function resizeRect(start, handle, dx, dy) {
  let x1 = start.x
  let y1 = start.y
  let x2 = start.x + start.w
  let y2 = start.y + start.h
  if (handle.indexOf('w') >= 0) x1 = start.x + dx
  if (handle.indexOf('e') >= 0) x2 = start.x + start.w + dx
  if (handle.indexOf('n') >= 0) y1 = start.y + dy
  if (handle.indexOf('s') >= 0) y2 = start.y + start.h + dy

  const nx1 = Math.max(0, Math.min(x1, x2))
  const nx2 = Math.min(dispW.value, Math.max(x1, x2))
  const ny1 = Math.max(0, Math.min(y1, y2))
  const ny2 = Math.min(dispH.value, Math.max(y1, y2))
  const w = Math.max(2, nx2 - nx1)
  const h = Math.max(2, ny2 - ny1)
  return { x: nx1, y: ny1, w, h }
}

function removeBox(i) {
  rects.value.splice(i, 1)
  resizing.value = null
  syncStore()
}

function clearBoxes() {
  rects.value = []
  dragRect.value = null
  resizing.value = null
  syncStore()
}

const rectStyle = (r) => ({
  left: r.x + 'px',
  top: r.y + 'px',
  width: r.w + 'px',
  height: r.h + 'px',
})

function handleStyle(r, h) {
  const cx = { nw: 0, n: 0.5, ne: 1, e: 1, se: 1, s: 0.5, sw: 0, w: 0 }[h]
  const cy = { nw: 0, n: 0, ne: 0, e: 0.5, se: 1, s: 1, sw: 1, w: 0.5 }[h]
  return {
    left: `calc(${cx * 100}% - 4px)`,
    top: `calc(${cy * 100}% - 4px)`,
    cursor: CURSORS[h],
  }
}

const infoText = computed(() => {
  if (!store.boxes.length) return '水印区域: 未框选（可拖拽框选多个，拖动手柄调整大小）'
  const parts = store.boxes.map((b, i) => {
    const [x1, y1, x2, y2] = b
    return `${i + 1}: (${x1},${y1})-(${x2},${y2})`
  })
  return `已框选 ${store.boxes.length} 个区域 | ${parts.join('  ')}`
})

watch(
  () => props.src,
  () => {
    rects.value = []
    dragRect.value = null
    resizing.value = null
    store.boxes = []
    store.ratios = []
  }
)
</script>

<template>
  <div class="wm-wrap">
    <div
      ref="stageEl"
      class="wm-stage"
      :style="{ width: dispW + 'px', height: dispH + 'px' }"
      @mousedown="onDown"
      @mousemove="onMove"
      @mouseup="onUp"
      @mouseleave="onUp"
    >
      <img v-if="src" :src="src" draggable="false" @load="onImgLoad" />
      <div
        v-for="(r, i) in rects"
        :key="'r' + i"
        class="wm-rect"
        :style="rectStyle(r)"
      >
        <button
          class="wm-rect-x"
          :title="`删除第 ${i + 1} 个区域`"
          @mousedown.stop
          @click.stop="removeBox(i)"
        >✕</button>
        <span
          v-for="h in HANDLES"
          :key="h"
          class="wm-handle"
          :style="handleStyle(r, h)"
          @mousedown.stop.prevent="startResize(i, h, $event)"
        ></span>
      </div>
      <div v-if="dragRect" class="wm-rect drag" :style="rectStyle(dragRect)"></div>
      <div v-if="!rects.length && !dragRect && src" class="wm-hint">按住左键拖拽框选水印区域（可框选多个，拖动手柄调整大小）</div>
    </div>
    <div class="wm-info">
      <span>{{ infoText }}</span>
      <button v-if="rects.length" class="wm-clear" @click="clearBoxes">清空</button>
    </div>
  </div>
</template>

<style scoped>
.wm-wrap {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 8px;
}
.wm-stage {
  position: relative;
  background: var(--card-solid);
  border-radius: var(--radius-sm);
  box-shadow: var(--shadow);
  overflow: hidden;
  cursor: crosshair;
  user-select: none;
  flex-shrink: 0;
}
.wm-stage img {
  display: block;
  width: 100%;
  height: 100%;
  object-fit: fill;
  pointer-events: none;
}
.wm-rect {
  position: absolute;
  border: 2px solid var(--danger);
  background: rgba(255, 69, 58, 0.15);
  border-radius: 2px;
  pointer-events: none;
}
.wm-rect.drag {
  border-style: dashed;
  background: rgba(255, 69, 58, 0.1);
}
.wm-rect-x {
  position: absolute;
  top: -26px;
  right: -26px;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  border: none;
  background: var(--danger);
  color: #fff;
  font-size: 11px;
  line-height: 1;
  cursor: pointer;
  pointer-events: auto;
  display: flex;
  align-items: center;
  justify-content: center;
  box-shadow: 0 1px 4px rgba(0, 0, 0, 0.3);
  transition: transform 0.15s var(--spring-bounce);
}
.wm-rect-x:hover { transform: scale(1.2); }
.wm-handle {
  position: absolute;
  width: 8px;
  height: 8px;
  background: #fff;
  border: 1.5px solid var(--danger);
  border-radius: 2px;
  pointer-events: auto;
}
.wm-handle:hover { background: var(--danger); }
.wm-hint {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--text-3);
  font-size: 13px;
  background: rgba(0, 0, 0, 0.18);
  pointer-events: none;
}
.wm-info {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 12px;
  color: var(--text-2);
  font-variant-numeric: tabular-nums;
  max-width: 620px;
  overflow-x: auto;
  white-space: nowrap;
}
.wm-clear {
  appearance: none;
  border: none;
  background: transparent;
  color: var(--danger);
  font-size: 12px;
  cursor: pointer;
  padding: 2px 6px;
  border-radius: 6px;
}
.wm-clear:hover { background: var(--danger-soft); }
</style>
