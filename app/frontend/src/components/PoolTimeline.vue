<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import { HealthPools } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

// Per-node health history of each master's subscription pool, sampled by
// the daemon every 10 s. Observation only: it shows whether a pool is
// steady or its nodes die and return in waves, and whether the nodes the
// balancer is using are the ones answering. Tall cells = the node was
// carrying traffic (shortlisted / fallback); short = ranked out.

const WINDOWS = [
  { label: '10 min', sec: 600 },
  { label: '30 min', sec: 1800 },
];

const pools = ref<ipc.PoolTimelineDTO[]>([]);
const windowSec = ref(600);
const error = ref('');
let timer: number | undefined;

async function refresh() {
  try {
    pools.value = (await HealthPools('', windowSec.value)) ?? [];
    error.value = '';
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  }
}

function setWindow(sec: number) {
  windowSec.value = sec;
  refresh();
}

const STATE_LABEL: Record<string, string> = {
  A: 'up',
  F: 'some pings lost',
  D: 'down',
  P: 'parked',
  '?': 'no pings yet',
  ' ': 'not in pool',
};
const ROLE_LABEL: Record<string, string> = {
  f: 'fallback (carrying)',
  a: 'active (carrying)',
  i: 'ranked out',
  u: 'unranked',
  ' ': '',
};

function carrying(role: string): boolean {
  return role === 'f' || role === 'a';
}

function cellClass(state: string, role: string): string[] {
  return ['cell', `s-${{ A: 'up', F: 'flaky', D: 'down', P: 'parked', '?': 'unknown' }[state] ?? 'absent'}`,
    carrying(role) ? 'carry' : 'idle'];
}

function bytes(n: number): string {
  if (n <= 0) return '—';
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1 << 30) return `${(n / (1 << 20)).toFixed(1)} MB`;
  return `${(n / (1 << 30)).toFixed(1)} GB`;
}

