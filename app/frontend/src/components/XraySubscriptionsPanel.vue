<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import {
  ListXraySubs, AddXraySub, UpdateXraySub, DeleteXraySub,
  SetXraySubEnabled, RefreshXraySub, XraySubNodes, SetXraySubNodeDisabled,
  XrayObservatory,
} from '../../wailsjs/go/main/App';

// Subscriptions are URL-fetched pools of nodes. They are consumed only
// through a master entry's Dialer (see DialerPicker) — never a direct rule
// target. This panel manages the URLs + their node pools.

type Sub = {
  id: number; name: string; url: string; userAgent: string;
  intervalSec: number; nodeCap: number; enabled: boolean;
  lastFetched: string; lastError: string; nodeCount: number; activeCount: number;
};
type Node = {
  fingerprint: string; name: string; active: boolean; disabled: boolean; latencyMs: number;
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
const obs = ref<{ winners: Set<string> }>({ winners: new Set() });
let obsTimer: number | undefined;

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
  return isNaN(d.getTime()) ? iso : d.toLocaleString();
}

async function refresh() {
  try {
    subs.value = ((await ListXraySubs()) || []) as unknown as Sub[];
    error.value = '';
    // Reload node tables for any expanded subs.
    for (const id of expanded.value) await loadNodes(id);
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function loadNodes(id: number) {
  try {
    nodesBySub.value[id] = ((await XraySubNodes(id)) || []) as unknown as Node[];
  } catch (e: any) {
    error.value = e?.message || String(e);
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
  busy.value = true;
  try {
    const r = await RefreshXraySub(s.id);
    if (r && !r.ok && r.message) error.value = r.message;
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}
async function refreshAll() {
  busy.value = true;
  try {
    await RefreshXraySub(0);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function toggleNode(subId: number, n: Node) {
  try {
    await SetXraySubNodeDisabled(subId, n.fingerprint, !n.disabled);
    await loadNodes(subId);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

// ---- Observatory (best-effort live latency + winner) ----

async function pollObs() {
  try {
    const r = await XrayObservatory();
    obs.value = { winners: new Set((r?.winners as string[]) || []) };
  } catch {
    obs.value = { winners: new Set() };
  }
}

function nodeLatency(n: Node): string {
  return n.latencyMs >= 0 ? `${n.latencyMs} ms` : '—';
}
function isWinner(n: Node): boolean {
  return obs.value.winners.has(n.fingerprint);
}

onMounted(() => {
  refresh();
  pollObs();
  obsTimer = window.setInterval(pollObs, 5000);
});
onUnmounted(() => {
  if (obsTimer) window.clearInterval(obsTimer);
});

defineExpose({ refresh });
</script>

<template>
  <div class="col" style="gap: 10px">
    <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap">
      <button v-if="!draft.open" @click="openDraft" :disabled="busy">+ Add subscription</button>
      <div style="flex: 1"></div>
      <button @click="refreshAll" :disabled="busy || subs.length === 0">Refresh all</button>
    </div>

    <div v-if="error" class="error" style="margin: 0">{{ error }}</div>

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

    <div v-if="subs.length === 0 && !draft.open" class="col"
         style="padding: 24px; background: var(--panel); border: 1px dashed var(--border); border-radius: 8px; align-items: center; gap: 6px">
      <span style="font-size: 13px">No subscriptions yet.</span>
      <span class="muted" style="font-size: 12px">Add a base64 share-link subscription URL; its nodes feed a master entry's Dialer.</span>
    </div>

    <!-- Subscription rows -->
    <div v-for="s in subs" :key="s.id"
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
          <button @click="refreshOne(s)" :disabled="busy">Refresh</button>
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

      <!-- Node table -->
      <div v-if="expanded.has(s.id)" class="col" style="gap: 4px; margin-top: 4px">
        <div v-if="(nodesBySub[s.id]?.length || 0) === 0" class="muted" style="font-size: 12px; padding: 6px 0">
          No nodes — refresh to fetch.
        </div>
        <div v-for="n in nodesBySub[s.id]" :key="n.fingerprint"
             class="row" style="justify-content: space-between; align-items: center; gap: 8px; padding: 5px 8px; background: var(--panel-2); border-radius: 6px">
          <div class="row" style="gap: 8px; align-items: center; min-width: 0; flex-wrap: wrap">
            <span v-if="isWinner(n)" title="current fastest" style="color: var(--success)">★</span>
            <span style="font-size: 12px">{{ n.name }}</span>
            <span v-if="n.disabled" class="tag tag-off" style="font-size: 10px">disabled</span>
            <span v-else-if="n.active" class="tag tag-route" style="font-size: 10px">active</span>
            <span v-else class="tag" style="font-size: 10px; background: rgba(141,141,160,0.15); color: var(--text-dim)">idle</span>
          </div>
          <div class="row" style="gap: 8px; align-items: center">
            <span class="muted" style="font-size: 11px; font-variant-numeric: tabular-nums">{{ nodeLatency(n) }}</span>
            <button @click="toggleNode(s.id, n)" :disabled="busy">{{ n.disabled ? 'Enable' : 'Disable' }}</button>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
