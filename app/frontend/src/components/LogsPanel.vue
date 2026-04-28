<script lang="ts" setup>
import { ref, onMounted, onUnmounted, computed } from 'vue';
import { RecentLogs } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

type Filter = 'all' | 'block' | 'route' | 'unavailable';

const logs = ref<ipc.LogDTO[]>([]);
const filter = ref<Filter>('all');
const search = ref<string>('');
const error = ref<string>('');
const lastRefresh = ref<Date | null>(null);
let timer: number | undefined;

async function refresh() {
  try {
    logs.value = (await RecentLogs(500)) || [];
    lastRefresh.value = new Date();
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

// "Unavailable" groups every block-* variant except plain `block`:
// iface-down, app-down, app-busy, app-unsupported.
function isUnavailable(action: string): boolean {
  return action.startsWith('block-');
}

const filtered = computed(() => {
  let xs = logs.value;
  if (filter.value === 'block') {
    xs = xs.filter(l => l.action === 'block');
  } else if (filter.value === 'route') {
    xs = xs.filter(l => l.action === 'route');
  } else if (filter.value === 'unavailable') {
    xs = xs.filter(l => isUnavailable(l.action));
  }
  if (search.value.trim()) {
    const q = search.value.trim().toLowerCase();
    xs = xs.filter(l => l.queryName.toLowerCase().includes(q));
  }
  return xs;
});

const counts = computed(() => {
  const c = { block: 0, route: 0, unavailable: 0 };
  for (const l of logs.value) {
    if (l.action === 'block') c.block++;
    else if (l.action === 'route') c.route++;
    else if (isUnavailable(l.action)) c.unavailable++;
  }
  return c;
});

function fmtTime(s: string): string {
  try { return new Date(s).toLocaleTimeString(); } catch { return s; }
}

function actionTag(action: string): string {
  // Anything blocked (rule match, iface down, app down, app busy
  // during transition, app unsupported) is red. "route" stays blue.
  if (action.startsWith('block')) return 'tag tag-block';
  if (action === 'route') return 'tag tag-route';
  return 'tag tag-off';
}

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
          All <span class="count">{{ logs.length }}</span>
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
