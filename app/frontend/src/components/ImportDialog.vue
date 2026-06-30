<script lang="ts" setup>
import { ref, computed } from 'vue';
import { ReadImportFile, Import } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

const emit = defineEmits<{ (e: 'close'): void; (e: 'done'): void }>();

// Step 1: pick file → blob (base64). Step 2: passphrase → apply.
const blob = ref('');
const pass = ref('');
const busy = ref(false);
const error = ref('');
const result = ref<ipc.ImportResult | null>(null);

const passValid = computed(() => pass.value.length > 0);

async function pickFile() {
  error.value = '';
  busy.value = true;
  try {
    const b = await ReadImportFile();
    if (!b) return; // cancelled dialog
    blob.value = b;
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function doImport() {
  if (busy.value || !blob.value || !passValid.value) return;
  busy.value = true;
  error.value = '';
  try {
    result.value = await Import(blob.value, pass.value);
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
      <h3 style="margin: 0 0 4px">Import rules</h3>
      <p class="muted" style="font-size: 11px; margin: 0 0 12px">
        Existing rules (matched by pattern) and groups (matched by key) are kept — only new ones are added.
      </p>

      <!-- Result view -->
      <template v-if="result">
        <div class="ok">
          Imported {{ result.rulesCreated }} rule(s), {{ result.groupsCreated }} group(s).
          <div v-if="result.rulesSkipped || result.groupsSkipped" class="muted" style="font-size: 11px; margin-top: 4px">
            Skipped {{ result.rulesSkipped }} existing rule(s), {{ result.groupsSkipped }} existing group(s).
          </div>
        </div>
        <div v-if="result.warnings && result.warnings.length" class="warn">
          <div style="font-weight: 600; margin-bottom: 4px">Warnings</div>
          <div v-for="(w, i) in result.warnings" :key="i" style="font-size: 11px">• {{ w }}</div>
        </div>
        <div class="row" style="justify-content: flex-end; margin-top: 12px">
          <button class="primary" @click="emit('close')">Done</button>
        </div>
      </template>

      <!-- Input view -->
      <template v-else>
        <div class="col" style="gap: 10px">
          <div class="row" style="gap: 8px; align-items: center">
            <button @click="pickFile" :disabled="busy">Choose file…</button>
            <span class="muted" style="font-size: 11px">
              {{ blob ? 'File loaded ✓' : 'No file selected (.embackup)' }}
            </span>
          </div>

          <label class="field">
            <span class="lbl">Passphrase</span>
            <input type="password" v-model="pass" placeholder="Passphrase used at export" @keyup.enter="doImport" />
          </label>

          <div v-if="error" class="err">{{ error }}</div>

          <div class="row" style="gap: 8px; justify-content: flex-end; margin-top: 4px">
            <button @click="emit('close')">Cancel</button>
            <button class="primary" @click="doImport" :disabled="busy || !blob || !passValid">
              {{ busy ? 'Importing…' : 'Import' }}
            </button>
          </div>
        </div>
      </template>
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
  padding: 18px; width: 420px; max-width: calc(100vw - 40px);
  max-height: calc(100vh - 60px); overflow: auto;
  box-shadow: 0 12px 40px rgba(0, 0, 0, 0.4);
}
.field { display: flex; flex-direction: column; gap: 4px; }
.lbl { font-size: 11px; color: var(--text-dim); }
.err {
  background: rgba(255, 111, 111, 0.12); color: var(--danger);
  border-radius: 6px; padding: 6px 8px; font-size: 12px;
}
.ok {
  background: rgba(95, 208, 122, 0.12); color: var(--success);
  border-radius: 8px; padding: 10px; font-size: 12px;
}
.warn {
  background: rgba(255, 200, 87, 0.10); color: var(--warn);
  border-radius: 8px; padding: 10px; font-size: 12px; margin-top: 8px;
}
</style>
