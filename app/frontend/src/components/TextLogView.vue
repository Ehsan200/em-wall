<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted, watch, nextTick } from 'vue';
import { startPoll } from '../poll';
import { fmtTime as fmtZoned } from '../timezone';
import { TailLog, RevealLog } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

const props = defineProps<{ source: string }>();

const LINE_CHOICES = [200, 1000, 5000];

const data = ref<ipc.LogsTailDTO | null>(null);
const lines = ref(1000);
const grep = ref('');
const live = ref(true);
const error = ref('');
const copied = ref(false);
const lastRefresh = ref<Date | null>(null);
const pending = ref(false);
const box = ref<HTMLElement | null>(null);
// follow: the user is at the bottom, so new lines keep it there. Scrolling
// up to read stops following until they scroll back down.
const follow = ref(true);
let stopPoll: (() => void) | undefined;
let seq = 0;

async function refresh() {
  const mine = ++seq;
  try {
    const out = await TailLog(props.source, lines.value, grep.value.trim());
    if (mine !== seq) return;
    data.value = out;
    error.value = '';
    lastRefresh.value = new Date();
  } catch (e: any) {
    if (mine !== seq) return;
    error.value = e?.message || String(e);
  }
  pending.value = false;
  if (follow.value) {
    await nextTick();
    if (box.value) box.value.scrollTop = box.value.scrollHeight;
  }
}

function onScroll() {
  const el = box.value;
  if (!el) return;
  follow.value = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
}

const text = computed(() => (data.value?.lines ?? []).join('\n'));

function fmtSize(n: number): string {
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(0) + ' KB';
  return n + ' B';
}

// Highlight severity so errors stand out in a wall of text.
function lineClass(l: string): string {
  const s = l.toLowerCase();
  if (s.includes('[error]') || s.includes('panic') || s.includes(' error') || s.includes('failed')) return 'ln err';
  if (s.includes('[warning]') || s.includes(' warn')) return 'ln warn';
  return 'ln';
}

async function copyAll() {
  try {
    await navigator.clipboard.writeText(text.value);
    copied.value = true;
    setTimeout(() => (copied.value = false), 1200);
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function reveal() {
  if (!data.value?.path) return;
  try { await RevealLog(data.value.path); } catch (e: any) { error.value = e?.message || String(e); }
}

function startLive() {
  stopPoll?.();
  stopPoll = live.value ? startPoll(refresh, 2000) : undefined;
}

// Debounce typing; line-count and source changes refetch at once.
let grepTimer: number | undefined;
watch(grep, () => {
  window.clearTimeout(grepTimer);
  grepTimer = window.setTimeout(() => { pending.value = true; follow.value = true; refresh(); }, 250);
});
watch(lines, () => { pending.value = true; follow.value = true; refresh(); });
watch(live, startLive);

onMounted(() => { pending.value = true; refresh(); startLive(); });
onUnmounted(() => { stopPoll?.(); window.clearTimeout(grepTimer); });
</script>

<template>
  <div>
    <div v-if="error" class="error">{{ error }}</div>

    <div class="row" style="justify-content: space-between; align-items: center; margin-bottom: 8px; gap: 8px">
      <div class="row" style="gap: 8px; align-items: center">
        <input v-model="grep" placeholder="search…" style="width: 220px" />
        <select v-model.number="lines">
          <option v-for="n in LINE_CHOICES" :key="n" :value="n">last {{ n }} lines</option>
        </select>
        <label class="row" style="gap: 4px; align-items: center; font-size: 12px">
          <input type="checkbox" v-model="live" /> live
        </label>
      </div>
      <div class="row" style="gap: 8px">
        <button @click="refresh" :disabled="pending">Refresh</button>
        <button @click="copyAll" :disabled="!data?.lines?.length">{{ copied ? 'Copied' : 'Copy' }}</button>
        <button @click="reveal" :disabled="!data?.path || data?.missing">Reveal in Finder</button>
      </div>
    </div>

    <div class="muted" style="font-size: 11px; margin-bottom: 8px">
      <template v-if="data">
        <code>{{ data.path || '—' }}</code>
        <template v-if="!data.missing"> · {{ fmtSize(data.size) }} · {{ data.lines.length }} lines<template v-if="grep.trim()"> matching</template><template v-if="data.truncated"> (from end of file)</template></template>
      </template>
      <template v-if="live"> · live · 2s refresh</template>
      · {{ lastRefresh ? fmtZoned(lastRefresh) : '—' }}
      <template v-if="!follow"> · <a href="#" @click.prevent="follow = true; refresh()">jump to newest</a></template>
    </div>

    <div class="loading-bar" :class="{ idle: !pending }"></div>
    <div ref="box" class="logbox" :class="{ stale: pending }" @scroll="onScroll">
      <div v-if="!data" class="muted placeholder">Loading…</div>
      <div v-else-if="data.missing" class="muted placeholder">
        No log file here{{ data.path ? '' : ' — this daemon was started without one' }}. A dev daemon (<code>make run-daemon</code>) logs to its terminal.
      </div>
      <div v-else-if="!data.lines.length" class="muted placeholder">
        {{ grep.trim() ? 'No matching lines.' : 'Log is empty.' }}
      </div>
      <template v-else>
        <div v-for="(l, i) in data.lines" :key="i" :class="lineClass(l)">{{ l }}</div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.logbox {
  height: calc(100vh - 230px);
  min-height: 240px;
  overflow: auto;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 8px 10px;
  font-family: ui-monospace, "SF Mono", Menlo, monospace;
  font-size: 11px;
  line-height: 1.5;
}
.ln { white-space: pre-wrap; word-break: break-all; }
.ln.err { color: var(--danger); }
.ln.warn { color: var(--warn); }
.placeholder { text-align: center; padding: 24px; font-family: -apple-system, system-ui, sans-serif; font-size: 12px; }
</style>
