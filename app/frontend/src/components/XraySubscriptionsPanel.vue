<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import { startPoll } from '../poll';
import { useXrayActive, pickText, PICK_NOTE } from '../xrayActive';
import type { ipc } from '../../wailsjs/go/models';
import { fmtDateTime, fmtDate } from '../timezone';
import {
  ListXraySubs, AddXraySub, UpdateXraySub, DeleteXraySub,
  SetXraySubEnabled, RefreshXraySub, XraySubNodes, SetXraySubNodeDisabled,
  ImportXraySubNode, XrayObservatory, SetXraySubStrategy, SetXraySubNodePinned,
} from '../../wailsjs/go/main/App';

// Subscriptions are URL-fetched pools of nodes. They are consumed only
// through a master entry's Dialer (see DialerPicker) — never a direct rule
// target. This panel manages the URLs + their node pools.

type Sub = {
  id: number; name: string; url: string; userAgent: string;
  intervalSec: number; nodeCap: number; enabled: boolean;
  lastFetched: string; lastError: string; nodeCount: number; activeCount: number;
  usageUpload: number; usageDownload: number; usageTotal: number; usageExpire: number;
  strategy: string; pinnedCount: number;
};
type Node = {
  fingerprint: string; name: string; active: boolean; disabled: boolean; pinned: boolean; latencyMs: number;
  // Name of the standalone xray entry already promoted out of this node,
  // '' if there is none.
  importedAs: string;
};

const emit = defineEmits<{ (e: 'changed'): void }>();

const subs = ref<Sub[]>([]);
const error = ref('');
const busy = ref(false);

const draft = ref<{ open: boolean; name: string; url: string; userAgent: string; intervalHours: string; nodeCap: string; enabled: boolean }>({
  open: false, name: '', url: '', userAgent: '', intervalHours: '', nodeCap: '', enabled: true,
});
type EditState = { id: number } & typeof draft.value;
const editing = ref<EditState | null>(null);

const expanded = ref<Set<number>>(new Set());
const nodesBySub = ref<Record<number, Node[]>>({});
const pendingDelete = ref<number | null>(null);
let pendingDeleteTimer: number | undefined;

// Live winner set: fingerprints the daemon parsed from `xray api bi`
// (the fastest node each master dialer is routing through). Per-node RTT is
// not exposed by xray's CLI, so we surface the ★ winner only; the latency
// column shows any stored probe value.
type NodePing = { latencyMs: number; down: boolean; parked: boolean };
const obs = ref<{ winners: Set<string>; nodes: Record<string, NodePing> }>({ winners: new Set(), nodes: {} });
let stopObsPoll: (() => void) | undefined;

function hoursToSec(h: string): number {
  const n = parseFloat(h);
  return isFinite(n) && n > 0 ? Math.round(n * 3600) : 0;
}
function secToHours(s: number): string {
  return s > 0 ? String(+(s / 3600).toFixed(2)) : '';
}
function fmtTime(iso: string): string {
  if (!iso) return 'never';
  const d = new Date(iso);
  return isNaN(d.getTime()) ? iso : fmtDateTime(d);
}

// ---- Data-quota display (Subscription-Userinfo header, v2rayN-style) ----

