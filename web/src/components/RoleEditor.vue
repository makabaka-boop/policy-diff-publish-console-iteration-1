<script setup>
// Editor for roles and the acyclic inheritance edges.
// Cap: at most 8 roles; parents are picked from the other roles and
// acyclicity is enforced by the backend (which returns the exact cycle
// error if an edge would close a loop).
defineProps({
  roles: { type: Array, required: true },
})
const emit = defineEmits(['update', 'add-role', 'remove-role'])

function toggleParent(role, parentName) {
  const parents = new Set(role.parents || [])
  if (parents.has(parentName)) parents.delete(parentName)
  else parents.add(parentName)
  role.parents = [...parents]
  emit('update')
}
</script>

<template>
  <section class="card">
    <h2>角色与继承（至多 8 个，无环有向图）</h2>
    <p class="hint">
      勾选表示「行角色继承列角色」的规则；规则选择某祖先角色时，其后代角色同样命中。
      禁止成环与自环。
    </p>
    <div class="role-table-wrap">
      <table class="role-table">
        <thead>
          <tr>
            <th>角色</th>
            <th v-for="r in roles" :key="r.name">{{ r.name }}</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="role in roles" :key="role.name">
            <td><strong>{{ role.name }}</strong></td>
            <td v-for="r in roles" :key="r.name" class="check-cell">
              <input
                v-if="r.name !== role.name"
                type="checkbox"
                :checked="(role.parents || []).includes(r.name)"
                @change="toggleParent(role, r.name)"
                :title="`${role.name} 继承 ${r.name}`"
              />
              <span v-else class="self" title="不可自继承">·</span>
            </td>
            <td>
              <button class="btn btn-small btn-danger" @click="emit('remove-role', role.name)">
                删除
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <button class="btn" :disabled="roles.length >= 8" @click="emit('add-role')">
      + 添加角色（{{ roles.length }}/8）
    </button>
  </section>
</template>
