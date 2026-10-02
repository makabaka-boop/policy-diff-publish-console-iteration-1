<script setup>
// Finite-domain decision grid for one revision. Rows are
// role/resource pairs, columns are actions; a cell is allow (green) or
// deny (red), and clicking a cell shows the full evidence chain.
//
// In the PUBLISHED matrix only, a pure rule-deny cell can be granted a
// simulated emergency exception. The temporary-allow colour, the
// original deny evidence and the exception identity always come from
// the SAME server response (one row object), so the page can never
// show a released cell next to pure-rule-deny evidence.
import { ref, watch, computed } from 'vue'
import { api } from '../api.js'

const props = defineProps({
  version: { type: String, default: 'draft' }, // 'draft' | 'published'
  bump: { type: Number, default: 0 },          // increment to refetch
  // Current published revision + server clock, owned by the parent so
  // the create payload and countdowns agree with the rest of the page.
  publishedRevision: { type: Number, default: 0 },
  serverNow: { type: String, default: '' },
  nowTick: { type: Number, default: 0 },
})
const emit = defineEmits(['exception-changed'])

const rows = ref([])
const revision = ref(null)
const loading = ref(false)
const error = ref('')
const selected = ref(null)

// create-form state, scoped to the selected cell.
const form = ref({ reason: '', ttlMinutes: 15 })
const creating = ref(false)
const formError = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const data = await api.decisions(props.version)
    rows.value = data.rows
    revision.value = data.revision
    selected.value = null
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}
watch(() => [props.version, props.bump], load, { immediate: true })
defineExpose({ reload: load })

const actions = computed(() => {
  const set = new Set()
  for (const r of rows.value) set.add(r.tuple.action)
  return [...set]
})
const grouped = computed(() => {
  const map = new Map()
  for (const r of rows.value) {
    const key = `${r.tuple.role} ${r.tuple.resource}`
    if (!map.has(key)) map.set(key, { role: r.tuple.role, resource: r.tuple.resource, cells: {} })
    map.get(key).cells[r.tuple.action] = r
  }
  return [...map.values()]
})

function cellClass(row) {
  if (!row) return ''
  if (row.evidence.decision === 'allow') {
    return row.exception ? 'cell-allow cell-exception' : 'cell-allow'
  }
  return 'cell-deny'
}

function select(g, action) {
  const row = g.cells[action]
  selected.value = {
    tuple: row.tuple,
    evidence: row.evidence,
    exception: row.exception || null,
  }
  form.value = { reason: '', ttlMinutes: 15 }
  formError.value = ''
}

const isPublished = computed(() => props.version === 'published')
// The exception form is offered only for a published, currently-denied
// tuple without an active exception.
const canCreate = computed(() => {
  if (!selected.value || !isPublished.value) return false
  return selected.value.evidence.decision === 'deny' && !selected.value.exception
})
const revisionMismatch = computed(() =>
  isPublished.value && props.publishedRevision !== 0 && revision.value !== null &&
  revision.value !== props.publishedRevision,
)

const reasonText = {
  'single-winner': '最高优先级规则决定',
  'tie-deny': '同优先级冲突，deny 胜出',
  'no-match-default-deny': '无命中，默认 deny',
  'emergency-exception-allow': '模拟应急例外临时放行（不是规则允许）',
}

