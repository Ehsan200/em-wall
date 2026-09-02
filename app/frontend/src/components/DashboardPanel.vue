<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted, watch } from 'vue';
import { Line } from 'vue-chartjs';
import {
  Chart as ChartJS,
  CategoryScale, LinearScale, PointElement, LineElement,
  Tooltip, Legend,
} from 'chart.js';
import { UsageStats, Groups } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

ChartJS.register(CategoryScale, LinearScale, PointElement, LineElement, Tooltip, Legend);

type Dimension = 'group' | 'domain' | 'proxy';
type Metric = 'total' | 'sent' | 'recv';
type Range = '1h' | '24h' | '7d';

// Fixed display bucket width (seconds). Stored data is 1-min granularity;
// the dashboard always rolls it up to 5-minute buckets.
const BUCKET_SECS = 300;

const dimension = ref<Dimension>('group');
const metric = ref<Metric>('total');
const range = ref<Range>('24h');
const showTotal = ref<boolean>(true);
const points = ref<ipc.UsagePointDTO[]>([]);
// Group registry (key, brand color, patterns) — fetched once, used to
// paint each group and its member domains in the brand color.
const groupMeta = ref<ipc.GroupDTO[]>([]);
const error = ref<string>('');
const lastRefresh = ref<Date | null>(null);
let timer: number | undefined;

// Show at most this many series; the rest collapse into "other" so a
// long-tail of domains doesn't drown the chart.
const TOP_N = 8;

const RANGE_SECONDS: Record<Range, number> = {
  '1h': 60 * 60,
  '24h': 24 * 60 * 60,
  '7d': 7 * 24 * 60 * 60,
};