// fmtBytes renders a byte count in binary units (GiB is what providers and
// v2rayN report). One decimal for GB and up, whole numbers below.
function fmtBytes(n: number): string {
  if (!isFinite(n) || n <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i >= 3 ? 1 : 0)} ${units[i]}`;
}
function usageUsed(s: Sub): number { return (s.usageUpload || 0) + (s.usageDownload || 0); }
// A subscription reports quota only when at least one counter is non-zero.
function hasUsage(s: Sub): boolean {
  return usageUsed(s) > 0 || (s.usageTotal || 0) > 0 || (s.usageExpire || 0) > 0;
}
function usagePct(s: Sub): number {
  if (!s.usageTotal || s.usageTotal <= 0) return 0;
  return Math.min(100, Math.round((usageUsed(s) / s.usageTotal) * 100));
}
function usageRemaining(s: Sub): number {
  return Math.max(0, (s.usageTotal || 0) - usageUsed(s));
}
function fmtExpire(unix: number): string {
  if (!unix || unix <= 0) return '';
  const d = new Date(unix * 1000);
  return isNaN(d.getTime()) ? '' : fmtDate(d);
}

// loaded: first list landed (until then, skeleton cards rather than "No
// subscriptions yet"). nodesLoading: subs whose node table is being fetched
// for the first time. refreshing: the subscription(s) being re-fetched from
// their provider — a slow network call, so its button says so.
const loaded = ref(false);
const nodesLoading = ref<Set<number>>(new Set());
const refreshing = ref<number | 'all' | null>(null);

async function refresh() {
  try {
    subs.value = ((await ListXraySubs()) || []) as unknown as Sub[];
    error.value = '';
    // Reload node tables for any expanded subs, together.
    await Promise.all([...expanded.value].map((id) => loadNodes(id)));
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
  loaded.value = true;
}

async function loadNodes(id: number) {
  const first = !nodesBySub.value[id];
  if (first) nodesLoading.value = new Set(nodesLoading.value).add(id);
  try {
    nodesBySub.value[id] = ((await XraySubNodes(id)) || []) as unknown as Node[];
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    if (first) {
      const next = new Set(nodesLoading.value);
      next.delete(id);
      nodesLoading.value = next;
    }
  }
}

function toggleExpand(id: number) {
  if (expanded.value.has(id)) {
    expanded.value.delete(id);
  } else {
    expanded.value.add(id);
    loadNodes(id);
  }
  expanded.value = new Set(expanded.value);
}

// ---- CRUD ----

function openDraft() {
  draft.value = { open: true, name: '', url: '', userAgent: '', intervalHours: '', nodeCap: '', enabled: true };
}
function cancelDraft() { draft.value.open = false; }

const draftValid = () => /^[a-z0-9_-]+$/.test(draft.value.name.trim().toLowerCase()) && /^https?:\/\//.test(draft.value.url.trim());

async function submitDraft() {
  if (!draftValid() || busy.value) return;
  busy.value = true;
  try {
    await AddXraySub(
      draft.value.name.trim().toLowerCase(), draft.value.url.trim(), draft.value.userAgent.trim(),
      hoursToSec(draft.value.intervalHours), parseInt(draft.value.nodeCap) || 0, draft.value.enabled,
    );
    draft.value.open = false;
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

function beginEdit(s: Sub) {
  editing.value = {
    id: s.id, open: true, name: s.name, url: s.url, userAgent: s.userAgent,
    intervalHours: secToHours(s.intervalSec), nodeCap: s.nodeCap ? String(s.nodeCap) : '', enabled: s.enabled,
  };
}
function cancelEdit() { editing.value = null; }

async function saveEdit() {
  const e = editing.value;
  if (!e) return;
  busy.value = true;
  try {
    await UpdateXraySub(
      e.id, e.name.trim().toLowerCase(), e.url.trim(), e.userAgent.trim(),
      hoursToSec(e.intervalHours), parseInt(e.nodeCap) || 0, e.enabled,
    );
    editing.value = null;
    await refresh();
    emit('changed');
  } catch (err: any) {
    error.value = err?.message || String(err);
  } finally {
    busy.value = false;
  }
}

async function toggleEnabled(s: Sub) {
  busy.value = true;
  try {
    await SetXraySubEnabled(s.id, !s.enabled);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

function askDelete(s: Sub) {
  if (pendingDelete.value === s.id) { confirmDelete(s); return; }
  pendingDelete.value = s.id;
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDeleteTimer = window.setTimeout(() => { pendingDelete.value = null; }, 3000);
}
async function confirmDelete(s: Sub) {
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDelete.value = null;
  busy.value = true;
  try {
    await DeleteXraySub(s.id);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function refreshOne(s: Sub) {
  if (busy.value) return;
  busy.value = true;
  refreshing.value = s.id;
  try {
    const r = await RefreshXraySub(s.id);
    if (r && !r.ok && r.message) error.value = r.message;
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
    refreshing.value = null;
  }
}
async function refreshAll() {
  if (busy.value) return;
  busy.value = true;
  refreshing.value = 'all';
  try {
    await RefreshXraySub(0);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
    refreshing.value = null;
  }
}

// Switch strategy: how pools drawing on this subscription move between its
// nodes. Applied live by the daemon (no xray restart).
const STRATEGIES: { value: string; label: string; help: string }[] = [
  { value: 'auto', label: 'Auto', help: 'Runs stable, and switches a pool to agile by itself while its nodes come and go in waves (back to stable after 30 min calm).' },
  { value: 'stable', label: 'Stable', help: 'For nodes that stay up or down for long stretches: ranks nodes over time, keeps a sticky pair, parks nodes that stay dead.' },
  { value: 'agile', label: 'Agile', help: 'For nodes that work for a minute and then drop: spreads over every node answering right now (up to 4), never parks, and closes stalled connections on a node that died so apps reconnect.' },
  { value: 'manual', label: 'Manual', help: 'Routes only through the nodes you pin below — never switches away from them, even if they all fail.' },
];

function strategyHelp(v: string): string {
  return STRATEGIES.find(x => x.value === v)?.help ?? '';
}

async function setStrategy(s: Sub, v: string) {
  if (v === s.strategy) return;
  busy.value = true;
  try {
    await SetXraySubStrategy(s.id, v);
    await refresh();
    if (v === 'manual' && !expanded.value.has(s.id)) toggleExpand(s.id);
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function togglePin(subId: number, n: Node) {
  if (busy.value) return;
  busy.value = true;
  try {
    await SetXraySubNodePinned(subId, n.fingerprint, !n.pinned);
    await loadNodes(subId);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function clearPins(s: Sub) {
  if (busy.value) return;
  busy.value = true;
  try {
    await SetXraySubNodePinned(s.id, '', false);
    await loadNodes(s.id);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function toggleNode(subId: number, n: Node) {
  if (busy.value) return;
  busy.value = true;
  try {
    await SetXraySubNodeDisabled(subId, n.fingerprint, !n.disabled);
    await loadNodes(subId);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

// Promote a pool node into a standalone xray entry so rules and outbound
// sets can target it by name. The daemon derives the entry name from the
// node's and dedupes it; re-clicking an already-promoted node hands back
// the existing entry rather than minting a second one, so the button is
// safe to hit twice.
//
// Reconciling restarts xray (a new entry means a new inbound), which drops
// live connections — the same cost as adding an entry by hand, and the
// reason this is a deliberate click rather than something automatic.
const importing = ref<string>('');
const imported = ref<{ node: string; entry: string } | null>(null);

async function importNode(subId: number, n: Node) {
  if (n.importedAs) return;
  importing.value = n.fingerprint;
  error.value = '';
  try {
    const entry = await ImportXraySubNode(subId, n.fingerprint, '');
    imported.value = { node: n.name, entry: (entry as any)?.name || '' };
    window.setTimeout(() => { imported.value = null; }, 6000);
    await loadNodes(subId);
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    importing.value = '';
  }
}

// ---- Observatory (best-effort live latency + winner) ----

async function pollObs() {
  try {
    const r = await XrayObservatory();
    obs.value = {
      winners: new Set((r?.winners as string[]) || []),
      nodes: (r?.nodes as Record<string, NodePing>) || {},
    };
  } catch {
    obs.value = { winners: new Set(), nodes: {} };
  }
}

// Live ping from xray's burst observatory, which health-pings every pool
// member every few seconds. Only nodes in a pool are pinged.
function nodePing(n: Node): { text: string; color: string; title: string } {
  const p = obs.value.nodes[n.fingerprint];
  if (!p) {
    return n.disabled
      ? { text: '', color: '', title: '' }
      : { text: '—', color: 'var(--text-dim)', title: 'Not pinged — only active nodes in a pool are pinged' };
  }
  if (p.parked) {
    return { text: 'parked', color: 'var(--danger)', title: 'Down for a while — left out of the pool and retried later' };
  }
  if (p.latencyMs >= 0) {
    const color = p.latencyMs < 300 ? 'var(--success)' : p.latencyMs < 800 ? 'var(--warn)' : 'var(--danger)';
    return { text: `${p.latencyMs} ms`, color, title: 'Last health ping through this node' };
  }
  if (p.down) {
    return { text: 'down', color: 'var(--danger)', title: 'Every recent health ping failed' };
  }
  return { text: '…', color: 'var(--text-dim)', title: 'Waiting for the first health ping' };
}
function isWinner(n: Node): boolean {
  return obs.value.winners.has(n.fingerprint);
}

// Live picks from xray's access log: which nodes the masters' balancers
// handed connections to in the last minute. A node in several pools shows
// its busiest one.
const { active: xrayActive } = useXrayActive(3000);
type NodePick = { m: ipc.XrayActiveMemberDTO; masters: string[] };
const nodePicks = computed<Record<string, NodePick>>(() => {
  const out: Record<string, NodePick> = {};
  for (const p of xrayActive.value?.pools ?? []) {
    for (const m of p.members) {
      const cur = out[m.key];
      if (!cur || m.recent > cur.m.recent || (m.recent === cur.m.recent && m.picks > cur.m.picks)) {
        out[m.key] = { m, masters: p.masters };
      }
    }
  }
  return out;
});
function nodePickText(n: Node): string {
  const p = nodePicks.value[n.fingerprint];
  return p ? pickText(p.m, xrayActive.value?.recentSec ?? 30) : '';
}
function nodePickTitle(n: Node): string {
  const p = nodePicks.value[n.fingerprint];
  if (!p) return '';
  const r = xrayActive.value;
  return `Picked for ${p.masters.join(', ')}: ${p.m.picks} connection${p.m.picks === 1 ? '' : 's'} in the last ${r?.windowSec ?? 60}s (${p.m.sharePct}% of the pool's).\n${PICK_NOTE}`;
}

