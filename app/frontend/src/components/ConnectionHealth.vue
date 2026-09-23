<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import { HealthStats } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

// Proxied-connection health over the daemon's rolling window. The point is
// to answer "is it the app or the network?" with numbers: how often a
// connection fails and why, how long setup takes, and which upstream is
// pulling its weight.

const stats = ref<ipc.HealthStatsDTO | null>(null);
const error = ref('');
let timer: number | undefined;

async function refresh() {
  try {
    stats.value = await HealthStats();
    error.value = '';
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  }
}

const CAUSE_LABELS: Record<string, string> = {
  'no-upstream': 'no upstream answered',
  'no-data': 'connected, no reply',
  'paused': 'destination paused',
  'dial-ceiling': 'dial limit hit',
  'no-mapping': 'unknown destination',
};

const failedTotal = computed(() =>
  Object.values(stats.value?.failed ?? {}).reduce((a, n) => a + n, 0));

const failPct = computed(() => {
  const n = stats.value?.connections ?? 0;
  return n ? (100 * failedTotal.value) / n : 0;
});

const causes = computed(() =>
  Object.entries(stats.value?.failed ?? {})
    .filter(([, n]) => n > 0)
    .sort((a, b) => b[1] - a[1]));

const udpSilentPct = computed(() => {
  const n = stats.value?.udpFlows ?? 0;
  return n ? (100 * (stats.value?.udpSilent ?? 0)) / n : 0;
});

const upstreams = computed(() => (stats.value?.upstreams ?? []).filter(u =>
  u.connections > 0 || u.blamed > 0 || u.breakerOpen || u.rttMs > 0));

function ms(v: number): string {
  if (v < 0) return '—';
  if (v > 20000) return '>20 s';
  return v >= 1000 ? `≤${(v / 1000).toFixed(v % 1000 ? 1 : 0)} s` : `≤${v} ms`;
}

function pct(v: number): string {
  return `${v.toFixed(v > 0 && v < 10 ? 1 : 0)}%`;
}

function failClass(p: number): string {
  if (p >= 10) return 'bad';
  if (p >= 2) return 'warn';
  return 'good';
}

function until(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime()) ? iso : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

onMounted(() => {
  refresh();
  timer = window.setInterval(refresh, 5000);
});
onUnmounted(() => { if (timer) window.clearInterval(timer); });
</script>

<template>
  <section class="health">
    <div class="head">
      <h3>Connection health</h3>
      <span class="muted">last {{ Math.round((stats?.windowSec ?? 900) / 60) }} min · proxied traffic</span>
    </div>
    <div v-if="error" class="error">{{ error }}</div>

    <div class="tiles">
      <div class="tile">
        <span class="label">Connections</span>
        <span class="value">{{ stats?.connections ?? 0 }}</span>
      </div>
      <div class="tile">
        <span class="label">Failed</span>
        <span class="value" :class="failClass(failPct)">{{ pct(failPct) }}</span>
        <span class="sub">{{ failedTotal }} of {{ stats?.connections ?? 0 }}</span>
      </div>
      <div class="tile">
        <span class="label">Setup time</span>
        <span class="value">{{ ms(stats?.setupP50Ms ?? -1) }}</span>
        <span class="sub">p95 {{ ms(stats?.setupP95Ms ?? -1) }}</span>
      </div>
      <div class="tile">
        <span class="label">Extra attempts</span>
        <span class="value">{{ stats?.extraAttempts ?? 0 }}</span>
        <span class="sub">raced or retried dials</span>
      </div>
      <div class="tile">
        <span class="label">QUIC / UDP silent</span>
        <span class="value" :class="failClass(udpSilentPct)">{{ pct(udpSilentPct) }}</span>
        <span class="sub">{{ stats?.udpSilent ?? 0 }} of {{ stats?.udpFlows ?? 0 }} flows</span>
      </div>
      <div class="tile">
        <span class="label">Xray</span>
        <span class="value" :class="(stats?.xrayRestarts ?? 0) > 0 ? 'warn' : ''">{{ stats?.xrayRestarts ?? 0 }} restarts</span>
        <span class="sub">{{ stats?.xrayLiveApplies ?? 0 }} changes applied live</span>
      </div>
    </div>

    <div v-if="causes.length" class="causes">
      <span class="label">Failures by cause</span>
      <span v-for="[cause, n] in causes" :key="cause" class="cause">
        {{ CAUSE_LABELS[cause] ?? cause }} <strong>{{ n }}</strong>
      </span>
    </div>

    <table v-if="upstreams.length">
      <thead>
        <tr>
          <th>Upstream</th>
          <th class="num">Carried</th>
          <th class="num">Blamed</th>
          <th class="num">No reply</th>
          <th class="num">Setup p50</th>
          <th class="num">Probe RTT</th>
          <th>State</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="u in upstreams" :key="u.name">
          <td><code>{{ u.name }}</code></td>
          <td class="num">{{ u.connections }}</td>
          <td class="num" :class="{ warn: u.blamed > 0 }">{{ u.blamed }}</td>
          <td class="num" :class="{ warn: u.noData > 0 }">{{ u.noData }}</td>
          <td class="num muted">{{ ms(u.setupP50Ms) }}</td>
          <td class="num muted">{{ u.rttMs > 0 ? `${u.rttMs} ms` : '—' }}</td>
          <td>
            <span v-if="u.breakerOpen" class="pill bad"
                  :title="`${pct(u.failureRate * 100)} of recent attempts failed — ranked last until it recovers`">
              demoted
            </span>
            <span v-else class="pill good">ok</span>
          </td>
        </tr>
      </tbody>
    </table>
    <div v-else class="muted empty">No proxied connections in this window yet.</div>

    <div v-if="stats?.parkedNodes?.length" class="parked">
      <span class="label">Parked pool nodes</span>
      <span class="muted">dead for a while — skipped until their next trial</span>
      <div class="parked-list">
        <span v-for="p in stats.parkedNodes" :key="p.name" class="pill">
          {{ p.name }} <span class="muted">· trial {{ until(p.until) }}</span>
        </span>
      </div>
    </div>
  </section>
</template>

<style scoped>
.health {
  margin-bottom: 20px;
  padding: 12px;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 8px;
}
.head {
  display: flex;
  align-items: baseline;
  gap: 10px;
  margin-bottom: 10px;
}
.head h3 { margin: 0; font-size: 14px; }
.head .muted { font-size: 11px; }
.tiles {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));
  gap: 8px;
  margin-bottom: 10px;
}
.tile {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 8px 10px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 6px;
}
.label {
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-dim);
}
.value { font-size: 18px; font-weight: 600; font-variant-numeric: tabular-nums; }
.sub { font-size: 11px; color: var(--text-dim); }
.good { color: var(--success); }
.warn { color: var(--warn); }
.bad { color: var(--danger); }
.causes, .parked {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin-bottom: 10px;
  font-size: 12px;
}
.parked { margin: 10px 0 0; }
.parked-list { display: flex; flex-wrap: wrap; gap: 6px; width: 100%; }
.cause strong { font-variant-numeric: tabular-nums; }
.num { text-align: right; white-space: nowrap; font-variant-numeric: tabular-nums; }
.pill {
  display: inline-block;
  padding: 1px 8px;
  border-radius: 10px;
  font-size: 11px;
  border: 1px solid var(--border);
  background: var(--panel);
}
.pill.good { color: var(--success); }
.pill.bad { color: var(--danger); border-color: var(--danger); }
.empty { padding: 12px; text-align: center; font-size: 12px; }
</style>
