<script setup>
// Resource classes (max 8) and actions (max 6): the concrete finite
// universe the engine enumerates. Wildcards exist only in rule
// selectors, never as universe members.
import { ref } from 'vue'

const props = defineProps({
  resources: { type: Array, required: true },
  actions: { type: Array, required: true },
})
const emit = defineEmits(['update'])

const newResource = ref('')
const newAction = ref('')

function addResource() {
  const name = newResource.value.trim()
  if (!name || props.resources.includes(name)) return
  props.resources.push(name)
  newResource.value = ''
  emit('update')
}
function addAction() {
  const name = newAction.value.trim()
  if (!name || props.actions.includes(name)) return
  props.actions.push(name)
  newAction.value = ''
  emit('update')
}
function removeResource(name) {
  const i = props.resources.indexOf(name)
  if (i >= 0) props.resources.splice(i, 1)
  emit('update')
}
function removeAction(name) {
  const i = props.actions.indexOf(name)
  if (i >= 0) props.actions.splice(i, 1)
  emit('update')
}
</script>

<template>
  <section class="card">
    <h2>有限决策域</h2>
    <p class="hint">
      后端穷举「角色 × 资源类别 × 操作」的全部元组：当前为
      <strong>{{ resources.length * actions.length }} 个资源×操作组合</strong>
      （再乘角色数即为元组总数）。
    </p>

    <div class="domain-cols">
      <div>
        <h3>资源类别（{{ resources.length }}/8）</h3>
        <div class="chip-row">
          <span v-for="r in resources" :key="r" class="chip">
            {{ r }}
            <button class="chip-x" title="删除" @click="removeResource(r)">×</button>
          </span>
        </div>
        <input
          v-model="newResource"
          class="text-input"
          placeholder="新资源名，如 secret"
          :disabled="resources.length >= 8"
          @keyup.enter="addResource"
        />
        <button class="btn btn-small" :disabled="resources.length >= 8" @click="addResource">
          添加资源
        </button>
      </div>

      <div>
        <h3>操作（{{ actions.length }}/6）</h3>
        <div class="chip-row">
          <span v-for="a in actions" :key="a" class="chip">
            {{ a }}
            <button class="chip-x" title="删除" @click="removeAction(a)">×</button>
          </span>
        </div>
        <input
          v-model="newAction"
          class="text-input"
          placeholder="新操作，如 approve"
          :disabled="actions.length >= 6"
          @keyup.enter="addAction"
        />
        <button class="btn btn-small" :disabled="actions.length >= 6" @click="addAction">
          添加操作
        </button>
      </div>
    </div>
  </section>
</template>