onMounted(() => {
  refresh();
  pollObs();
  stopObsPoll = startPoll(pollObs, 5000);
});
onUnmounted(() => {
  stopObsPoll?.();
});

defineExpose({ refresh });
</script>

<template>
  <div class="col" style="gap: 10px">
    <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap" data-tour="xray-sub-add">
      <button v-if="!draft.open" @click="openDraft" :disabled="busy">+ Add subscription</button>
      <div style="flex: 1"></div>
      <button @click="refreshAll" :disabled="busy || subs.length === 0">
        <span v-if="refreshing === 'all'" class="spinner"></span>{{ refreshing === 'all' ? 'Refreshing all…' : 'Refresh all' }}
      </button>
    </div>

    <div v-if="error" class="error" style="margin: 0">{{ error }}</div>
    <div v-if="imported" class="muted" style="margin: 0; font-size: 12px">
      Added <strong>{{ imported.node }}</strong> as xray entry
      <code>{{ imported.entry }}</code> — target it in a rule as
      <code>xray:{{ imported.entry }}</code>.
    </div>

    <!-- New / edit form -->
    <div v-if="draft.open || editing"
         class="col" style="gap: 8px; padding: 12px 14px; background: var(--panel); border: 1px dashed var(--accent); border-radius: 8px">
      <strong style="font-size: 12px; color: var(--text-dim); text-transform: uppercase; letter-spacing: 0.5px">
        {{ editing ? 'Edit subscription' : 'New subscription' }}
      </strong>
      <template v-for="form in [editing || draft]" :key="editing ? 'edit' : 'draft'">
        <div class="row" style="gap: 8px; flex-wrap: wrap; align-items: center">
          <input v-model="form.name" placeholder="name (a-z 0-9 _ -)" style="width: 180px" />
          <input v-model="form.url" placeholder="https://…/subscription" style="flex: 1; min-width: 240px" />
        </div>
        <div class="row" style="gap: 8px; flex-wrap: wrap; align-items: center">
          <input v-model="form.userAgent" placeholder="User-Agent (blank = v2rayN)" style="width: 220px" />
          <input v-model="form.intervalHours" placeholder="refresh h (blank = 12)" style="width: 150px" />
          <input v-model="form.nodeCap" placeholder="active cap (blank = 30)" style="width: 150px" />
          <label class="row" style="gap: 6px; align-items: center; font-size: 12px">
            <input type="checkbox" v-model="form.enabled" /> Enabled
          </label>
          <div style="flex: 1"></div>
          <button v-if="editing" @click="cancelEdit" :disabled="busy">Cancel</button>
          <button v-else @click="cancelDraft" :disabled="busy">Cancel</button>
          <button class="primary" @click="editing ? saveEdit() : submitDraft()" :disabled="busy">
            {{ editing ? 'Save' : 'Add' }}
          </button>
        </div>
      </template>
    </div>

    <div v-if="!loaded" class="skeleton-stack">
      <span v-for="i in 2" :key="i" class="skeleton" style="height: 56px"></span>
    </div>
    <div v-else-if="subs.length === 0 && !draft.open" class="col"
         style="padding: 24px; background: var(--panel); border: 1px dashed var(--border); border-radius: 8px; align-items: center; gap: 6px">
      <span style="font-size: 13px">No subscriptions yet.</span>
      <span class="muted" style="font-size: 12px">Add a base64 share-link subscription URL; its nodes feed a master entry's Dialer.</span>
    </div>

    <!-- Subscription rows -->
    <div v-for="s in subs" :key="s.id" data-tour="xray-sub"
         class="col" style="gap: 8px; padding: 12px 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
      <div class="row" style="justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap">
        <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap; min-width: 0">
          <strong style="font-size: 13px">{{ s.name }}</strong>
          <span class="tag" :class="s.enabled ? 'tag-route' : 'tag-off'" style="font-size: 11px">
            {{ s.enabled ? 'enabled' : 'disabled' }}
          </span>
          <span class="tag" style="font-size: 11px; background: var(--panel-2); color: var(--text-dim)">
            {{ s.activeCount }} / {{ s.nodeCount }} active
          </span>
          <span v-if="s.lastError" class="tag tag-block" style="font-size: 11px" :title="s.lastError">fetch error</span>
        </div>
        <div class="row" style="gap: 6px">
          <button @click="toggleExpand(s.id)" :disabled="busy">{{ expanded.has(s.id) ? 'Hide nodes' : 'Nodes' }}</button>
          <button @click="refreshOne(s)" :disabled="busy">
            <span v-if="refreshing === s.id" class="spinner"></span>{{ refreshing === s.id ? 'Refreshing…' : 'Refresh' }}
          </button>
          <button @click="toggleEnabled(s)" :disabled="busy">{{ s.enabled ? 'Disable' : 'Enable' }}</button>
          <button @click="beginEdit(s)" :disabled="busy">Edit</button>
          <button @click="askDelete(s)" :disabled="busy" :class="pendingDelete === s.id ? 'danger' : ''">
            {{ pendingDelete === s.id ? 'Confirm?' : 'Delete' }}
          </button>
        </div>
      </div>
      <div class="row" style="gap: 10px; flex-wrap: wrap; font-size: 11px; color: var(--text-dim)">
        <code style="font-size: 11px">{{ s.url }}</code>
        <span>·</span>
        <span>fetched {{ fmtTime(s.lastFetched) }}</span>
      </div>
      <div v-if="s.lastError" class="muted" style="font-size: 11px; color: var(--danger)">✗ {{ s.lastError }}</div>

      <!-- Switch strategy -->
      <div data-tour="xray-strategy" class="row" style="gap: 8px; align-items: center; flex-wrap: wrap; font-size: 12px">
        <span class="muted">Switch strategy</span>
        <div class="row strategy-seg" style="gap: 0">
          <button v-for="o in STRATEGIES" :key="o.value" class="seg" :class="{ active: s.strategy === o.value }"
                  :title="o.help" :disabled="busy" @click="setStrategy(s, o.value)">{{ o.label }}</button>
        </div>
        <span class="muted" style="font-size: 11px; flex: 1 1 260px">{{ strategyHelp(s.strategy) }}</span>
      </div>
      <div v-if="s.strategy === 'manual'" class="row" style="gap: 8px; align-items: center; font-size: 11px">
        <span v-if="s.pinnedCount === 0" style="color: var(--warn)">
          No node pinned yet — open Nodes and pin one or more. Until then the pool runs stable.
        </span>
        <span v-else class="muted">{{ s.pinnedCount }} node{{ s.pinnedCount === 1 ? '' : 's' }} pinned</span>
        <button v-if="s.pinnedCount > 0" @click="clearPins(s)" :disabled="busy">Unpin all</button>
      </div>

      <!-- Data quota (only when the provider reports Subscription-Userinfo) -->
      <div v-if="hasUsage(s)" class="col" style="gap: 4px; margin-top: 2px">
        <div class="row" style="justify-content: space-between; font-size: 11px; color: var(--text-dim); gap: 8px; flex-wrap: wrap">
          <span>
            <strong style="color: var(--text)">{{ fmtBytes(usageRemaining(s)) }}</strong> left
            <span v-if="s.usageTotal > 0" style="opacity: 0.8">
              · {{ fmtBytes(usageUsed(s)) }} / {{ fmtBytes(s.usageTotal) }} used
            </span>
            <span v-else style="opacity: 0.8">· {{ fmtBytes(usageUsed(s)) }} used (unlimited)</span>
          </span>
          <span v-if="s.usageExpire > 0">expires {{ fmtExpire(s.usageExpire) }}</span>
        </div>
        <div v-if="s.usageTotal > 0" class="usage-bar">
          <div class="usage-bar-fill" :class="{ 'usage-bar-hot': usagePct(s) >= 90 }"
               :style="{ width: usagePct(s) + '%' }"></div>
        </div>
      </div>

      <!-- Node table -->
      <div v-if="expanded.has(s.id)" class="col" style="gap: 4px; margin-top: 4px">
        <div v-if="nodesLoading.has(s.id)" class="skeleton-stack">
          <span v-for="i in 3" :key="i" class="skeleton" style="height: 20px"></span>
        </div>
        <div v-else-if="(nodesBySub[s.id]?.length || 0) === 0" class="muted" style="font-size: 12px; padding: 6px 0">
          No nodes — refresh to fetch.
        </div>
        <div v-for="n in nodesBySub[s.id]" :key="n.fingerprint"
             class="row" style="justify-content: space-between; align-items: center; gap: 8px; padding: 5px 8px; background: var(--panel-2); border-radius: 6px">
          <div class="row" style="gap: 8px; align-items: center; min-width: 0; flex-wrap: wrap">
            <span v-if="isWinner(n)" title="Carrying master traffic now — one of the nodes the balancer spreads connections over" style="color: var(--success)">★</span>
            <span style="font-size: 12px">{{ n.name }}</span>
            <span v-if="nodePicks[n.fingerprint]" class="tag live-pick" :title="nodePickTitle(n)">
              <i class="live-dot" :class="{ dim: !nodePicks[n.fingerprint].m.active }"></i>{{ nodePickText(n) }}
            </span>
            <span v-if="n.pinned" class="tag tag-route" style="font-size: 10px"
                  :title="s.strategy === 'manual' ? 'Pinned: this pool routes only through pinned nodes' : 'Pinned — takes effect when the strategy is Manual'">pinned</span>
            <span v-if="n.disabled" class="tag tag-off" style="font-size: 10px">disabled</span>
            <span v-else-if="n.active" class="tag tag-route" style="font-size: 10px">active</span>
            <span v-else class="tag" style="font-size: 10px; background: rgba(141,141,160,0.15); color: var(--text-dim)">idle</span>
            <span v-if="n.importedAs" class="tag tag-route" style="font-size: 10px"
                  :title="`Already an xray entry — target it in a rule as xray:${n.importedAs}`">
              xray:{{ n.importedAs }}
            </span>
          </div>
          <div class="row" style="gap: 8px; align-items: center">
            <span style="font-size: 11px; font-variant-numeric: tabular-nums"
                  :style="{ color: nodePing(n).color }" :title="nodePing(n).title">{{ nodePing(n).text }}</span>
            <button v-if="n.importedAs" disabled title="An xray entry already exists for this node">✓ Added</button>
            <button v-else @click="importNode(s.id, n)" :disabled="busy || importing === n.fingerprint"
                    title="Create a standalone xray entry from this node so rules and sets can target it. Restarts xray.">
              {{ importing === n.fingerprint ? 'Adding…' : '+ Add' }}
            </button>
            <button v-if="s.strategy === 'manual' || n.pinned" @click="togglePin(s.id, n)" :disabled="busy"
                    :title="n.pinned ? 'Stop routing through this node' : 'Route this pool through this node (Manual strategy)'">
              {{ n.pinned ? 'Unpin' : 'Pin' }}
            </button>
            <button @click="toggleNode(s.id, n)" :disabled="busy">{{ n.disabled ? 'Enable' : 'Disable' }}</button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.live-pick {
  font-size: 10px;
  background: var(--panel);
  color: var(--text-dim);
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-variant-numeric: tabular-nums;
}
.live-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--success);
  box-shadow: 0 0 0 2px rgba(95, 208, 122, 0.2);
  flex: none;
}
.live-dot.dim { background: var(--text-dim); box-shadow: none; opacity: 0.6; }
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
.seg.active { color: var(--text); background: var(--panel-2); border-color: var(--accent); }
.usage-bar {
  height: 5px;
  border-radius: 3px;
  background: var(--panel-2);
  overflow: hidden;
}
.usage-bar-fill {
  height: 100%;
  border-radius: 3px;
  background: var(--accent);
  transition: width 0.3s ease;
}
.usage-bar-fill.usage-bar-hot {
  background: var(--danger);
}
</style>
