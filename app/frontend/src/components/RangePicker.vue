<script lang="ts" setup>
import { ref, computed, watch, onMounted, onUnmounted } from 'vue';
import {
  type RangeSel, RELATIVE_PRESETS, RETENTION_SECS, ROLLUP_AGE_SECS,
  calendarPresets, fmtPoint, rangeLabel, resolveRange, shiftRange, toLocalInput, fromLocalInput,
} from '../usageRange';

const model = defineModel<RangeSel>({ required: true });

const open = ref(false);
const root = ref<HTMLElement | null>(null);
const calendar = ref(calendarPresets());
const customFrom = ref('');
const customTo = ref('');
const customError = ref('');

const label = computed(() => rangeLabel(model.value));
const live = computed(() => model.value.kind !== 'between');
const canBack = computed(() =>
  resolveRange(model.value).from > Math.floor(Date.now() / 1000) - RETENTION_SECS);

function isActive(sel: RangeSel): boolean {
  const m = model.value;
  if (m.kind !== sel.kind) return false;
  if (m.kind === 'last' && sel.kind === 'last') return m.secs === sel.secs;
  if (m.kind === 'since' && sel.kind === 'since') return m.from === sel.from;
  if (m.kind === 'between' && sel.kind === 'between') return m.from === sel.from && m.to === sel.to;
  return false;
}

function pick(sel: RangeSel) {
  model.value = sel;
  open.value = false;
}

function toggle() {
  open.value = !open.value;
  if (!open.value) return;
  // Rebuild on open: "Today" must mean today even if the app ran overnight.
  calendar.value = calendarPresets();
  const r = resolveRange(model.value);
  customFrom.value = toLocalInput(r.from);
  customTo.value = toLocalInput(r.to);
  customError.value = '';
}

function applyCustom() {
  const from = fromLocalInput(customFrom.value);
  const to = fromLocalInput(customTo.value);
  if (from === null || to === null) { customError.value = 'Pick both a start and an end.'; return; }
  if (to <= from) { customError.value = 'End must be after start.'; return; }
  const now = Math.floor(Date.now() / 1000);
  // An end in the future (or within a minute of now) means "until now", live.
  pick(to >= now - 60 ? { kind: 'since', from, label: `Since ${fmtPoint(from)}` } : { kind: 'between', from, to });
}

function step(dir: -1 | 1) {
  model.value = shiftRange(model.value, dir);
}

function onDocClick(e: MouseEvent) {
  if (open.value && root.value && !root.value.contains(e.target as Node)) open.value = false;
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') open.value = false;
}
onMounted(() => {
  document.addEventListener('mousedown', onDocClick);
  document.addEventListener('keydown', onKey);
});
onUnmounted(() => {
  document.removeEventListener('mousedown', onDocClick);
  document.removeEventListener('keydown', onKey);
});
watch(model, () => { customError.value = ''; });

const minInput = computed(() => toLocalInput(Math.floor(Date.now() / 1000) - RETENTION_SECS));
const rollupDays = ROLLUP_AGE_SECS / 86400;
const retentionDays = RETENTION_SECS / 86400;
</script>

<template>
  <div ref="root" class="rp">
    <div class="bar">
      <button class="step" :disabled="!canBack" title="Previous window" @click="step(-1)">‹</button>
      <button class="trigger" :class="{ open }" @click="toggle">
        <span class="dot" :class="{ live }" :title="live ? 'Live — follows the clock' : 'Fixed window'"></span>
        <span class="lbl">{{ label }}</span>
        <span class="caret">▾</span>
      </button>
      <button class="step" :disabled="live" title="Next window" @click="step(1)">›</button>
    </div>

    <div v-if="open" class="pop">
      <div class="sec-title">Relative</div>
      <div class="chips">
        <button v-for="p in RELATIVE_PRESETS" :key="p.secs" class="chip"
                :class="{ active: isActive({ kind: 'last', secs: p.secs }) }"
                @click="pick({ kind: 'last', secs: p.secs })">{{ p.short }}</button>
      </div>

      <div class="sec-title">Calendar</div>
      <div class="cal">
        <button v-for="c in calendar" :key="c.label" class="chip wide"
                :class="{ active: isActive(c.sel) }" @click="pick(c.sel)">{{ c.label }}</button>
      </div>

      <div class="sec-title">Custom</div>
      <div class="custom">
        <label>From <input v-model="customFrom" type="datetime-local" :min="minInput" /></label>
        <label>To <input v-model="customTo" type="datetime-local" :min="minInput" /></label>
        <button class="apply" @click="applyCustom">Apply</button>
      </div>
      <div v-if="customError" class="err">{{ customError }}</div>

      <div class="foot">
        History is kept {{ retentionDays }} days. Data older than {{ rollupDays }} days is stored per hour.
      </div>
    </div>
  </div>
</template>

<style scoped>
.rp { position: relative; }
.bar { display: flex; align-items: stretch; }
.trigger, .step {
  background: transparent;
  border: 1px solid var(--border);
  color: var(--text);
  font-size: 12px;
  padding: 6px 10px;
}
.step { color: var(--text-dim); padding: 6px 9px; font-size: 14px; line-height: 1; }
.step:first-child { border-radius: 6px 0 0 6px; border-right: none; }
.step:last-child { border-radius: 0 6px 6px 0; border-left: none; }
.step:disabled { opacity: 0.35; cursor: default; }
.step:not(:disabled):hover, .trigger:hover { background: var(--panel-2); }
.trigger { display: inline-flex; align-items: center; gap: 8px; min-width: 170px; }
.trigger.open { border-color: var(--accent); }
.lbl { flex: 1; text-align: left; white-space: nowrap; }
.caret { color: var(--text-dim); font-size: 10px; }
.dot { width: 7px; height: 7px; border-radius: 50%; background: var(--text-dim); flex: none; }
.dot.live { background: var(--success); box-shadow: 0 0 0 3px rgba(95, 208, 122, 0.15); }

.pop {
  position: absolute;
  top: calc(100% + 6px);
  left: 0;
  z-index: 200;
  width: 340px;
  max-width: calc(100vw - 32px);
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
  box-shadow: 0 10px 30px rgba(0, 0, 0, 0.45);
  padding: 12px;
}
.sec-title {
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-dim);
  margin: 10px 0 6px;
}
.sec-title:first-child { margin-top: 0; }
.chips { display: grid; grid-template-columns: repeat(5, 1fr); gap: 4px; }
.cal { display: grid; grid-template-columns: repeat(2, 1fr); gap: 4px; }
.chip {
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 5px;
  color: var(--text-dim);
  font-size: 12px;
  padding: 5px 0;
}
.chip.wide { text-align: left; padding: 5px 10px; }
.chip:hover { color: var(--text); }
.chip.active { color: var(--text); border-color: var(--accent); background: rgba(110, 168, 255, 0.12); }
.custom { display: flex; flex-direction: column; gap: 6px; }
.custom label {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  font-size: 12px;
  color: var(--text-dim);
}
.custom input {
  flex: 1;
  max-width: 230px;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 5px;
  color: var(--text);
  font-size: 12px;
  padding: 4px 6px;
  color-scheme: dark;
}
.apply {
  align-self: flex-end;
  background: var(--accent);
  border: none;
  border-radius: 5px;
  color: #0b0b10;
  font-size: 12px;
  font-weight: 600;
  padding: 5px 14px;
}
.err { color: var(--danger); font-size: 11px; margin-top: 6px; }
.foot {
  margin-top: 10px;
  padding-top: 8px;
  border-top: 1px solid var(--border);
  font-size: 11px;
  color: var(--text-dim);
}
</style>