function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v >= 100 ? 0 : 1)} ${units[i]}`;
}

function valueOf(p: ipc.UsagePointDTO): number {
  if (metric.value === 'sent') return p.bytesSent;
  if (metric.value === 'recv') return p.bytesRecv;
  return p.bytesSent + p.bytesRecv;
}

async function refresh() {
  const now = Math.floor(Date.now() / 1000);
  const from = now - RANGE_SECONDS[range.value];
  try {
    points.value = (await UsageStats(from, now, dimension.value, BUCKET_SECS)) || [];
    error.value = '';
    lastRefresh.value = new Date();
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

// Distinct bucket timestamps present in the data, ascending.
const buckets = computed(() => {
  const set = new Set<number>();
  for (const p of points.value) set.add(p.bucketUnix);
  return [...set].sort((a, b) => a - b);
});

// Per-key totals across the whole window (for ranking + the table).
const keyTotals = computed(() => {
  const m = new Map<string, { sent: number; recv: number }>();
  for (const p of points.value) {
    const e = m.get(p.key) || { sent: 0, recv: 0 };
    e.sent += p.bytesSent;
    e.recv += p.bytesRecv;
    m.set(p.key, e);
  }
  return m;
});

// Keys to chart: top-N by the active metric, remainder folded into "other".
const chartKeys = computed(() => {
  const ranked = [...keyTotals.value.entries()].sort((a, b) => {
    const va = metric.value === 'sent' ? a[1].sent : metric.value === 'recv' ? a[1].recv : a[1].sent + a[1].recv;
    const vb = metric.value === 'sent' ? b[1].sent : metric.value === 'recv' ? b[1].recv : b[1].sent + b[1].recv;
    return vb - va;
  }).map(e => e[0]);
  return ranked.slice(0, TOP_N);
});

const PALETTE = [
  '#6ea8ff', '#5fd07a', '#ffc857', '#ff6f6f', '#b58cff',
  '#4fd1c5', '#f78fb3', '#f6a623', '#9ad0ec', '#c0c0c0',
];

// Brand colors keyed by group key — source of truth for coloring, so the
// chart shows brand hues even if the running daemon predates GroupDTO.Color.
// Kept in sync with core/groups/match.go brandColors.
const BRAND: Record<string, string> = {
  'anthropic': '#d97757', 'openai': '#10a37f', 'google-ai': '#4285f4',
  'google': '#ea4335', 'google-media': '#00832d', 'microsoft': '#00a4ef',
  'apple': '#d2d2d7', 'sony': '#0070d1',
  'github-copilot': '#8957e5', 'cursor': '#b6b6c2',
  'perplexity': '#22b8cd', 'huggingface': '#ffd21e', 'mistral': '#ff7000',
  'grok': '#e7e9ea', 'x': '#1d9bf0', 'telegram': '#229ed9', 'meta': '#0866ff',
  'youtube': '#ff0000', 'spotify': '#1db954', 'soundcloud': '#ff5500',
  'jetbrains': '#ff318c', 'github': '#e6edf3', 'docker': '#2496ed',
  'homebrew': '#fbb040', 'android-studio': '#3ddc84', 'telemetry-common': '#6c5ce7',
  'python': '#3776ab', 'golang': '#00add8', 'maven': '#c71a36', 'npm': '#cb3837',
  'rust': '#dea584', 'ruby': '#cc342d', 'dotnet': '#a67bff', 'php': '#777bb4',
  'hashicorp': '#a77bff',
};

// Wildcard match mirroring the daemon's rules.Match: `*.x.com` matches the
// apex x.com and any subdomain; a bare pattern matches exactly.
function matchPattern(pat: string, host: string): boolean {
  const p = pat.toLowerCase().replace(/\.$/, '');
  const h = host.toLowerCase().replace(/\.$/, '');
  if (!p || !h) return false;
  if (p.startsWith('*.')) {
    const s = p.slice(2);
    return h === s || h.endsWith('.' + s);
  }
  return p === h;
}

// Brand color for a domain via the group that owns it; "" if none. Group
// membership comes from the daemon's pattern lists (always present); the
// color comes from the local BRAND map (robust to a stale daemon).
function brandForDomain(host: string): string {
  for (const g of groupMeta.value) {
    for (const pat of g.patterns) {
      if (matchPattern(pat, host)) return BRAND[g.key] || '';
    }
  }
  return '';
}

// Deterministic fallback palette color, stable per key across refreshes
// (hash, not rank index, so a series keeps its color as data shifts).
function paletteFor(key: string): string {
  let h = 0;
  for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) >>> 0;
  return PALETTE[h % PALETTE.length];
}

// Resolve a series' color: brand color for groups/domains, deterministic
// palette for proxies and anything ungrouped. Fixed colors for the
// synthetic Total/other/ungrouped series.
function colorFor(key: string): string {
  if (key === 'Total (all)') return '#ffffff';
  if (key === 'other') return '#6a6a78';
  if (key === 'ungrouped' || key === '(unknown)') return '#8a8a96';
  if (dimension.value === 'group') {
    if (BRAND[key]) return BRAND[key];
  } else if (dimension.value === 'domain') {
    const c = brandForDomain(key);
    if (c) return c;
  }
  return paletteFor(key);
}

const chartData = computed(() => {
  const labels = buckets.value.map(b =>
    new Date(b * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }));
  const bucketIndex = new Map(buckets.value.map((b, i) => [b, i]));
  const keys = chartKeys.value;
  const keySet = new Set(keys);

  // series[key] -> aligned values per bucket; "other" catches the tail.
  const series = new Map<string, number[]>();
  for (const k of keys) series.set(k, new Array(buckets.value.length).fill(0));
  let hasOther = false;
  const other = new Array(buckets.value.length).fill(0);

  for (const p of points.value) {
    const idx = bucketIndex.get(p.bucketUnix);
    if (idx === undefined) continue;
    const v = valueOf(p);
    if (keySet.has(p.key)) {
      series.get(p.key)![idx] += v;
    } else {
      other[idx] += v;
      if (v > 0) hasOther = true;
    }
  }

  const datasets = keys.map((k) => {
    const c = colorFor(k);
    return {
      label: k,
      data: series.get(k)!,
      borderColor: c,
      backgroundColor: c,
      fill: false,
      tension: 0,
      pointRadius: 3,
      pointHoverRadius: 5,
      borderWidth: 2,
    };
  });
  if (hasOther) {
    datasets.push({
      label: 'other',
      data: other,
      borderColor: colorFor('other'),
      backgroundColor: colorFor('other'),
      fill: false,
      tension: 0,
      pointRadius: 3,
      pointHoverRadius: 5,
      borderWidth: 2,
    });
  }
  // "Total (all)" — sum of every series (incl. tail) per bucket, so the
  // user sees overall traffic regardless of top-N truncation.
  if (showTotal.value) {
    const total = new Array(buckets.value.length).fill(0);
    for (const p of points.value) {
      const idx = bucketIndex.get(p.bucketUnix);
      if (idx === undefined) continue;
      total[idx] += valueOf(p);
    }
    datasets.unshift({
      label: 'Total (all)',
      data: total,
      borderColor: '#ffffff',
      backgroundColor: '#ffffff',
      fill: false,
      tension: 0,
      pointRadius: 3,
      pointHoverRadius: 5,
      borderWidth: 2.5,
      borderDash: [6, 3],
    } as any);
  }
  return { labels, datasets };
});

// Static color-key for the custom legend (label + line color, dashed flag
// for the Total series). Non-interactive by design.
const legendItems = computed(() =>
  chartData.value.datasets.map((d: any) => ({
    label: d.label as string,
    color: d.borderColor as string,
    dashed: Array.isArray(d.borderDash) && d.borderDash.length > 0,
  })));

// A key that changes whenever the rendered series materially change.
// Bound to <Line :key> so vue-chartjs always reflects fresh data — the
// in-place reactive update can miss when only data values (not identity)
// shift on the 5s poll.
const chartKey = computed(() =>
  `${dimension.value}:${metric.value}:${showTotal.value}:` +
  `${groupMeta.value.length}:${buckets.value.length}:${buckets.value[buckets.value.length - 1] ?? 0}:${points.value.length}`);

const chartOptions = computed(() => ({
  responsive: true,
  maintainAspectRatio: false,
  interaction: { mode: 'index' as const, intersect: false },
  animation: false as const,
  scales: {
    x: {
      ticks: { color: '#8d8da0', maxTicksLimit: 8, font: { size: 10 } },
      grid: { color: '#2a2a3633' },
    },
    y: {
      beginAtZero: true,
      ticks: { color: '#8d8da0', font: { size: 10 }, callback: (v: any) => humanBytes(Number(v)) },
      grid: { color: '#2a2a3633' },
    },
  },
  plugins: {
    // Native legend disabled — it let clicks hide series, which clashed
    // with the toolbar. A custom static color-key renders below instead.
    legend: { display: false },
    tooltip: {
      callbacks: { label: (c: any) => `${c.dataset.label}: ${humanBytes(Number(c.parsed.y))}` },
    },
  },
}));

// Table rows: every key (not just top-N), sorted by total desc.
const tableRows = computed(() =>
  [...keyTotals.value.entries()]
    .map(([key, v]) => ({ key, sent: v.sent, recv: v.recv, total: v.sent + v.recv }))
    .sort((a, b) => b.total - a.total));

const grandTotal = computed(() =>
  tableRows.value.reduce((acc, r) => ({ sent: acc.sent + r.sent, recv: acc.recv + r.recv }), { sent: 0, recv: 0 }));

// Dimension / range change the server query → refetch.
// Metric is a pure client-side recompute, no refetch needed.
watch([dimension, range], refresh);

onMounted(async () => {
  // Group registry is static; fetch once for brand colors + domain matching.
  try { groupMeta.value = (await Groups()) || []; } catch { /* fall back to palette */ }
  refresh();
  timer = window.setInterval(refresh, 5000);
});
onUnmounted(() => { if (timer) window.clearInterval(timer); });
</script>

<template>
  <div class="panel">
    <div v-if="error" class="error">{{ error }}</div>

    <div class="note">
      Shows data volume for <strong>proxied</strong> traffic only (SOCKS / Xray routes).
      DNS-only and interface-routed traffic does not pass through the daemon and can't be measured.
    </div>

    <div class="toolbar">
      <div class="ctl">
        <span class="ctl-label">Group by</span>
        <div class="row" style="gap: 0">
          <button :class="{active: dimension==='group'}"  class="seg" @click="dimension='group'">Group</button>
          <button :class="{active: dimension==='domain'}" class="seg" @click="dimension='domain'">Domain</button>
          <button :class="{active: dimension==='proxy'}"  class="seg" @click="dimension='proxy'">Proxy</button>
        </div>
      </div>
      <div class="ctl">
        <span class="ctl-label">Direction</span>
        <div class="row" style="gap: 0">
          <button :class="{active: metric==='total'}" class="seg" @click="metric='total'">Total</button>
          <button :class="{active: metric==='sent'}"  class="seg" @click="metric='sent'">Sent ↑</button>
          <button :class="{active: metric==='recv'}"  class="seg" @click="metric='recv'">Recv ↓</button>
        </div>
      </div>
      <div class="ctl">
        <span class="ctl-label">Range</span>
        <div class="row" style="gap: 0">
          <button :class="{active: range==='1h'}"  class="seg" @click="range='1h'">1h</button>
          <button :class="{active: range==='24h'}" class="seg" @click="range='24h'">24h</button>
          <button :class="{active: range==='7d'}"  class="seg" @click="range='7d'">7d</button>
        </div>
      </div>
      <div class="ctl">
        <span class="ctl-label">Total line</span>
        <button :class="{active: showTotal}" class="seg solo" @click="showTotal = !showTotal"
                title="Overlay a Total (all) line summing every series">{{ showTotal ? 'On' : 'Off' }}</button>
      </div>
    </div>

    <div class="muted" style="font-size: 11px; margin-bottom: 8px">
      live · 5s refresh · {{ lastRefresh ? lastRefresh.toLocaleTimeString() : '—' }} ·
      total ↑ {{ humanBytes(grandTotal.sent) }} · ↓ {{ humanBytes(grandTotal.recv) }}
    </div>

    <div v-if="buckets.length" class="legend">
      <span v-for="it in legendItems" :key="it.label" class="legend-item">
        <span class="swatch" :class="{ dashed: it.dashed }" :style="{ background: it.color }"></span>
        {{ it.label }}
      </span>
    </div>

    <div class="chart-wrap">
      <Line v-if="buckets.length" :key="chartKey" :data="chartData" :options="chartOptions" />
      <div v-else class="muted empty">No proxied traffic recorded in this window yet.</div>
    </div>

    <table>
      <thead>
        <tr>
          <th>{{ dimension === 'group' ? 'Group' : dimension === 'proxy' ? 'Proxy' : 'Domain' }}</th>
          <th style="text-align:right">Sent ↑</th>
          <th style="text-align:right">Recv ↓</th>
          <th style="text-align:right">Total</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="r in tableRows" :key="r.key">
          <td>
            <span class="dot" :style="{ background: colorFor(r.key) }"></span>
            <code>{{ r.key }}</code>
          </td>
          <td class="muted num">{{ humanBytes(r.sent) }}</td>
          <td class="muted num">{{ humanBytes(r.recv) }}</td>
          <td class="num"><strong>{{ humanBytes(r.total) }}</strong></td>
        </tr>
        <tr v-if="!tableRows.length">
          <td colspan="4" class="muted" style="text-align:center; padding:24px">
            Nothing to show. Route a rule through a proxy and generate some traffic.
          </td>
        </tr>
      </tbody>
      <tfoot v-if="tableRows.length">
        <tr class="total-row">
          <td><strong>Total (all)</strong></td>
          <td class="num">{{ humanBytes(grandTotal.sent) }}</td>
          <td class="num">{{ humanBytes(grandTotal.recv) }}</td>
          <td class="num"><strong>{{ humanBytes(grandTotal.sent + grandTotal.recv) }}</strong></td>
        </tr>
      </tfoot>
    </table>
  </div>
</template>

<style scoped>
.note {
  font-size: 12px;
  color: var(--text-dim);
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 8px 12px;
  margin-bottom: 12px;
}
.toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: 16px;
  margin-bottom: 14px;
}
.ctl {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.ctl-label {
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-dim);
}
.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 14px;
  margin-bottom: 8px;
  padding: 8px 12px;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 6px;
}
.legend-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--text);
}
.swatch {
  width: 14px;
  height: 3px;
  border-radius: 2px;
  display: inline-block;
}
.swatch.dashed {
  background-image: repeating-linear-gradient(90deg, currentColor 0 4px, transparent 4px 7px) !important;
}
.chart-wrap {
  position: relative;
  height: 320px;
  margin-bottom: 16px;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 12px;
}
.empty {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 100%;
  font-size: 13px;
}
.num { text-align: right; white-space: nowrap; font-variant-numeric: tabular-nums; }
.dot {
  display: inline-block;
  width: 9px;
  height: 9px;
  border-radius: 50%;
  margin-right: 7px;
  vertical-align: middle;
}
.total-row td {
  border-top: 2px solid var(--border);
  background: var(--panel-2);
}
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
.seg.solo {
  border: 1px solid var(--border);
  border-radius: 6px;
}
.seg.solo.active { border-color: var(--accent); }
</style>
