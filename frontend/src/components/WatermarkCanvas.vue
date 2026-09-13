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
const SNAP = 8 // 边缘吸附阈值（显示像素），框边进入该范围即贴合图像边缘

const stageEl = ref(null)
const dispW = ref(0)
const dispH = ref(0)
const natW = ref(0)
const natH = ref(0)
const rects = ref([]) // 已确认的框选（显示坐标） [{x,y,w,h}]
const dragRect = ref(null) // 拖拽新建中的框
const resizing = ref(null) // { index, handle, sx, sy, startRect }
const moving = ref(null) // { index, sx, sy, startRect }
const snapGuides = ref({ v: [], h: [] }) // 吸附提示线：v=竖线x坐标，h=横线y坐标
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
  moving.value = null
  clearGuides()
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
    const cand = resizeRect(rs.startRect, rs.handle, dx, dy)
    const snap = snapResizeRect(cand, rs.handle)
    rects.value[rs.index] = snap.rect
    snapGuides.value = { v: snap.vg, h: snap.hg }
    syncStore()
    return
  }
  if (moving.value) {
    const mv = moving.value
    const s = mv.startRect
    const maxX = Math.max(0, dispW.value - s.w)
    const maxY = Math.max(0, dispH.value - s.h)
    const nx = Math.min(Math.max(0, s.x + (p.x - mv.sx)), maxX)
    const ny = Math.min(Math.max(0, s.y + (p.y - mv.sy)), maxY)
    const snap = snapMoveRect({ x: nx, y: ny, w: s.w, h: s.h })
    rects.value[mv.index] = snap.rect
    snapGuides.value = { v: snap.vg, h: snap.hg }
    syncStore()
    return
  }
  if (!dragging) return
  const raw = {
    x: Math.min(startX, p.x),
    y: Math.min(startY, p.y),
    w: Math.abs(p.x - startX),
    h: Math.abs(p.y - startY),
  }
  const snap = snapEdges(raw)
  dragRect.value = snap.rect
  snapGuides.value = { v: snap.vg, h: snap.hg }
}

function onUp() {
  if (resizing.value) {
    resizing.value = null
    clearGuides()
    syncStore()
    return
  }
  if (moving.value) {
    moving.value = null
    clearGuides()
    syncStore()
    return
  }
  if (!dragging) return
  dragging = false
  const r = dragRect.value
  dragRect.value = null
  clearGuides()
  if (!r || r.w < 4 || r.h < 4) return
  const b = toPixel(r)
  if (b.x2 - b.x1 < 2 || b.y2 - b.y1 < 2) return
  rects.value.push(r)
  syncStore()
}

function clearGuides() {
  snapGuides.value = { v: [], h: [] }
}

// 新建框：四条边各自独立吸附（角点由鼠标自由张合，可分别贴边）
function snapEdges(r) {
  let x1 = r.x
  let y1 = r.y
  let x2 = r.x + r.w
  let y2 = r.y + r.h
  const vg = []
  const hg = []
  if (Math.abs(x1) <= SNAP) { x1 = 0; vg.push(0) }
  if (Math.abs(x2 - dispW.value) <= SNAP) { x2 = dispW.value; vg.push(dispW.value) }
  if (Math.abs(y1) <= SNAP) { y1 = 0; hg.push(0) }
  if (Math.abs(y2 - dispH.value) <= SNAP) { y2 = dispH.value; hg.push(dispH.value) }
  return {
    rect: { x: x1, y: y1, w: Math.max(2, x2 - x1), h: Math.max(2, y2 - y1) },
    vg, hg,
  }
}

// 移动：整体平移吸附，保持宽高不变（避免边各自吸附导致变形）
function snapMoveRect(r) {
  let x = r.x
  let y = r.y
  const vg = []
  const hg = []
  if (Math.abs(x) <= SNAP) { x = 0; vg.push(0) }
  else if (Math.abs(x + r.w - dispW.value) <= SNAP) { x = dispW.value - r.w; vg.push(dispW.value) }
  if (Math.abs(y) <= SNAP) { y = 0; hg.push(0) }
  else if (Math.abs(y + r.h - dispH.value) <= SNAP) { y = dispH.value - r.h; hg.push(dispH.value) }
  return { rect: { x, y, w: r.w, h: r.h }, vg, hg }
}