// remaining computes the countdown from the server-anchored clock.
function remaining(ex) {
  if (!ex) return ''
  void props.nowTick // repaint countdown every second
  // Anchor local time to the last known server time: drift-corrected
  // expiry countdown without trusting the browser clock alone.
  const base = props.serverNow ? new Date(props.serverNow).getTime() : Date.now()
  const elapsed = props.serverNow ? Date.now() - base : 0
  const ms = new Date(ex.expiresAt).getTime() - base - elapsed
  if (ms <= 0) return '已到期（刷新即恢复原裁决）'
  const total = Math.floor(ms / 1000)
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m} 分 ${String(s).padStart(2, '0')} 秒`
}

async function submitException() {
  if (!form.value.reason.trim()) {
    formError.value = '必须填写明确的放行理由。'
    return
  }
  if (form.value.ttlMinutes < 1 || form.value.ttlMinutes > 60) {
    formError.value = '期限必须在 1–60 分钟之间。'
    return
  }
  creating.value = true
  formError.value = ''
  // Capture before load(): a successful create refetches the matrix,
  // which clears the selection.
  const target = { ...selected.value.tuple }
  try {
    await api.createException({
      tuple: target,
      reason: form.value.reason.trim(),
      ttlMinutes: form.value.ttlMinutes,
      // Pin the request to the revision whose evidence the admin is
      // looking at; the server refuses it if a publish happened in
      // between, so a late request can never bind to a new revision.
      publishedRevision: revision.value,
    })
    await load()
    emit('exception-changed')
    // Re-open the same cell so the created override is visible.
    const fresh = rows.value.find(
      (r) => r.tuple.role === target.role && r.tuple.resource === target.resource && r.tuple.action === target.action,
    )
    if (fresh) {
      selected.value = { tuple: fresh.tuple, evidence: fresh.evidence, exception: fresh.exception || null }
    }
  } catch (e) {
    if (e.code === 'published_moved') {
      formError.value = '已发布修订已变化（有新发布或重置）：请刷新矩阵后基于新修订重新裁决，例外不会附着到新修订。'
    } else if (e.code === 'tuple_not_denied') {
      formError.value = '该元组在当前已发布策略下并非 deny，整次请求被拒绝。请刷新矩阵。'
    } else if (e.code === 'exception_exists') {
      formError.value = '该元组已存在生效中的应急例外。'
    } else {
      formError.value = e.message
    }
  } finally {
    creating.value = false
  }
}
</script>

<template>
  <section class="card">
    <div class="preview-head">
      <h2>
        决策矩阵 · {{ version === 'draft' ? '草稿' : '已发布' }}
        <span v-if="revision !== null" class="rev-badge">rev {{ revision }}</span>
      </h2>
      <button class="btn btn-small" @click="load">刷新</button>
    </div>
    <p v-if="loading" class="hint">穷举中…</p>
    <p v-else-if="error" class="banner banner-err">{{ error }}</p>
    <p v-else-if="revisionMismatch" class="banner banner-warn">
      ⚠ 本矩阵基于已发布 p{{ revision }}，页面其他部分已是 p{{ publishedRevision }}，请刷新。
    </p>
    <template v-else>
      <p v-if="isPublished" class="hint exception-legend">
        图例：<span class="swatch cell-allow">✓</span> 规则允许 ·
        <span class="swatch cell-deny">✗</span> 规则拒绝 ·
        <span class="swatch cell-exception">✓</span> 应急例外临时放行（证据保留原 deny）
      </p>
      <div class="matrix-wrap">
        <table class="matrix">
          <thead>
            <tr>
              <th>角色</th><th>资源</th>
              <th v-for="a in actions" :key="a">{{ a }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="g in grouped" :key="g.role + g.resource">
              <td>{{ g.role }}</td>
              <td>{{ g.resource }}</td>
              <td v-for="a in actions" :key="a"
                  class="cell"
                  :class="cellClass(g.cells[a])"
                  @click="select(g, a)">
                {{ g.cells[a]?.evidence.decision === 'allow' ? '✓' : '✗' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="selected" class="evidence-box">
        <div class="evidence-head">
          <strong>{{ selected.tuple.role }} / {{ selected.tuple.resource }} / {{ selected.tuple.action }}</strong>
          <button class="btn btn-small" @click="selected = null">关闭</button>
        </div>

        <!-- Active emergency exception: the single source of truth is
             this row: allow colour + exception id + original deny chain. -->
        <div v-if="selected.exception" class="exception-block">
          <div class="exception-title">
            🚑 应急例外临时放行（模拟，非已发布规则）
          </div>
          <div class="exception-meta">
            <div>例外身份：<code>{{ selected.exception.id }}</code>（绑定已发布 p{{ selected.exception.publishedRevision }}）</div>
            <div>理由：{{ selected.exception.reason }}</div>
            <div>到期：{{ new Date(selected.exception.expiresAt).toLocaleString() }} · 剩余 {{ remaining(selected.exception) }}</div>
          </div>
          <div class="hint">
            下列是被例外覆盖的<span class="deny-text">原已发布 deny 证据</span>，到期或发布新版本后立即恢复该裁决：
          </div>
        </div>

        <div>
          当前结论：
          <span :class="selected.evidence.decision === 'allow' ? 'allow-text' : 'deny-text'">
            {{ selected.evidence.decision.toUpperCase() }}
          </span>
          （{{ reasonText[selected.evidence.reason] || selected.evidence.reason }}）
          <span v-if="selected.evidence.exceptionId" class="tag ex-tag">例外 {{ selected.evidence.exceptionId }}</span>
          <template v-if="selected.exception">
            <br /><span class="hint">被覆盖的规则裁决理由：{{ reasonText[selected.evidence.originalReason] }}</span>
          </template>
        </div>
        <div v-if="selected.evidence.considered.length" class="considered">
          <div class="hint">参与匹配的规则（按优先级降序）：</div>
          <ul>
            <li v-for="c in selected.evidence.considered" :key="c.ruleId"
                :class="{ winner: selected.evidence.winners.some((w) => w.ruleId === c.ruleId) }">
              {{ c.ruleId }} · p{{ c.priority }} · {{ c.effect }}
            </li>
          </ul>
        </div>
        <div v-else class="hint">无任何规则命中该元组（默认 deny 证据同样保留）。</div>

        <!-- Create form: published deny tuples only. -->
        <div v-if="canCreate" class="exception-form">
          <div class="exception-form-title">为这个 deny 元组创建模拟应急例外</div>
          <p class="hint">
            服务端会在当前已发布有限域内重新裁决：只有此刻确实 deny 的精确元组才能建立；
            草稿与发布预览不受影响。例外立即随发布/重置失效，到期自动恢复，无需后台任务。
          </p>
          <label class="form-row">
            <span>明确理由（必填）</span>
            <textarea v-model="form.reason" rows="2" maxlength="300"
              placeholder="例如：演练中临时让该角色读取该资源 10 分钟，事后复盘。"></textarea>
          </label>
          <label class="form-row inline">
            <span>期限（1–60 分钟）</span>
            <input v-model.number="form.ttlMinutes" type="number" min="1" max="60" />
          </label>
          <div class="form-actions">
            <button class="btn btn-primary btn-small" :disabled="creating" @click="submitException">
              {{ creating ? '裁决中…' : '重新裁决并建立例外' }}
            </button>
          </div>
          <p v-if="formError" class="banner banner-err">{{ formError }}</p>
        </div>
      </div>
    </template>
  </section>
</template>
