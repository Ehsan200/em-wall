<script lang="ts" setup>
import { ref, onMounted, onUnmounted, computed, watch } from 'vue';
import { startPoll } from '../poll';
import SkeletonRows from './SkeletonRows.vue';
import { fmtTime as fmtZoned } from '../timezone';
import { RecentLogs, ClearLogs } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

type Filter = 'all' | 'block' | 'route' | 'unavailable';

const logs = ref<ipc.LogDTO[]>([]);
const totalsByAction = ref<Record<string, number>>({});
const filter = ref<Filter>('all');
const search = ref<string>('');
const error = ref<string>('');
const lastRefresh = ref<Date | null>(null);
const clearing = ref(false);
let stopPoll: (() => void) | undefined;

// Map UI filter chip → daemon-side filter string. The daemon filters
// at the DB level so a busy run of "block-app-down" entries can't
// hide the latest "route" rows behind a 500-row window.
function daemonFilter(f: Filter): string {
  if (f === 'all') return '';
  if (f === 'unavailable') return 'unavailable';
  return f;
}

// loaded: first reply landed (until then, skeleton rows — not "No
// decisions logged yet"). pending: a filter switch is in flight, so the
// rows on screen belong to the previous chip. seq drops replies overtaken
// by a newer query; the 1s poll otherwise races a chip switch and can
// land the old filter's rows under the new chip.
const loaded = ref(false);
const pending = ref(false);
let seq = 0;

async function refresh() {
  const mine = ++seq;
  const f = filter.value;
  try {
    // Latest entries scoped to the active filter (so Routed actually
    // returns the most recent routes, not the most recent anything). The
    // chip counters need the full breakdown, fetched alongside.
    const [rows, allRows] = await Promise.all([
      RecentLogs(500, daemonFilter(f)),
      f === 'all' ? Promise.resolve(null) : RecentLogs(500, ''),
    ]);
    if (mine !== seq) return;
    logs.value = rows || [];
    const counts: Record<string, number> = {};
    for (const r of (allRows ?? logs.value)) counts[r.action] = (counts[r.action] || 0) + 1;
    totalsByAction.value = counts;
    lastRefresh.value = new Date();
    error.value = '';
  } catch (e: any) {
    if (mine !== seq) return;
    error.value = e?.message || String(e);
  }
  loaded.value = true;
  pending.value = false;
}

// "Unavailable" groups every block-* variant + forward-failed:
// iface-down, app-down, app-busy, app-unsupported, route-failed,
// forward-failed.
function isUnavailable(action: string): boolean {
  return action.startsWith('block-') || action === 'forward-failed';
}

// Client-side search only; action filtering happens at the daemon.
const filtered = computed(() => {
  let xs = logs.value;
  if (search.value.trim()) {
    const q = search.value.trim().toLowerCase();
    xs = xs.filter(l => l.queryName.toLowerCase().includes(q));
  }
  return xs;
});

const counts = computed(() => {
  const c = { all: 0, block: 0, route: 0, unavailable: 0 };
  for (const [action, n] of Object.entries(totalsByAction.value)) {
    c.all += n;
    if (action === 'block') c.block += n;
    else if (action === 'route') c.route += n;
    else if (isUnavailable(action)) c.unavailable += n;
  }
  return c;
});

function fmtTime(s: string): string {
  try { return fmtZoned(s); } catch { return s; }
}

function actionTag(action: string): string {
  // Anything blocked (rule match, iface down, app down, app busy,
  // app unsupported, route-failed) or forward-failed is red.
  // "route" stays blue.
  if (action.startsWith('block') || action === 'forward-failed') return 'tag tag-block';
  if (action === 'route') return 'tag tag-route';
  return 'tag tag-off';
}

async function clearLogs() {
  clearing.value = true;
  try {
    await ClearLogs();
    logs.value = [];
    totalsByAction.value = {};
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    clearing.value = false;
  }
}

// Switching chips should refetch immediately, not wait for the
// 1s timer.
watch(filter, () => { pending.value = true; refresh(); });

