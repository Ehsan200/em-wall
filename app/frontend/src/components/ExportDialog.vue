<script lang="ts" setup>
import { ref, computed } from 'vue';
import { Export, SaveExportFile } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

// Two modes:
//  - fixed set  → the selection is already decided (e.g. "everything" or a
//    set of rule IDs); the dialog only asks for a passphrase.
//  - fixed null → show a scope picker built from `groups`.
const props = defineProps<{
  fixed: ipc.ExportSelection | null;
  groups?: ipc.GroupDTO[];
  label?: string;
}>();
const emit = defineEmits<{ (e: 'close'): void; (e: 'done'): void }>();

const pass = ref('');
const pass2 = ref('');
const busy = ref(false);
const error = ref('');
const savedPath = ref('');

// Picker state (only used when fixed === null).
const scope = ref<'all' | 'groups'>('all');
const pickedKeys = ref<Set<string>>(new Set());
function togglePick(key: string) {
  const next = new Set(pickedKeys.value);
  if (next.has(key)) next.delete(key); else next.add(key);
  pickedKeys.value = next;
}

const passphraseValid = computed(() => pass.value.length >= 4 && pass.value === pass2.value);

const selectionValid = computed(() => {
  if (props.fixed) return true;
  if (scope.value === 'all') return true;
  return pickedKeys.value.size > 0;
});

function buildSelection(): ipc.ExportSelection {
  if (props.fixed) return props.fixed;
  if (scope.value === 'all') {
    return { all: true, ruleIds: [], curatedKeys: [], customKeys: [] };
  }
  const curated: string[] = [];
  const custom: string[] = [];
  for (const g of props.groups ?? []) {
    if (!pickedKeys.value.has(g.key)) continue;
    if (g.custom) custom.push(g.key); else curated.push(g.key);
  }
  return { all: false, ruleIds: [], curatedKeys: curated, customKeys: custom };
}

async function doExport() {
  if (busy.value || !passphraseValid.value || !selectionValid.value) return;
  busy.value = true;
  error.value = '';
  try {
    const res = await Export(buildSelection(), pass.value);
    if (res.ruleCount === 0 && res.groupCount === 0) {
      error.value = 'Nothing matched the selection — nothing to export.';
      return;
    }
    const path = await SaveExportFile(res.blob, res.filename);
    if (!path) { // user cancelled the save dialog
      emit('close');
      return;
    }
    savedPath.value = `Exported ${res.ruleCount} rule(s) and ${res.groupCount} group(s) to ${path}`;
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
      <h3 style="margin: 0 0 4px">Export {{ label || 'rules' }}</h3>
      <p class="muted" style="font-size: 11px; margin: 0 0 12px">
        The exported file is always encrypted with the passphrase you choose.
        You'll need the same passphrase to import it.
      </p>

      <div v-if="savedPath" class="ok">{{ savedPath }}</div>

      <template v-else>
        <!-- scope picker (only when selection isn't fixed) -->
        <div v-if="!fixed" class="col" style="gap: 8px; margin-bottom: 12px">
          <label class="row" style="gap: 6px; align-items: center">
            <input type="radio" value="all" v-model="scope" /> Everything (all rules + custom groups)
          </label>
          <label class="row" style="gap: 6px; align-items: center">
            <input type="radio" value="groups" v-model="scope" /> Choose groups
          </label>
          <div v-if="scope === 'groups'" class="picker">
            <label v-for="g in groups" :key="g.key" class="row pick-row" :title="g.patterns.join('\n')">
              <input type="checkbox" :checked="pickedKeys.has(g.key)" @change="togglePick(g.key)" />
              <span class="dot" :style="{ background: g.color || 'var(--text-dim)' }"></span>
              <span style="flex: 1">{{ g.displayName }}</span>
              <span v-if="g.custom" class="badge">custom</span>
              <span class="muted" style="font-size: 10px">{{ g.patterns.length }}</span>
            </label>
          </div>
        </div>

        <div class="col" style="gap: 10px">
          <label class="field">
            <span class="lbl">Passphrase <span class="muted">(min 4 chars)</span></span>
            <input type="password" v-model="pass" placeholder="Choose a passphrase" />
          </label>
          <label class="field">
            <span class="lbl">Confirm passphrase</span>
            <input type="password" v-model="pass2" placeholder="Repeat it" @keyup.enter="doExport" />
            <span v-if="pass2 && pass !== pass2" class="muted" style="font-size: 10px; color: var(--danger)">
              Passphrases don't match.
            </span>
          </label>

          <div v-if="error" class="err">{{ error }}</div>

          <div class="row" style="gap: 8px; justify-content: flex-end; margin-top: 4px">
            <button @click="emit('close')">Cancel</button>
            <button class="primary" @click="doExport" :disabled="busy || !passphraseValid || !selectionValid">
              {{ busy ? 'Encrypting…' : 'Export…' }}
            </button>
          </div>
        </div>
      </template>

      <div v-if="savedPath" class="row" style="justify-content: flex-end; margin-top: 12px">
        <button class="primary" @click="emit('close')">Done</button>
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
.picker {
  border: 1px solid var(--border); border-radius: 8px; padding: 6px;
  max-height: 200px; overflow: auto; display: flex; flex-direction: column; gap: 2px;
}
.pick-row { gap: 8px; align-items: center; padding: 4px 6px; border-radius: 6px; cursor: pointer; }
.pick-row:hover { background: var(--panel-2); }
.dot { width: 10px; height: 10px; border-radius: 3px; flex-shrink: 0; }
.badge {
  font-size: 9px; text-transform: uppercase; letter-spacing: 0.04em;
  background: var(--panel-2); border: 1px solid var(--border);
  border-radius: 4px; padding: 1px 4px; color: var(--text-dim);
}
.err {
  background: rgba(255, 111, 111, 0.12); color: var(--danger);
  border-radius: 6px; padding: 6px 8px; font-size: 12px;
}
.ok {
  background: rgba(95, 208, 122, 0.12); color: var(--success);
  border-radius: 6px; padding: 8px 10px; font-size: 12px; word-break: break-all;
}
</style>
