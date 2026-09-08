<script setup>
import { store } from '../store.js'

const props = defineProps({
  images: { type: Array, default: () => [] }, // [{path,thumb,name}]
  selectable: { type: Boolean, default: false }, // 左上角勾选框选中；点击图片=预览
  zoomable: { type: Boolean, default: false }, // 点击放大查看（结果页用）
  activePath: { type: String, default: '' }, // 当前预览中的图片（高亮）
})
const emit = defineEmits(['toggle', 'preview', 'view'])

function isSelected(path) {
  return props.selectable && store.selected.includes(path)
}
function onCheck(path) {
  const i = store.selected.indexOf(path)
  if (i >= 0) store.selected.splice(i, 1)
  else store.selected.push(path)
  emit('toggle', path)
}
function onTile(path) {
  if (props.selectable) emit('preview', path)
  else if (props.zoomable) emit('view', path)
}
</script>

<template>
  <div class="grid">
    <div
      v-for="(img, i) in images"
      :key="img.path"
      class="tile"
      :class="{ selected: isSelected(img.path), active: props.activePath === img.path }"
      :style="{ animationDelay: Math.min(i * 40, 400) + 'ms' }"
      @click="onTile(img.path)"
    >
      <img v-if="img.thumb" :src="img.thumb" :alt="img.name" loading="lazy" draggable="false" />
      <div v-else class="ph" :title="img.name">无法预览</div>
      <button
        v-if="selectable"
        class="check"
        :class="{ checked: isSelected(img.path) }"
        :title="isSelected(img.path) ? '取消勾选' : '勾选（用于仅勾选处理）'"
        @click.stop="onCheck(img.path)"
      >
        <span v-if="isSelected(img.path)">✓</span>
      </button>
      <div v-if="zoomable" class="zoom-hint">🔍</div>
      <div class="name">{{ img.name }}</div>
    </div>
  </div>
</template>

<style scoped>
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
