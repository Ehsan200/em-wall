<script lang="ts" setup>
import { ref, onMounted, onUnmounted, computed, watch } from 'vue';
import { RecentLogs } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

type Filter = 'all' | 'block' | 'route' | 'unavailable';

const logs = ref<ipc.LogDTO[]>([]);
const totalsByAction = ref<Record<string, number>>({});
const filter = ref<Filter>('all');
const search = ref<string>('');
const error = ref<string>('');
const lastRefresh = ref<Date | null>(null);
let timer: number | undefined;

// Map UI filter chip → daemon-side filter string. The daemon filters
// at the DB level so a busy run of "block-app-down" entries can't
// hide the latest "route" rows behind a 500-row window.
function daemonFilter(f: Filter): string {
  if (f === 'all') return '';
  if (f === 'unavailable') return 'unavailable';
  return f;
}

async function refresh() {
  try {
    // Latest entries scoped to the active filter (so Routed actually
    // returns the most recent routes, not the most recent anything).
    logs.value = (await RecentLogs(500, daemonFilter(filter.value))) || [];
    // Header counters stay accurate by also fetching counts per group
    // via separate small queries. We piggy-back on the same RecentLogs
    // call with bigger limits per filter — cheap enough.
    if (filter.value !== 'all') {
      // For non-"all", we already have the filtered list, but the chip
      // counters need the full breakdown. Fall through to a full fetch
      // for counter accuracy.
    }
    const allRows = filter.value === 'all'
      ? logs.value
      : ((await RecentLogs(2000, '')) || []);
    const counts: Record<string, number> = {};
    for (const r of allRows) counts[r.action] = (counts[r.action] || 0) + 1;
    totalsByAction.value = counts;
    lastRefresh.value = new Date();
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
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
  try { return new Date(s).toLocaleTimeString(); } catch { return s; }
}

function actionTag(action: string): string {
  // Anything blocked (rule match, iface down, app down, app busy,
  // app unsupported, route-failed) or forward-failed is red.
  // "route" stays blue.
  if (action.startsWith('block') || action === 'forward-failed') return 'tag tag-block';
  if (action === 'route') return 'tag tag-route';
  return 'tag tag-off';
}

// Switching chips should refetch immediately, not wait for the
// 1s timer.
watch(filter, () => { refresh(); });

onMounted(() => {
  refresh();
  timer = window.setInterval(refresh, 1000);
});
onUnmounted(() => { if (timer) window.clearInterval(timer); });
</script>

<template>
  <div class="panel">
    <div v-if="error" class="error">{{ error }}</div>

    <div class="row" style="justify-content: space-between; align-items: center; margin-bottom: 12px">
      <div class="row" style="gap: 0">
        <button :class="{active: filter==='all'}" class="seg" @click="filter='all'">
          All <span class="count">{{ counts.all }}</span>
        </button>
        <button :class="{active: filter==='block'}" class="seg" @click="filter='block'">
          Blocked <span class="count">{{ counts.block }}</span>
        </button>
        <button :class="{active: filter==='route'}" class="seg" @click="filter='route'">
          Routed <span class="count">{{ counts.route }}</span>
        </button>
        <button :class="{active: filter==='unavailable'}" class="seg" @click="filter='unavailable'"
                title="Iface down + App down + App busy + App unsupported">
          Unavailable <span class="count">{{ counts.unavailable }}</span>
        </button>
      </div>
      <input v-model="search" placeholder="filter by domain…" style="width: 220px" />
    </div>

    <div class="muted" style="font-size: 11px; margin-bottom: 8px">
      live · 1s refresh · {{ lastRefresh ? lastRefresh.toLocaleTimeString() : '—' }} ·
      showing {{ filtered.length }} of {{ logs.length }}
    </div>

    <table>
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
        <tr v-if="!filtered.length">
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
</style>
