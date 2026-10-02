<script setup>
// Lists the currently active simulated emergency exceptions with a
// server-anchored countdown. The list is authoritative server state:
// expiry is lazy on the server, so when a countdown reaches zero a
// refresh is enough to see the original verdict restored.
import { computed } from 'vue'

const props = defineProps({
  exceptions: { type: Array, default: () => [] },
  publishedRevision: { type: Number, default: 0 },
  serverNow: { type: String, default: '' },
  nowTick: { type: Number, default: 0 },
  loading: { type: Boolean, default: false },
})
const emit = defineEmits(['refresh'])

function tupleLabel(t) {
  return `${t.role} / ${t.resource} / ${t.action}`
}
function fmt(ts) {
  return new Date(ts).toLocaleString()
}
function remaining(ex) {
  // Referenced so the countdown repaints on every tick.
  void props.nowTick
  const base = props.serverNow ? new Date(props.serverNow).getTime() : Date.now()
  const elapsed = props.serverNow ? Date.now() - base : 0
  const ms = new Date(ex.expiresAt).getTime() - base - elapsed
  if (ms <= 0) return '已到期'
  const total = Math.floor(ms / 1000)
  return `${Math.floor(total / 60)} 分 ${String(total % 60).padStart(2, '0')} 秒`
}
const stalePinned = computed(() =>
  props.exceptions.some((e) => e.publishedRevision !== props.publishedRevision),
)
</script>

<template>
  <section class="card exceptions-card">
    <div class="preview-head">
      <h2>模拟应急例外（生效中 {{ exceptions.length }}）</h2>
      <button class="btn btn-small" :disabled="loading" @click="emit('refresh')">刷新例外</button>
    </div>
    <p class="hint">
      应急例外只影响<b>已发布</b>矩阵的一个精确元组，不是草稿规则、不会出现在发布预览里；
      发布新策略或重置演示数据会立即使其失效。
    </p>
    <p v-if="stalePinned" class="banner banner-warn">
      ⚠ 列表中存在绑定到旧修订的例外，刷新后即清除。
    </p>
    <p v-if="exceptions.length === 0" class="empty">当前没有生效中的应急例外。</p>
    <div v-else class="ex-list">
      <div v-for="ex in exceptions" :key="ex.id" class="ex-item">
        <div class="ex-head">
          <span class="tag ex-tag">{{ ex.id }}</span>
          <strong>{{ tupleLabel(ex.tuple) }}</strong>
          <span class="hint">绑定 p{{ ex.publishedRevision }}</span>
          <span class="ex-ttl">剩余 {{ remaining(ex) }}</span>
        </div>
        <div class="ex-reason">理由：{{ ex.reason }}</div>
        <div class="hint">创建 {{ fmt(ex.createdAt) }} · 到期 {{ fmt(ex.expiresAt) }}</div>
      </div>
    </div>
  </section>
</template>
