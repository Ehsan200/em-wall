<script lang="ts" setup>
import { ref, computed } from 'vue';
import { AddCustomGroup, UpdateCustomGroup, DeleteRule } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';
import SearchSelect from './SearchSelect.vue';

// Moves the selected rules into a custom group: their patterns are added to
// the group definition, then the standalone rules are deleted. The group's
// Apply re-creates rules (with a freshly chosen binding) when needed.
const props = defineProps<{
  rules: ipc.RuleDTO[];           // selected rules to move
  customGroups: ipc.GroupDTO[];   // existing custom groups (targets)
}>();
const emit = defineEmits<{ (e: 'done'): void; (e: 'close'): void }>();

// target: an existing custom group key, or '' meaning "new group".
const target = ref<string>(props.customGroups[0]?.key ?? '');
const newName = ref<string>('');
const newColor = ref<string>('#6c5ce7');
const busy = ref(false);
const error = ref('');

const patterns = computed<string[]>(() => {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const r of props.rules) {
    if (seen.has(r.pattern)) continue;
    seen.add(r.pattern);
    out.push(r.pattern);
  }
  return out;
});

// Existing custom groups plus a trailing "new group" row.
const targetOptions = computed(() => [
  ...props.customGroups.map(g => ({ value: g.key, label: g.displayName })),
  { value: '__new__', label: '+ New group…' },
]);

const isNew = computed(() => target.value === '__new__' || props.customGroups.length === 0);
const valid = computed(() => {
  if (patterns.value.length === 0) return false;
  if (isNew.value) return newName.value.trim().length > 0;
  return target.value.length > 0;
});

async function confirm() {
  if (busy.value || !valid.value) return;
  busy.value = true;
  error.value = '';
  try {
    if (isNew.value) {
      await AddCustomGroup('', newName.value.trim(), '', patterns.value, newColor.value);
    } else {
      const g = props.customGroups.find(x => x.key === target.value);
      if (!g) { error.value = 'Group not found.'; return; }
      const merged = Array.from(new Set([...g.patterns, ...patterns.value]));
      await UpdateCustomGroup(g.key, g.displayName, g.description, merged, g.color);
    }
    // Delete the moved rules (binding is intentionally dropped — re-apply
    // the group to recreate them).
    for (const r of props.rules) {
      await DeleteRule(r.id);
    }
    emit('done');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}
</script>

<template>
  <div class="overlay" @click.self="emit('close')">
    <div class="dialog">
      <h3 style="margin: 0 0 4px">Move {{ rules.length }} rule(s) into a group</h3>
      <p class="muted" style="font-size: 11px; margin: 0 0 12px; line-height: 1.5">
        Adds {{ patterns.length }} pattern(s) to the group, then deletes the selected rules.
        Routing/block binding is dropped — re-apply the group to recreate rules with a binding.
      </p>

      <div class="col" style="gap: 10px">
        <label class="field" v-if="customGroups.length">
          <span class="lbl">Target group</span>
          <SearchSelect v-model="target" :options="targetOptions"
                        placeholder="— pick group —" search-placeholder="search groups…" />
        </label>

        <template v-if="isNew">
          <label class="field">
            <span class="lbl">New group name</span>
            <input v-model="newName" placeholder="e.g. Work tools" @keyup.enter="confirm" />
          </label>
          <label class="field">
            <span class="lbl">Accent color</span>
            <div class="row" style="gap: 8px; align-items: center">
              <input type="color" v-model="newColor" style="width: 44px; padding: 2px; height: 30px" />
              <code style="font-size: 11px">{{ newColor }}</code>
            </div>
          </label>
        </template>

        <div class="patterns muted">
          <code v-for="p in patterns" :key="p" class="pat">{{ p }}</code>
        </div>

        <div v-if="error" class="err">{{ error }}</div>

        <div class="row" style="gap: 8px; justify-content: flex-end; margin-top: 4px">
          <button @click="emit('close')">Cancel</button>
          <button class="primary" @click="confirm" :disabled="busy || !valid">
            {{ busy ? 'Moving…' : 'Move into group' }}
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.overlay {
  position: fixed; inset: 0; z-index: 50;
  background: rgba(0, 0, 0, 0.5);
  display: flex; align-items: center; justify-content: center;
}
.dialog {
  background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
  padding: 18px; width: 440px; max-width: calc(100vw - 40px);
  max-height: calc(100vh - 60px); overflow: auto;
  box-shadow: 0 12px 40px rgba(0, 0, 0, 0.4);
}
.field { display: flex; flex-direction: column; gap: 4px; }
.lbl { font-size: 11px; color: var(--text-dim); }
.patterns { display: flex; flex-wrap: wrap; gap: 4px; max-height: 120px; overflow: auto; }
.pat {
  font-size: 11px; background: var(--panel-2); border: 1px solid var(--border);
  border-radius: 4px; padding: 1px 5px;
}
.err {
  background: rgba(255, 111, 111, 0.12); color: var(--danger);
  border-radius: 6px; padding: 6px 8px; font-size: 12px;
}
</style>
