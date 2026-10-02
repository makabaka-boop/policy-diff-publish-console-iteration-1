<script setup>
// Preview panel: exhaustive draft-vs-published comparison with evidence,
// plus the gated publish control. The publish request echoes the exact
// summary object returned by the server; the server rejects any revision
// drift or hash mismatch.
import { computed } from 'vue'

const props = defineProps({
  preview: { type: Object, default: null },
  stale: { type: Boolean, default: false },
  publishing: { type: Boolean, default: false },
  canPublish: { type: Boolean, default: false },
})
const emit = defineEmits(['publish', 'refresh'])

const shortHash = computed(() => (props.preview?.summary?.hash || '').slice(0, 12))

function tupleLabel(t) {
  return `${t.role} / ${t.resource} / ${t.action}`
}
function winnersText(ev) {
  if (!ev.winners || ev.winners.length === 0) return '未命中任何规则（默认 deny）'
  return ev.winners.map((w) => `${w.ruleId}[p${w.priority}/${w.effect}]`).join('，')
}
const reasonText = {
  'single-winner': '单一最高优先级规则决定',
  'tie-deny': '同优先级 allow/deny 冲突 → deny 胜出',
  'no-match-default-deny': '无规则命中 → 默认 deny',
}
</script>

<template>
  <section class="card preview-card" :class="{ stale }">
    <div class="preview-head">
      <h2>发布预览（穷举对比）</h2>
      <button class="btn btn-small" @click="emit('refresh')">
        {{ preview ? '重新生成预览' : '生成预览' }}
      </button>
    </div>

    <div v-if="!preview" class="empty">
      尚未生成预览。保存草稿后点击「生成预览」，后端将穷举当前有限决策域并对比已发布版本。
    </div>

    <template v-else>
      <div v-if="stale" class="banner banner-warn">
        ⚠ 此预览已过期：草稿或已发布版本在预览后发生变化，发布将被拒绝。请重新生成预览。
      </div>

      <div class="rev-line">
        基于草稿修订 <code>r{{ preview.draftRevision }}</code>
        对比已发布修订 <code>p{{ preview.publishedRevision }}</code>
        · 摘要指纹 <code :title="preview.summary.hash">{{ shortHash }}…</code>
      </div>

      <div class="stat-grid">
        <div class="stat">
          <div class="stat-num">{{ preview.summary.tupleCount }}</div>
          <div class="stat-label">穷举元组</div>
        </div>
        <div class="stat stat-allow">
          <div class="stat-num">{{ preview.summary.allowCount }}</div>
          <div class="stat-label">草稿允许</div>
        </div>
        <div class="stat stat-deny">
          <div class="stat-num">{{ preview.summary.denyCount }}</div>
          <div class="stat-label">草稿拒绝</div>
        </div>
        <div class="stat">
          <div class="stat-num">{{ preview.summary.unchanged }}</div>
          <div class="stat-label">无变化</div>
        </div>
      </div>

      <div class="diff-cols">
        <div class="diff-col">
          <h3 class="allow-title">新增允许（{{ preview.summary.newAllows.length }}）</h3>
          <p v-if="preview.summary.newAllows.length === 0" class="hint">无</p>
          <div v-for="e in preview.summary.newAllows" :key="'a' + tupleLabel(e.tuple)" class="diff-entry allow-bg">
            <div class="diff-tuple">{{ tupleLabel(e.tuple) }}</div>
            <div class="diff-evidence">
              <div><span class="tag deny-tag">发布版</span> {{ reasonText[e.before.reason] }}</div>
              <div class="rule-ev">{{ winnersText(e.before) }}</div>
              <div><span class="tag allow-tag">草稿</span> {{ reasonText[e.after.reason] }}</div>
              <div class="rule-ev">{{ winnersText(e.after) }}</div>
            </div>
          </div>
        </div>

        <div class="diff-col">
          <h3 class="deny-title">新增拒绝（{{ preview.summary.newDenies.length }}）</h3>
          <p v-if="preview.summary.newDenies.length === 0" class="hint">无</p>
          <div v-for="e in preview.summary.newDenies" :key="'d' + tupleLabel(e.tuple)" class="diff-entry deny-bg">
            <div class="diff-tuple">{{ tupleLabel(e.tuple) }}</div>
            <div class="diff-evidence">
              <div><span class="tag allow-tag">发布版</span> {{ reasonText[e.before.reason] }}</div>
              <div class="rule-ev">{{ winnersText(e.before) }}</div>
              <div><span class="tag deny-tag">草稿</span> {{ reasonText[e.after.reason] }}</div>
              <div class="rule-ev">{{ winnersText(e.after) }}</div>
            </div>
          </div>
        </div>
      </div>

      <div class="publish-row">
        <button class="btn btn-primary" :disabled="!canPublish || publishing" @click="emit('publish')">
          {{ publishing ? '发布中…' : '携带预览发布' }}
        </button>
        <span v-if="!canPublish" class="hint">
          发布请求会同时提交草稿修订、已发布修订与完整预览摘要（含指纹），任一方变化即被拒绝。
        </span>
      </div>
    </template>
  </section>
</template>
