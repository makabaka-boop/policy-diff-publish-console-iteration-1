<script setup>
import { ref, computed, onMounted } from 'vue'
import { api } from './api.js'
import RoleEditor from './components/RoleEditor.vue'
import DomainEditor from './components/DomainEditor.vue'
import RuleEditor from './components/RuleEditor.vue'
import PreviewPanel from './components/PreviewPanel.vue'
import DecisionMatrix from './components/DecisionMatrix.vue'

const notice = ref('')
const draftDoc = ref(null)
const draftRevision = ref(0)
const publishedRevision = ref(0)
const publishedDoc = ref(null)

const preview = ref(null) // { summary, draftRevision, publishedRevision }
const busy = ref(false)
const publishing = ref(false)
const message = ref(null) // { kind: 'ok' | 'err', text }
const matrixBump = ref(0)
const matrixVersion = ref('draft')

let roleNameCounter = 0

const dirty = ref(false)
const previewStale = computed(() => {
  if (!preview.value) return false
  return preview.value.draftRevision !== draftRevision.value ||
    preview.value.publishedRevision !== publishedRevision.value
})
const canPublish = computed(() => !!preview.value && !previewStale.value && !busy.value)

function flash(kind, text) {
  message.value = { kind, text }
}

async function loadState() {
  const st = await api.state()
  notice.value = st.notice
  draftDoc.value = st.draft.document
  draftRevision.value = st.draft.revision
  publishedDoc.value = st.published.document
  publishedRevision.value = st.published.revision
  dirty.value = false
  matrixBump.value++
}

function markDirty() {
  dirty.value = true
  // A preview cannot stay valid while the draft content mutates locally.
  // It is only formally stale server-side after saving, but the UI warns
  // immediately; the authoritative rejection happens in Publish().
}

function addRole() {
  roleNameCounter += 1
  let name = `role${roleNameCounter}`
  const existing = new Set(draftDoc.value.roles.map((r) => r.name))
  let i = roleNameCounter
  while (existing.has(name)) {
    i += 1
    name = `role${i}`
  }
  draftDoc.value.roles.push({ name, parents: [] })
  markDirty()
}
function removeRole(name) {
  draftDoc.value.roles = draftDoc.value.roles.filter((r) => r.name !== name)
  for (const r of draftDoc.value.roles) {
    r.parents = (r.parents || []).filter((p) => p !== name)
  }
  draftDoc.value.rules = draftDoc.value.rules.filter(
    (rl) => rl.role !== name,
  )
  markDirty()
}

async function saveDraft() {
  busy.value = true
  try {
    const res = await api.saveDraft(draftDoc.value)
    draftRevision.value = res.draft.revision
    draftDoc.value = res.draft.document
    dirty.value = false
    flash('ok', `草稿已保存（修订 r${res.draft.revision}）；旧预览立即失效，需重新生成。`)
    matrixBump.value++
  } catch (e) {
    flash('err', `保存被拒绝：${e.message}`)
  } finally {
    busy.value = false
  }
}

async function makePreview() {
  busy.value = true
  try {
    preview.value = await api.preview()
    matrixBump.value++
    if (preview.value.draftRevision !== draftRevision.value) {
      // State drifted in another tab; resync.
      await loadState()
      preview.value = await api.preview()
    }
    flash('ok', '预览已生成：摘要指纹绑定当前草稿/已发布修订对。')
  } catch (e) {
    flash('err', `预览失败：${e.message}`)
  } finally {
    busy.value = false
  }
}

async function publish() {
  if (!preview.value || previewStale.value) {
    flash('err', '预览不存在或已过期，无法发布。')
    return
  }
  publishing.value = true
  busy.value = true
  try {
    const res = await api.publish({
      draftRevision: preview.value.draftRevision,
      publishedRevision: preview.value.publishedRevision,
      summary: preview.value.summary,
    })
    flash('ok', `发布成功：新已发布修订 p${res.published.revision}`)
    await loadState()
    // The published content is now the just-published draft; regenerate
    // the preview so its revision pair reflects reality.
    preview.value = await api.preview()
    matrixVersion.value = 'published'
    matrixBump.value++
  } catch (e) {
    if (e.status === 409) {
      flash('err', `发布被拒绝（并发或过期）：${e.message}。请重新加载状态并重新预览。`)
    } else {
      flash('err', `发布被拒绝：${e.message}`)
    }
    await loadState()
  } finally {
    publishing.value = false
    busy.value = false
  }
}

async function resetDemo() {
  busy.value = true
  try {
    const st = await api.reset()
    notice.value = st.notice
    draftDoc.value = st.draft.document
    draftRevision.value = st.draft.revision
    publishedDoc.value = st.published.document
    publishedRevision.value = st.published.revision
    preview.value = null
    dirty.value = false
    matrixBump.value++
    flash('ok', '已重置为内置菱形继承演示策略。')
  } catch (e) {
    flash('err', e.message)
  } finally {
    busy.value = false
  }
}

onMounted(loadState)
</script>

<template>
  <div class="page">
    <header class="topbar">
      <h1>有限域访问策略模拟器</h1>
      <div class="topline">
        <span class="rev-chip">草稿 r{{ draftRevision }}</span>
        <span class="rev-chip published">已发布 p{{ publishedRevision }}</span>
        <button class="btn btn-small" :disabled="busy" @click="resetDemo">重置演示数据</button>
      </div>
    </header>

    <div class="banner banner-sim">{{ notice || '本工具仅用于策略推演与教学，不是任何真实系统的鉴权入口。' }}</div>

    <div v-if="message" :class="['banner', message.kind === 'ok' ? 'banner-ok' : 'banner-err']">
      {{ message.text }}
    </div>

    <div v-if="!draftDoc" class="card">加载中…</div>

    <template v-else>
      <div class="action-bar">
        <button class="btn btn-primary" :disabled="busy || !dirty" @click="saveDraft">
          保存草稿（会使旧预览失效）
        </button>
        <span v-if="dirty" class="dirty-dot">有未保存修改</span>
      </div>

      <div class="grid-2">
        <RoleEditor
          :roles="draftDoc.roles"
          @update="markDirty"
          @add-role="addRole"
          @remove-role="removeRole"
        />
        <DomainEditor
          :resources="draftDoc.resources"
          :actions="draftDoc.actions"
          @update="markDirty"
        />
      </div>

      <RuleEditor
        :rules="draftDoc.rules"
        :roles="draftDoc.roles"
        :resources="draftDoc.resources"
        :actions="draftDoc.actions"
        @update="markDirty"
      />

      <PreviewPanel
        :preview="preview"
        :stale="previewStale"
        :publishing="publishing"
        :can-publish="canPublish"
        @refresh="makePreview"
        @publish="publish"
      />

      <div class="tabs">
        <button :class="['tab', matrixVersion === 'draft' && 'on']" @click="matrixVersion = 'draft'">
          草稿矩阵
        </button>
        <button :class="['tab', matrixVersion === 'published' && 'on']" @click="matrixVersion = 'published'">
          已发布矩阵
        </button>
      </div>
      <DecisionMatrix :version="matrixVersion" :bump="matrixBump" />
    </template>

    <footer class="footer">
      所有判定只发生在本页枚举的有限集合内；默认 deny、同优先级 deny 胜出、高优先级覆盖。
    </footer>
  </div>
</template>
