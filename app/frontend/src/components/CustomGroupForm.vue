<script lang="ts" setup>
import { ref, computed } from 'vue';
import { AddCustomGroup, UpdateCustomGroup } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

// group=null → create mode; group set → edit mode (key is immutable).
const props = defineProps<{ group: ipc.GroupDTO | null }>();
const emit = defineEmits<{ (e: 'saved'): void; (e: 'close'): void }>();

const isEdit = computed(() => !!props.group);
const displayName = ref(props.group?.displayName ?? '');
const description = ref(props.group?.description ?? '');
const color = ref(props.group?.color ?? '#6c5ce7');
const busy = ref(false);
const error = ref('');

// Patterns are NOT edited here — a new group starts empty and gains patterns
// via "Move to group" (selecting existing rules). Edit mode carries the
// group's current patterns through unchanged so saving name/color never
// wipes them.
const patterns = computed<string[]>(() => props.group?.patterns ?? []);

const valid = computed(() => displayName.value.trim().length > 0);

async function save() {
  if (!valid.value || busy.value) return;
  busy.value = true;
  error.value = '';
  try {
    if (isEdit.value && props.group) {
      await UpdateCustomGroup(props.group.key, displayName.value.trim(), description.value.trim(), patterns.value, color.value);
    } else {
      await AddCustomGroup('', displayName.value.trim(), description.value.trim(), patterns.value, color.value);
    }
    emit('saved');
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
      <h3 style="margin: 0 0 12px">{{ isEdit ? 'Edit custom group' : 'New custom group' }}</h3>

      <div class="col" style="gap: 10px">
        <label class="field">
          <span class="lbl">Name</span>
          <input v-model="displayName" placeholder="e.g. Work tools" @keyup.enter="save" />
        </label>

        <label class="field">
          <span class="lbl">Description <span class="muted">(optional)</span></span>
          <input v-model="description" placeholder="What this group covers" />
        </label>

        <label class="field">
          <span class="lbl">Accent color</span>
          <div class="row" style="gap: 8px; align-items: center">
            <input type="color" v-model="color" style="width: 44px; padding: 2px; height: 30px" />
            <code style="font-size: 11px">{{ color }}</code>
          </div>
        </label>

        <p class="muted" style="font-size: 11px; margin: 2px 0 0; line-height: 1.5">
          <template v-if="isEdit">This group has {{ patterns.length }} pattern(s). Add more from the
          Rules list: select rules and use “Move to group”.</template>
          <template v-else>The group starts empty. Add patterns by selecting rules in the Rules
          list and choosing “Move to group”.</template>
        </p>

        <div v-if="error" class="err">{{ error }}</div>

        <div class="row" style="gap: 8px; justify-content: flex-end; margin-top: 4px">
          <button @click="emit('close')">Cancel</button>
          <button class="primary" @click="save" :disabled="!valid || busy">
            {{ busy ? 'Saving…' : (isEdit ? 'Save changes' : 'Create group') }}
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
.err {
  background: rgba(255, 111, 111, 0.12); color: var(--danger);
  border-radius: 6px; padding: 6px 8px; font-size: 12px;
}
</style>
