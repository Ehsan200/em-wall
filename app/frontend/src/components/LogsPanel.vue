<script lang="ts" setup>
import { ref, onMounted, onUnmounted, computed } from 'vue';
import { RecentLogs } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

const logs = ref<ipc.LogDTO[]>([]);
const filter = ref<'all' | 'block' | 'route' | 'block-iface-down'>('all');
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

const filtered = computed(() => {
  let xs = logs.value;
  if (filter.value !== 'all') {
    xs = xs.filter(l => l.action === filter.value);
  }
  if (search.value.trim()) {
    const q = search.value.trim().toLowerCase();
    xs = xs.filter(l => l.queryName.toLowerCase().includes(q));
  }
  return xs;
});

const counts = computed(() => {
  const c = { block: 0, route: 0, ifdown: 0 };
  for (const l of logs.value) {
    if (l.action === 'block') c.block++;
    else if (l.action === 'route') c.route++;
    else if (l.action === 'block-iface-down') c.ifdown++;
  }
  return c;
});

function fmtTime(s: string): string {
  try { return new Date(s).toLocaleTimeString(); } catch { return s; }
}

function actionTag(action: string): string {
  if (action === 'block' || action === 'block-iface-down') return 'tag tag-block';
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
        <button :class="{active: filter==='block-iface-down'}" class="seg" @click="filter='block-iface-down'">
          Iface down <span class="count">{{ counts.ifdown }}</span>
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
