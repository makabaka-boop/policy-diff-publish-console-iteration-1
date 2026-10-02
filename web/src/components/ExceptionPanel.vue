<script setup>
// 模拟应急例外面板：只针对【已发布】策略中当前确为 deny 的精确
// （角色—资源—操作）元组建立 1–60 分钟的临时放行。生效期间已发布矩阵
// 显示临时 allow + 原规则 deny 证据 + 例外身份；草稿与发布预览永远按
// 纯策略计算，因此这里的任何操作都不会让学员误以为草稿规则已发布。
import { ref, computed, watch, onMounted, onBeforeUnmount } from 'vue'
import { api } from '../api.js'

const props = defineProps({
  // 已发布文档：提供角色/资源/操作的有限域选项。
  publishedDoc: { type: Object, default: null },
  publishedRevision: { type: Number, default: 0 },
  // 外部递增（发布、重置）后重新拉取列表。
  bump: { type: Number, default: 0 },
})
const emit = defineEmits(['created'])

const exceptions = ref([])
const loaded = ref(false)
const loadError = ref('')
const submitting = ref(false)
const formError = ref('')
const formOk = ref('')
const nowMs = ref(Date.now())

// 新例外表单。
const role = ref('')
const resource = ref('')
const action = ref('')
const ttlMinutes = ref(5)
const reason = ref('')

const roles = computed(() => (props.publishedDoc?.roles || []).map((r) => r.name))
const resources = computed(() => props.publishedDoc?.resources || [])
const actions = computed(() => props.publishedDoc?.actions || [])

const tickTimer = setInterval(() => {
  nowMs.value = Date.now()
}, 1000)
onBeforeUnmount(() => clearInterval(tickTimer))

function expiresAtMs(ex) {
  return new Date(ex.expiresAt).getTime()
}
function remainingSeconds(ex) {
  return Math.max(0, Math.ceil((expiresAtMs(ex) - nowMs.value) / 1000))
}
function fmtRemaining(sec) {
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return `${m}:${String(s).padStart(2, '0')}`
}
function fmtClock(iso) {
  const d = new Date(iso)
  return Number.isNaN(d.getTime())
    ? iso
    : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

async function load() {
  loadError.value = ''
  try {
    const data = await api.listExceptions()
    // 只展示绑定到当前已发布修订的例外（正常情况下服务端保证一致）。
    exceptions.value = (data.exceptions || []).filter(
      (ex) => ex.publishedRevision === data.publishedRevision,
    )
    loaded.value = true
  } catch (e) {
    loadError.value = e.message
  }
}

function defaultsIfMissing() {
  if (!role.value && roles.value.length) role.value = roles.value[0]
  if (!resource.value && resources.value.length) resource.value = resources.value[0]
  if (!action.value && actions.value.length) action.value = actions.value[0]
}
watch([roles, resources, actions], defaultsIfMissing, { immediate: true })
watch(() => props.bump, load)
onMounted(load)

async function submit() {
  formError.value = ''
  formOk.value = ''
  if (!role.value || !resource.value || !action.value) {
    formError.value = '请选择一个精确的角色—资源—操作元组。'
    return
  }
  if (!reason.value.trim()) {
    formError.value = '必须填写明确的放行理由。'
    return
  }
  submitting.value = true
  try {
    await api.createException({
      role: role.value,
      resource: resource.value,
      action: action.value,
      ttlMinutes: Number(ttlMinutes.value),
      reason: reason.value.trim(),
    })
    formOk.value = '模拟应急例外已建立：已发布矩阵临时放行，原规则 deny 证据与例外身份同时保留。'
    reason.value = ''
    await load()
    emit('created')
  } catch (e) {
    formError.value = `${e.message}（${e.code || e.status}）`
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <section class="card exception-card">
    <div class="preview-head">
      <h2>
        模拟应急例外（仅已发布）
        <span v-if="publishedRevision" class="rev-badge">已发布 p{{ publishedRevision }}</span>
      </h2>
      <button class="btn btn-small" @click="load">刷新例外</button>
    </div>
    <p class="hint">
      仅可对已发布策略中<b>当前确为 deny</b> 的精确元组建立 1–60 分钟的临时放行。
      生效时矩阵显示临时 allow，同时保留原规则 deny 证据与例外身份；
      <b>草稿决策与发布预览不受影响</b>，不代表草稿规则已发布。到期自动恢复，发布新策略或重置演示数据时立即失效。
    </p>

    <div v-if="loadError" class="banner banner-err">{{ loadError }}</div>

    <div class="exception-form">
      <select v-model="role" class="text-input" aria-label="角色">
        <option v-for="r in roles" :key="r" :value="r">{{ r }}</option>
      </select>
      <select v-model="resource" class="text-input" aria-label="资源">
        <option v-for="r in resources" :key="r" :value="r">{{ r }}</option>
      </select>
      <select v-model="action" class="text-input" aria-label="操作">
        <option v-for="a in actions" :key="a" :value="a">{{ a }}</option>
      </select>
      <label class="ttl-line">
        期限
        <input v-model.number="ttlMinutes" type="number" min="1" max="60" class="text-input ttl-input" />
        分钟（1–60）
      </label>
    </div>
    <div class="exception-form">
      <input
        v-model="reason"
        class="text-input reason-input"
        type="text"
        maxlength="300"
        placeholder="明确理由，例如：演练第 3 步，教员临时放行 billing/read 5 分钟演示审计流程"
      />
      <button class="btn btn-primary" :disabled="submitting" @click="submit">
        {{ submitting ? '裁决中…' : '建立临时放行' }}
      </button>
    </div>
    <div v-if="formError" class="banner banner-err">{{ formError }}</div>
    <div v-if="formOk" class="banner banner-ok">{{ formOk }}</div>

    <h3>生效中的例外（{{ exceptions.length }}）</h3>
    <p v-if="loaded && exceptions.length === 0" class="hint">无。已发布矩阵完全按发布规则裁决。</p>
    <div
      v-for="ex in exceptions"
      :key="ex.id"
      class="diff-entry allow-bg exception-entry"
      :class="{ expired: remainingSeconds(ex) === 0 }"
    >
      <div class="exception-row">
        <span class="diff-tuple">{{ ex.tuple.role }} / {{ ex.tuple.resource }} / {{ ex.tuple.action }}</span>
        <span class="exc-meta">
          <code :title="ex.id">{{ ex.id }}</code>
          · 绑定 p{{ ex.publishedRevision }}
          · 剩余 <b>{{ fmtRemaining(remainingSeconds(ex)) }}</b>
          · {{ fmtClock(ex.expiresAt) }} 到期
        </span>
      </div>
      <div class="hint">理由：{{ ex.reason }}</div>
      <div v-if="remainingSeconds(ex) === 0" class="banner banner-warn expired-note">
        已到期：下次读取即恢复原 deny（无需后台任务），点击「刷新例外」可从列表移除。
      </div>
    </div>
  </section>
</template>
