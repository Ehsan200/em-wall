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
// Patterns edited as one-per-line text; split + trimmed on save.
const patternsText = ref((props.group?.patterns ?? []).join('\n'));
const busy = ref(false);
const error = ref('');

const patterns = computed<string[]>(() =>
  patternsText.value
    .split('\n')
    .map(p => p.trim())
    .filter(p => p.length > 0)
);

const valid = computed(() => displayName.value.trim().length > 0 && patterns.value.length > 0);

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

        <label class="field">
          <span class="lbl">Patterns <span class="muted">— one per line ({{ patterns.length }})</span></span>
          <textarea v-model="patternsText" rows="7"
                    placeholder="*.example.com&#10;api.example.com&#10;10.0.0.0/8"
                    style="font-family: ui-monospace, monospace; font-size: 12px"></textarea>
          <span class="muted" style="font-size: 10px">Domains (wildcards ok) or IP/CIDR — same syntax as rules.</span>
        </label>

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