onMounted(() => {
  refresh();
  stopPoll = startPoll(refresh, 1000);
});
onUnmounted(() => stopPoll?.());
</script>

<template>
  <div>
    <div v-if="error" class="error">{{ error }}</div>

    <div class="row" style="justify-content: space-between; align-items: center; margin-bottom: 12px">
      <div class="row" style="gap: 0">
        <button :class="{active: filter==='all'}" class="seg" @click="filter='all'">
          All <span class="count">{{ loaded ? counts.all : '…' }}</span>
        </button>
        <button :class="{active: filter==='block'}" class="seg" @click="filter='block'">
          Blocked <span class="count">{{ loaded ? counts.block : '…' }}</span>
        </button>
        <button :class="{active: filter==='route'}" class="seg" @click="filter='route'">
          Routed <span class="count">{{ loaded ? counts.route : '…' }}</span>
        </button>
        <button :class="{active: filter==='unavailable'}" class="seg" @click="filter='unavailable'"
                title="Iface down + App down + App busy + App unsupported">
          Unavailable <span class="count">{{ loaded ? counts.unavailable : '…' }}</span>
        </button>
      </div>
      <div class="row" style="gap: 8px; align-items: center">
        <input v-model="search" placeholder="filter by domain…" style="width: 220px" />
        <button @click="clearLogs" :disabled="clearing || !logs.length" class="danger-subtle">
          {{ clearing ? 'Clearing…' : 'Clear' }}
        </button>
      </div>
    </div>

    <div class="muted" style="font-size: 11px; margin-bottom: 8px">
      live · 1s refresh · {{ lastRefresh ? fmtZoned(lastRefresh) : '—' }} ·
      <template v-if="loaded">showing {{ filtered.length }} of {{ logs.length }}</template>
      <span v-else class="skeleton inline" style="width: 7em"></span>
    </div>

    <div class="loading-bar" :class="{ idle: !pending }"></div>
    <table :class="{ stale: pending }">
      <thead>
        <tr><th>Time</th><th>Domain</th><th>Action</th><th>Interface</th><th>Client</th></tr>
      </thead>
      <tbody>
        <tr v-for="l in filtered" :key="l.id">
          <td class="muted" style="white-space: nowrap">{{ fmtTime(l.timestamp) }}</td>
          <td><code>{{ l.queryName }}</code></td>
          <td><span :class="actionTag(l.action)">{{ l.action }}</span></td>
          <td class="muted">{{ l.interface || '—' }}</td>
          <td class="muted">{{ l.clientIp || '—' }}</td>
        </tr>
        <SkeletonRows v-if="!loaded" :cols="5" :rows="8" />
        <tr v-else-if="!filtered.length">
          <td colspan="5" class="muted" style="text-align:center; padding:24px">
            <span v-if="!logs.length">No decisions logged yet. Plain allows are not logged.</span>
            <span v-else>No matches for current filter.</span>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
.seg {
  background: transparent;
  border: 1px solid var(--border);
  border-right: none;
  border-radius: 0;
  padding: 6px 14px;
  color: var(--text-dim);
  font-size: 12px;
}
.seg:first-child { border-top-left-radius: 6px; border-bottom-left-radius: 6px; }
.seg:last-child  { border-right: 1px solid var(--border); border-top-right-radius: 6px; border-bottom-right-radius: 6px; }
.seg.active {
  color: var(--text);
  background: var(--panel-2);
  border-color: var(--accent);
}
.count {
  display: inline-block;
  margin-left: 6px;
  padding: 1px 6px;
  background: var(--border);
  border-radius: 8px;
  font-size: 10px;
  color: var(--text-dim);
}
.seg.active .count { background: var(--accent); color: #fff; }
.danger-subtle {
  color: var(--danger);
  background: transparent;
  border-color: var(--danger);
  opacity: 0.75;
}
.danger-subtle:hover:not(:disabled) { opacity: 1; }
</style>
