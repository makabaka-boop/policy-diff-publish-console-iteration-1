<script setup>
// Rule table: one row per rule. Role / resource / action selectors may
// be the "*" wildcard. Priority is an integer; ties are resolved deny.
import { computed } from 'vue'

const props = defineProps({
  rules: { type: Array, required: true },
  roles: { type: Array, required: true },
  resources: { type: Array, required: true },
  actions: { type: Array, required: true },
})
const emit = defineEmits(['update'])

let idCounter = 1000
function newId() {
  idCounter += 1
  return `r-${Date.now().toString(36)}-${idCounter}`
}

const sorted = computed(() =>
  [...props.rules].sort((a, b) => b.priority - a.priority || a.id.localeCompare(b.id)),
)

function addRule() {
  props.rules.push({
    id: newId(),
    role: '*',
    resource: '*',
    action: '*',
    priority: 10,
    effect: 'allow',
  })
  emit('update')
}
function removeRule(id) {
  const i = props.rules.findIndex((r) => r.id === id)
  if (i >= 0) props.rules.splice(i, 1)
  emit('update')
}
function changed() {
  emit('update')
}
</script>

<template>
  <section class="card">
    <h2>规则（优先级最高组决定；同优先级 deny 胜出；未命中默认 deny）</h2>
    <div class="rule-table-wrap">
      <table class="rule-table">
        <thead>
          <tr>
            <th>ID</th>
            <th>角色</th>
            <th>资源类别</th>
            <th>操作</th>
            <th>优先级</th>
            <th>效果</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in sorted" :key="r.id">
            <td class="rule-id" :title="r.id">{{ r.id }}</td>
            <td>
              <select v-model="r.role" @change="changed">
                <option value="*">* (任意)</option>
                <option v-for="role in roles" :key="role.name" :value="role.name">{{ role.name }}</option>
              </select>
            </td>
            <td>
              <select v-model="r.resource" @change="changed">
                <option value="*">* (任意)</option>
                <option v-for="res in resources" :key="res" :value="res">{{ res }}</option>
              </select>
            </td>
            <td>
              <select v-model="r.action" @change="changed">
                <option value="*">* (任意)</option>
                <option v-for="a in actions" :key="a" :value="a">{{ a }}</option>
              </select>
            </td>
            <td>
              <input v-model.number="r.priority" type="number" min="0" max="1000000"
                class="priority-input" @change="changed" />
            </td>
            <td>
              <div class="effect-toggle">
                <label :class="['seg', r.effect === 'allow' ? 'seg-allow on' : 'seg-allow']">
                  <input type="radio" :name="`eff-${r.id}`" value="allow" v-model="r.effect"
                    @change="changed" />allow
                </label>
                <label :class="['seg', r.effect === 'deny' ? 'seg-deny on' : 'seg-deny']">
                  <input type="radio" :name="`eff-${r.id}`" value="deny" v-model="r.effect"
                    @change="changed" />deny
                </label>
              </div>
            </td>
            <td>
              <button class="btn btn-small btn-danger" @click="removeRule(r.id)">删除</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <button class="btn" @click="addRule">+ 添加规则（{{ rules.length }}）</button>
  </section>
</template>
