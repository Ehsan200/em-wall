<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted, watch, nextTick } from 'vue';
import { startPoll } from '../poll';
import { MapPaths } from '../../wailsjs/go/main/App';
import GroupIcon from './GroupIcon.vue';
import SearchSelect from './SearchSelect.vue';
import WorldMap from './map/WorldMap.vue';
import { flowKey, flowRate, fmtRate, fmtBytes, groupColor, hopShortPlace, placed } from './map/types';
import type { MapResult, ViewFlow } from './map/types';
import type { MockOptions } from './map/mock';

// World map of live proxied flows: you → pool node → server → exit →
// destination, one arc per hop, the group's icon travelling along it. All
// the data comes from map.paths (the daemon locates hops in its offline
// geo DB); this panel only draws it.

const result = ref<MapResult | null>(null);
const error = ref('');
const loaded = ref(false);
let stopPoll: (() => void) | undefined;
let seq = 0;

// Dev-only sample data (see map/mock.ts for how to switch it on). The
// import.meta.env.DEV guard is folded at build time, so a production
// bundle never contains the mock.
const mock = ref<MockOptions | null>(null);
let mockPaths: ((o: MockOptions) => MapResult) | null = null;

async function refresh() {
  const mine = ++seq;
  try {
    let got: MapResult;
    if (mock.value && mockPaths) got = mockPaths(mock.value);
    // Filters are applied here, not in the call: the select options are
    // derived from the current flows, and a server-side filter would
    // shrink them to the one already chosen.
    else got = (await MapPaths('', '')) as unknown as MapResult;
    if (mine !== seq) return;
    result.value = got;
    error.value = '';
  } catch (e: any) {
    if (mine !== seq) return;
    error.value = String(e?.message ?? e);
  }
  loaded.value = true;
}

// ---- filters ----
// "" in the select means "all"; a flow with no group gets its own value.
const NO_GROUP = '__none__';
function groupValue(g: string): string { return g || NO_GROUP; }

const groupFilter = ref('');
const ifaceFilter = ref('');

const allFlows = computed<ViewFlow[]>(() =>
  (result.value?.flows ?? []).map((f) => ({
    key: flowKey(f),
    flow: f,
    color: groupColor(f),
    rate: flowRate(f),
    label: f.groupName || f.group || 'No group',
  })));

const groupOptions = computed(() => {
  const m = new Map<string, string>();
  for (const vf of allFlows.value) m.set(groupValue(vf.flow.group), vf.label);
  // Keep the chosen group listed after its last flow ends.
  if (groupFilter.value && !m.has(groupFilter.value)) m.set(groupFilter.value, groupFilter.value);
  return [...m.entries()].sort((a, b) => a[1].localeCompare(b[1]));
});
const ifaceOptions = computed(() => {
  const s = new Set<string>();
  for (const vf of allFlows.value) {
    if (!groupFilter.value || groupValue(vf.flow.group) === groupFilter.value) {
      if (vf.flow.iface) s.add(vf.flow.iface);
    }
  }
  if (ifaceFilter.value) s.add(ifaceFilter.value);
  return [...s].sort();
});

const flows = computed(() =>
  allFlows.value
    .filter((vf) => !groupFilter.value || groupValue(vf.flow.group) === groupFilter.value)
    .filter((vf) => !ifaceFilter.value || vf.flow.iface === ifaceFilter.value)
    .sort((a, b) => b.rate - a.rate || b.flow.conns - a.flow.conns));

const totals = computed(() => {
  let up = 0, down = 0, conns = 0;
  for (const vf of flows.value) { up += vf.flow.rateUp || 0; down += vf.flow.rateDown || 0; conns += vf.flow.conns || 0; }
  return { up, down, conns };
});

const unplacedHops = computed(() => {
  let n = 0;
  for (const vf of flows.value) for (const h of vf.flow.hops ?? []) if (!placed(h)) n++;
  return n;
});

// ---- view ----
const fit = ref(true);

const reducedMotion = ref(false);
let mq: MediaQueryList | undefined;
const onMq = () => { reducedMotion.value = !!mq?.matches; };

// ---- highlight / selection ----
const hover = ref<string[]>([]);
const selected = ref<string | null>(null);
const active = computed(() => (selected.value ? [selected.value, ...hover.value] : hover.value));
const activeSet = computed(() => new Set(active.value));

// A selection whose flow ended goes away with it.
watch(allFlows, (fs) => {
  if (selected.value && !fs.some((f) => f.key === selected.value)) selected.value = null;
});

const listEl = ref<HTMLElement | null>(null);
function onMapSelect(key: string | null) {
  selected.value = key && selected.value !== key ? key : null;
  if (selected.value) {
    nextTick(() => {
      listEl.value?.querySelector<HTMLElement>(`[data-key="${CSS.escape(selected.value!)}"]`)
        ?.scrollIntoView({ block: 'nearest' });
    });
  }
}
function toggleRow(key: string) {
  selected.value = selected.value === key ? null : key;
}