// 缩放：仅吸附当前手柄所驱动的那条（些）边，其余边保持不动
function snapResizeRect(cand, handle) {
  let x1 = cand.x
  let y1 = cand.y
  let x2 = cand.x + cand.w
  let y2 = cand.y + cand.h
  const vg = []
  const hg = []
  if (handle.indexOf('w') >= 0 && Math.abs(x1) <= SNAP) { x1 = 0; vg.push(0) }
  if (handle.indexOf('e') >= 0 && Math.abs(x2 - dispW.value) <= SNAP) { x2 = dispW.value; vg.push(dispW.value) }
  if (handle.indexOf('n') >= 0 && Math.abs(y1) <= SNAP) { y1 = 0; hg.push(0) }
  if (handle.indexOf('s') >= 0 && Math.abs(y2 - dispH.value) <= SNAP) { y2 = dispH.value; hg.push(dispH.value) }
  return {
    rect: { x: x1, y: y1, w: Math.max(2, x2 - x1), h: Math.max(2, y2 - y1) },
    vg, hg,
  }
}

function startMove(i, e) {
  if (!dispW.value) return
  const p = stageXY(e)
  moving.value = {
    index: i,
    sx: p.x,
    sy: p.y,
    startRect: { ...rects.value[i] },
  }
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
  moving.value = null
  clearGuides()
  syncStore()
}

function clearBoxes() {
  rects.value = []
  dragRect.value = null
  resizing.value = null
  moving.value = null
  clearGuides()
  syncStore()
}

const rectStyle = (r) => ({
  left: r.x + 'px',
  top: r.y + 'px',
  width: r.w + 'px',
  height: r.h + 'px',
})

// 删除按钮默认浮在框右上角外侧；贴近画布边缘时依次退到左侧/下方，避免被画布裁掉
function xBtnStyle(r) {
  const m = 26 // 按钮直径 20 + 6 间隙
  const s = { left: 'auto', right: 'auto', top: 'auto', bottom: 'auto' }
  const spaceRight = dispW.value - (r.x + r.w)
  const spaceAbove = r.y
  const spaceBelow = dispH.value - (r.y + r.h)

  if (spaceRight >= m) s.right = '-26px'
  else if (r.x >= m) s.left = '-26px'
  else s.right = '3px'

  if (spaceAbove >= m) s.top = '-26px'
  else if (spaceBelow >= m) s.bottom = 'calc(100% + 6px)'
  else s.top = '3px'

  return s
}

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
  if (!store.boxes.length) return '水印区域: 未框选（拖拽框选多个，拖动框可移动，接触图像边缘自动吸附）'
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
    moving.value = null
    clearGuides()
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
      <!-- 吸附提示线 -->
      <div
        v-for="(gx, i) in snapGuides.v"
        :key="'vg' + i"
        class="wm-guide v"
        :style="{ left: Math.min(gx, Math.max(0, dispW - 2)) + 'px' }"
      ></div>
      <div
        v-for="(gy, i) in snapGuides.h"
        :key="'hg' + i"
        class="wm-guide h"
        :style="{ top: Math.min(gy, Math.max(0, dispH - 2)) + 'px' }"
      ></div>
      <div
        v-for="(r, i) in rects"
        :key="'r' + i"
        class="wm-rect"
        :class="{ moving: moving && moving.index === i }"
        :style="rectStyle(r)"
        :title="`拖动可移动第 ${i + 1} 个区域`"
        @mousedown.stop.prevent="startMove(i, $event)"
      >
        <button
          class="wm-rect-x"
          :style="xBtnStyle(r)"
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
      <div v-if="!rects.length && !dragRect && src" class="wm-hint">按住左键拖拽框选水印区域（可框选多个；拖动框可移动，接触图像边缘自动吸附）</div>
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
  pointer-events: auto;
  cursor: move;
}
.wm-rect.moving {
  background: rgba(255, 69, 58, 0.28);
  box-shadow: 0 0 0 1px rgba(255, 69, 58, 0.35), 0 4px 14px rgba(255, 69, 58, 0.25);
}
.wm-rect.drag {
  border-style: dashed;
  background: rgba(255, 69, 58, 0.1);
  pointer-events: none;
  cursor: crosshair;
}
.wm-guide {
  position: absolute;
  background: var(--accent, #0a84ff);
  box-shadow: 0 0 6px rgba(10, 132, 255, 0.65);
  pointer-events: none;
  z-index: 3;
  animation: wm-guide-in 0.12s ease-out;
}
.wm-guide.v { width: 2px; top: 0; bottom: 0; }
.wm-guide.h { height: 2px; left: 0; right: 0; }
@keyframes wm-guide-in {
  from { opacity: 0; }
  to { opacity: 1; }
}
.wm-rect-x {
  position: absolute;
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
  z-index: 4;
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
