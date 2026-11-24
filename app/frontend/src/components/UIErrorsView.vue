<script lang="ts" setup>
import { ref } from 'vue';
import { uiErrors, clearUIErrors, type UIError } from '../uiErrors';
import { fmtTime } from '../timezone';

// Errors thrown in this window (render, unhandled throw/rejection), newest
// first. Kept in memory only — they also go to the app's Wails log.
const open = ref<Set<number>>(new Set());
const copied = ref(false);

function toggle(id: number) {
  const s = new Set(open.value);
  s.has(id) ? s.delete(id) : s.add(id);
  open.value = s;
}

function asText(e: UIError): string {
  return [
    `${e.at.toISOString()} [${e.source}]${e.count > 1 ? ` ×${e.count}` : ''} ${e.component ? e.component + ': ' : ''}${e.message}`,
    e.info ? `info: ${e.info}` : '',
    e.stack,
  ].filter(Boolean).join('\n');
}

async function copyAll() {
  await navigator.clipboard.writeText(uiErrors.value.map(asText).join('\n\n'));
  copied.value = true;
  window.setTimeout(() => { copied.value = false; }, 1500);
}
</script>

<template>
  <div class="col" style="gap: 8px">
    <div class="row" style="gap: 8px; align-items: center">
      <span class="muted" style="font-size: 12px; flex: 1">
        Errors raised by this window since it opened. A panel that shows up blank usually has one here.
      </span>
      <button :disabled="!uiErrors.length" @click="copyAll">{{ copied ? 'Copied' : 'Copy all' }}</button>
      <button :disabled="!uiErrors.length" @click="clearUIErrors">Clear</button>
    </div>
    <div v-if="!uiErrors.length" class="muted" style="font-size: 12px; padding: 12px 0">No UI errors.</div>
    <div v-for="e in uiErrors" :key="e.id" class="err-row" @click="toggle(e.id)">
      <div class="row" style="gap: 8px; align-items: baseline">
        <span class="muted mono">{{ fmtTime(e.at) }}</span>
        <span class="tag tag-block" style="font-size: 10px">{{ e.source }}</span>
        <span v-if="e.component" class="mono" style="color: var(--text-dim)">{{ e.component }}</span>
        <span class="msg">{{ e.message }}</span>
        <span v-if="e.count > 1" class="muted">×{{ e.count }}</span>
      </div>
      <pre v-if="open.has(e.id)" class="stack">{{ [e.info && 'info: ' + e.info, e.stack || '(no stack)'].filter(Boolean).join('\n') }}</pre>
    </div>
  </div>
</template>

<style scoped>
.err-row {
  padding: 6px 8px;
  background: var(--panel-2);
  border-radius: 6px;
  font-size: 12px;
  cursor: pointer;
}
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 11px; }
.msg { color: var(--danger); word-break: break-word; flex: 1; }
.stack {
  margin: 6px 0 0;
  font-size: 11px;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text-dim);
  user-select: text;
}
</style>