function pathSummary(vf: ViewFlow): string {
  return (vf.flow.hops ?? []).map(hopShortPlace).join(' → ');
}
function hostsText(vf: ViewFlow): string {
  const hs = vf.flow.hosts ?? [];
  if (!hs.length) return '—';
  return hs.length > 2 ? `${hs.slice(0, 2).join(', ')} +${hs.length - 2}` : hs.join(', ');
}

onMounted(async () => {
  mq = window.matchMedia('(prefers-reduced-motion: reduce)');
  onMq();
  mq.addEventListener('change', onMq);
  if (import.meta.env.DEV) {
    const m = await import('./map/mock');
    mock.value = m.mockOptions();
    mockPaths = m.mockPaths;
  }
  await refresh();
  stopPoll = startPoll(refresh, 2000);
});
onUnmounted(() => {
  stopPoll?.();
  mq?.removeEventListener('change', onMq);
});
</script>

<template>
  <div class="panel map-panel">
    <div class="head" data-tour="map-head">
      <h2>Map</h2>
      <span v-if="mock" class="mock-pill" title="Dev build showing sample data (map/mock.ts)">sample data</span>
      <div class="filters">
        <SearchSelect v-model="groupFilter" aria-label="Group" @change="ifaceFilter = ''"
                      :options="[{ value: '', label: 'All groups' }, ...groupOptions.map(([g, name]) => ({ value: g, label: name }))]"
                      search-placeholder="search groups…" style="min-width: 160px" />
        <SearchSelect v-model="ifaceFilter" aria-label="Set"
                      :options="[{ value: '', label: 'All sets' }, ...ifaceOptions]"
                      search-placeholder="search sets…" style="min-width: 160px" />
        <div class="seg-group" role="group" aria-label="View">
          <button class="seg" :class="{ active: fit }" @click="fit = true">Fit paths</button>
          <button class="seg" :class="{ active: !fit }" @click="fit = false">World</button>
        </div>
      </div>
      <div v-if="loaded && flows.length" class="totals muted">
        <span>{{ flows.length }} {{ flows.length === 1 ? 'flow' : 'flows' }}</span>
        <span>{{ totals.conns }} conn</span>
        <span>↓ {{ fmtRate(totals.down) }}</span>
        <span>↑ {{ fmtRate(totals.up) }}</span>
      </div>
    </div>

    <div v-if="error" class="error">{{ error }}</div>
    <div v-if="result && !result.cityData" class="banner">
      Country precision — city data is downloading. Hops sit at their country's centre until it arrives.
    </div>

    <div v-if="!loaded" class="body">
      <div class="skeleton map-skel"></div>
      <div class="side"><div class="skeleton-stack"><span v-for="i in 5" :key="i" class="skeleton" style="height: 44px"></span></div></div>
    </div>

    <div v-else-if="!allFlows.length && !error" class="empty">
      <strong>No proxied connections right now</strong>
      <span class="muted">
        Flows show up here while traffic goes through a <code>proxy:</code>, <code>xray:</code> or
        <code>xrayset:</code> rule. Direct and blocked traffic isn't drawn.
      </span>
    </div>

    <div v-else-if="allFlows.length" class="body">
      <div class="map-col">
        <WorldMap :flows="flows" :active="active" :fit="fit" :reducedMotion="reducedMotion"
                  @hover="(k: string[]) => hover = k" @select="onMapSelect" />
        <div class="legend muted">
          <span><i class="dot r-you"></i>You</span>
          <span><i class="dot r-pool"></i>Pool node</span>
          <span><i class="dot r-server"></i>Server</span>
          <span><i class="dot r-exit"></i>Exit</span>
          <span><i class="dot r-dest"></i>Destination</span>
          <span><i class="dash"></i>Uncertain</span>
          <span><i class="ring"></i>Anycast</span>
          <span><i class="hollow"></i>Country only</span>
          <span v-if="unplacedHops">{{ unplacedHops }} {{ unplacedHops === 1 ? 'hop' : 'hops' }} without a location</span>
        </div>
      </div>

      <div ref="listEl" class="side" role="list">
        <div v-if="!flows.length" class="muted none">No flows match these filters.</div>
        <div v-for="vf in flows" :key="vf.key" :data-key="vf.key" role="listitem" tabindex="0"
             class="row-item" :class="{ active: activeSet.has(vf.key), selected: selected === vf.key }"
             :style="{ '--c': vf.color }"
             @mouseenter="hover = [vf.key]" @mouseleave="hover = []"
             @focus="hover = [vf.key]" @blur="hover = []"
             @click="toggleRow(vf.key)" @keydown.enter.prevent="toggleRow(vf.key)">
          <div class="r1">
            <GroupIcon v-if="vf.flow.group" :groupKey="vf.flow.group" :size="18" />
            <span v-else class="nogroup"></span>
            <strong class="name">{{ vf.label }}</strong>
            <span class="rate">{{ fmtRate(vf.rate) }}</span>
          </div>
          <div class="r2 muted">
            <code>{{ vf.flow.iface }}</code>
            <template v-if="vf.flow.upstream && vf.flow.upstream !== vf.flow.iface"> · {{ vf.flow.upstream }}</template>
            <span class="conns">{{ vf.flow.conns }} conn</span>
          </div>
          <div class="r3" :title="(vf.flow.hosts ?? []).join('\n')">{{ hostsText(vf) }}</div>
          <div class="r4 muted" :title="pathSummary(vf)">{{ pathSummary(vf) }}</div>
          <div v-if="selected === vf.key" class="r5 muted">
            ↓ {{ fmtRate(vf.flow.rateDown) }} ({{ fmtBytes(vf.flow.bytesDown) }})
            · ↑ {{ fmtRate(vf.flow.rateUp) }} ({{ fmtBytes(vf.flow.bytesUp) }})
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.map-panel {
  /* Role palette, shared with the map and the hop card. */
  --map-you: var(--accent);
  --map-pool: var(--warn);
  --map-server: #b48cff;
  --map-exit: var(--success);
  --map-dest: var(--text);
  --map-ocean: var(--bg);
  --map-land: var(--panel-2);
  --map-border: var(--border);
  display: flex;
  flex-direction: column;
  overflow: hidden;
  min-height: 0;
}
.head { display: flex; align-items: center; flex-wrap: wrap; gap: 10px 14px; margin-bottom: 12px; }
.head h2 { margin: 0; }
.filters { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.filters select { max-width: 200px; padding: 4px 8px; font-size: 12px; }
.totals { margin-left: auto; display: flex; gap: 12px; font-size: 12px; font-variant-numeric: tabular-nums; }
.mock-pill {
  padding: 1px 8px;
  border-radius: 10px;
  font-size: 11px;
  color: var(--warn);
  border: 1px solid var(--warn);
}
.seg-group { display: flex; }
.seg {
  background: transparent;
  border: 1px solid var(--border);
  border-right: none;
  border-radius: 0;
  padding: 3px 10px;
  color: var(--text-dim);
  font-size: 11px;
}
.seg:first-child { border-top-left-radius: 6px; border-bottom-left-radius: 6px; }
.seg:last-child { border-right: 1px solid var(--border); border-top-right-radius: 6px; border-bottom-right-radius: 6px; }
.seg.active { color: var(--text); background: var(--panel); border-color: var(--accent); }

.banner {
  margin-bottom: 10px;
  padding: 8px 12px;
  border-radius: 8px;
  font-size: 12px;
  color: var(--warn);
  background: rgba(255, 200, 87, 0.1);
  border: 1px solid rgba(255, 200, 87, 0.35);
}

.body { flex: 1; min-height: 0; display: grid; grid-template-columns: minmax(0, 1fr) 320px; gap: 12px; }
@media (max-width: 860px) {
  .body { grid-template-columns: minmax(0, 1fr); grid-template-rows: minmax(260px, 1fr) minmax(160px, 40%); }
}
.map-col { display: flex; flex-direction: column; min-height: 0; gap: 6px; }
.map-col > :first-child { flex: 1; }
.map-skel { height: 100%; min-height: 260px; border-radius: 8px; }

.legend { display: flex; flex-wrap: wrap; gap: 4px 14px; font-size: 11px; }
.legend span { display: inline-flex; align-items: center; gap: 5px; }
.legend .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--role-c); }
.legend .dash { width: 16px; border-top: 2px dashed var(--text-dim); }
.legend .ring { width: 10px; height: 10px; border-radius: 50%; border: 1px dashed var(--text); }
.legend .hollow { width: 8px; height: 8px; border-radius: 50%; border: 1.5px dashed var(--text-dim); }
.r-you { --role-c: var(--map-you); }
.r-pool { --role-c: var(--map-pool); }
.r-server { --role-c: var(--map-server); }
.r-exit { --role-c: var(--map-exit); }
.r-dest { --role-c: var(--map-dest); }

.side {
  min-height: 0;
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.none { padding: 12px; font-size: 12px; }
.row-item {
  padding: 8px 10px;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-left: 3px solid var(--c);
  border-radius: 6px;
  cursor: pointer;
  display: flex;
  flex-direction: column;
  gap: 3px;
  font-size: 12px;
  outline: none;
}
.row-item.active { background: var(--panel); border-color: var(--c); }
.row-item.selected { box-shadow: 0 0 0 1px var(--c); }
.row-item:focus-visible { box-shadow: 0 0 0 2px var(--accent); }
.r1 { display: flex; align-items: center; gap: 8px; min-width: 0; }
.name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.rate { font-variant-numeric: tabular-nums; flex: none; }
.r2 { display: flex; gap: 4px; align-items: baseline; font-size: 11px; min-width: 0; }
.r2 code { font-size: 11px; }
.conns { margin-left: auto; flex: none; }
.r3, .r4 { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.r4, .r5 { font-size: 11px; }
.nogroup { width: 18px; height: 18px; border-radius: 5px; background: var(--c); opacity: 0.6; flex: none; }

.empty {
  flex: 1;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  text-align: center;
  max-width: 460px;
  margin: 0 auto;
}
</style>