function clock(unix: number): string {
  return new Date(unix * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

function cellTitle(p: ipc.PoolTimelineDTO, n: ipc.PoolNodeTimelineDTO, i: number): string {
  const parts = [clock(p.times[i]), STATE_LABEL[n.states[i]] ?? n.states[i]];
  const role = ROLE_LABEL[n.roles[i]];
  if (role) parts.push(role);
  if (n.rttMs[i] > 0) parts.push(`${n.rttMs[i]} ms`);
  if (n.upBytes[i] > 0 || n.downBytes[i] > 0) parts.push(`↑${bytes(n.upBytes[i])} ↓${bytes(n.downBytes[i])}`);
  if (p.uplinkDown[i]) parts.push('no node answered — local uplink');
  return parts.join(' · ');
}

const MASTER_LABEL: Record<string, string> = {
  O: 'ok',
  X: 'demoted — ranked last in its sets',
  D: 'down — in an outage',
  '?': 'not measured',
};

function masterClass(c: string): string[] {
  return ['cell', 'carry', `m-${{ O: 'ok', X: 'demoted', D: 'down' }[c] ?? 'unknown'}`];
}

const STRATEGY_HELP: Record<string, string> = {
  stable: 'Stable — ranks nodes over time, keeps a sticky pair, parks nodes that stay dead',
  agile: 'Agile — follows the nodes answering right now, no parking; stalled connections on a node that died are closed so apps reconnect',
  manual: 'Manual — routes only through the pinned nodes',
};

function strategyText(p: ipc.PoolTimelineDTO): string {
  if (!p.strategy) return '';
  if (!p.strategyAuto) return p.strategy;
  let out = `auto → ${p.strategy}`;
  if (p.strategySince) out += ` since ${new Date(p.strategySince).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
  return out;
}

function strategyTitle(p: ipc.PoolTimelineDTO): string {
  const parts = [STRATEGY_HELP[p.strategy] ?? p.strategy];
  if (p.strategyAuto) parts.push(`Chosen by auto: ${p.strategyReason || 'no flapping seen'}`);
  return parts.join('\n');
}

function roleText(r: string): string {
  return { f: 'fallback', a: 'active', i: 'idle', u: 'unranked' }[r] ?? 'gone';
}

const visible = computed(() => pools.value.filter(p => p.nodes.length > 0));

onMounted(() => {
  refresh();
  timer = window.setInterval(refresh, 10000);
});
onUnmounted(() => { if (timer) window.clearInterval(timer); });
</script>

<template>
  <section v-if="visible.length || error" class="pools">
    <div class="head">
      <h3>Pool timeline</h3>
      <span class="muted">subscription nodes behind each master · one cell per 10 s · tall = carrying traffic</span>
      <div class="seg-group">
        <button v-for="w in WINDOWS" :key="w.sec" class="seg" :class="{ active: windowSec === w.sec }"
                @click="setWindow(w.sec)">{{ w.label }}</button>
      </div>
    </div>
    <div v-if="error" class="error">{{ error }}</div>

    <div v-for="p in visible" :key="p.master" class="pool">
      <div class="pool-head">
        <code>{{ p.master }}</code>
        <span v-if="p.masters.length > 1" class="muted">shared by {{ p.masters.join(', ') }}</span>
        <span v-if="p.strategy" class="pill strategy" :class="p.strategy" :title="strategyTitle(p)">{{ strategyText(p) }}</span>
        <span class="stat" :class="{ bad: p.pickLosses > 0 }"
              title="Times every node carrying traffic was down at once while another node answered">
          {{ p.pickLosses }} pick {{ p.pickLosses === 1 ? 'loss' : 'losses' }}
        </span>
        <span class="stat" :class="{ warn: p.flappers > 0 }"
              title="Nodes that went down and came back (or the reverse) at least twice">
          {{ p.flappers }}/{{ p.nodes.length }} flapping
        </span>
        <span class="stat muted" title="Share of nodes whose state changed at all">churn {{ p.churnPct.toFixed(0) }}%</span>
      </div>

      <div class="grid">
        <span class="name muted">uplink</span>
        <div class="strip">
          <span v-for="(down, i) in p.uplinkDown" :key="i" class="cell uplink" :class="{ lost: down }"
                :title="down ? `${clock(p.times[i])} · no node answered — local uplink` : clock(p.times[i])" />
        </div>
        <span class="name muted">swap</span>
        <div class="strip">
          <span v-for="(sw, i) in p.swaps" :key="i" class="cell swap" :class="{ on: sw }"
                :title="sw ? `${clock(p.times[i])} · shortlist changed` : clock(p.times[i])" />
        </div>
        <span /><span /><span /><span /><span />

        <span class="num muted">up</span>
        <span class="num muted">flips</span>
        <span class="num muted">rtt</span>
        <span class="num muted">recv</span>
        <span class="muted">role</span>

        <template v-for="n in p.nodes" :key="n.key">
          <span class="name" :title="n.name">{{ n.name }}</span>
          <div class="strip">
            <span v-for="(s, i) in n.states" :key="i" :class="cellClass(s, n.roles[i])" :title="cellTitle(p, n, i)" />
          </div>
          <span class="num">{{ n.uptimePct.toFixed(0) }}%</span>
          <span class="num" :class="{ warn: n.flips >= 2 }">{{ n.flips }}</span>
          <span class="num muted">{{ n.avgRttMs > 0 ? `${n.avgRttMs} ms` : '—' }}</span>
          <span class="num muted" :title="`sent ${bytes(n.totalUp)}`">{{ bytes(n.totalDown) }}</span>
          <span class="muted role">{{ roleText(n.role) }}</span>
        </template>

        <template v-if="p.masterRows?.length">
          <span class="name muted section">master</span>
          <span class="muted section strip-note">as the set ranking saw it</span>
          <span class="num muted section" title="Outage starts in the window">outages</span>
          <span class="num muted section" title="Outages that began while a node carrying the pool was answering — the master or the path past the node, not the pool">node up</span>
          <span class="num muted section" title="Outages that began within a minute of a shortlist swap">near swap</span>
          <span class="num muted section">demoted</span>
          <span />
          <template v-for="m in p.masterRows" :key="m.name">
            <code class="name" :title="m.name">{{ m.name }}</code>
            <div class="strip">
              <span v-for="(c, i) in m.states" :key="i" :class="masterClass(c)"
                    :title="`${clock(p.times[i])} · ${MASTER_LABEL[c] ?? c}`" />
            </div>
            <span class="num" :class="{ bad: m.outages > 0 }">{{ m.outages }}</span>
            <span class="num" :class="{ warn: m.outagesCarrierUp > 0 }">{{ m.outagesCarrierUp }}</span>
            <span class="num" :class="{ warn: m.outagesNearSwap > 0 }">{{ m.outagesNearSwap }}</span>
            <span class="num">{{ m.demotions }}</span>
            <span />
          </template>
        </template>
      </div>
    </div>

    <div class="legend muted">
      <span><i class="cell s-up carry" /> up</span>
      <span><i class="cell s-flaky carry" /> some pings lost</span>
      <span><i class="cell s-down carry" /> down</span>
      <span><i class="cell s-parked carry" /> parked</span>
      <span><i class="cell s-up idle" /> ranked out</span>
      <span><i class="cell carry m-demoted" /> master demoted</span>
      <span><i class="cell carry m-down" /> master down</span>
    </div>
  </section>
</template>

<style scoped>
.pools {
  margin-bottom: 20px;
  padding: 12px;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 8px;
}
.head {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 10px;
}
.head h3 { margin: 0; font-size: 14px; }
.head .muted { font-size: 11px; }
.seg-group { margin-left: auto; display: flex; }
.seg {
  background: transparent;
  border: 1px solid var(--border);
  border-right: none;
  border-radius: 0;
  padding: 2px 10px;
  color: var(--text-dim);
  font-size: 11px;
}
.seg:first-child { border-top-left-radius: 6px; border-bottom-left-radius: 6px; }
.seg:last-child  { border-right: 1px solid var(--border); border-top-right-radius: 6px; border-bottom-right-radius: 6px; }
.seg.active { color: var(--text); background: var(--panel); border-color: var(--accent); }
.pool { margin-bottom: 14px; }
.pool:last-of-type { margin-bottom: 8px; }
.pool-head {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 6px;
  font-size: 12px;
}
.stat { font-variant-numeric: tabular-nums; }
.pill.strategy {
  padding: 0 8px;
  border-radius: 10px;
  font-size: 11px;
  border: 1px solid var(--border);
  background: var(--panel);
  color: var(--text-dim);
}
.pill.strategy.agile { color: var(--warn); border-color: var(--warn); }
.pill.strategy.manual { color: var(--accent); border-color: var(--accent); }
.warn { color: var(--warn); }
.bad { color: var(--danger); }
.grid {
  display: grid;
  grid-template-columns: minmax(80px, 200px) minmax(0, 1fr) auto auto auto auto auto;
  column-gap: 10px;
  row-gap: 3px;
  align-items: center;
  font-size: 11px;
}
.name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.num { text-align: right; white-space: nowrap; font-variant-numeric: tabular-nums; }
.role { white-space: nowrap; }
.strip {
  display: flex;
  align-items: flex-end;
  gap: 1px;
  height: 14px;
  min-width: 0;
}
.strip .cell { flex: 1 1 0; min-width: 1px; }
.cell { display: inline-block; height: 14px; border-radius: 1px; background: transparent; }
.cell.idle { height: 6px; }
.s-up { background: var(--success); }
.s-flaky { background: var(--warn); }
.s-down { background: var(--danger); }
.s-parked { background: var(--text-dim); opacity: 0.5; }
.s-unknown { background: var(--border); }
.cell.uplink { height: 4px; background: var(--border); }
.cell.swap { height: 2px; background: transparent; }
.cell.swap.on { height: 10px; background: var(--accent); }
.m-ok { background: var(--success); opacity: 0.35; }
.m-demoted { background: var(--warn); }
.m-down { background: var(--danger); }
.m-unknown { background: var(--border); }
.section { padding-top: 6px; }
.strip-note { font-size: 10px; }
.cell.uplink.lost { height: 14px; background: var(--danger); opacity: 0.6; }
.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 14px;
  font-size: 11px;
}
.legend span { display: inline-flex; align-items: flex-end; gap: 4px; }
.legend .cell { width: 8px; }
</style>
