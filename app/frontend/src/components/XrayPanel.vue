<script lang="ts" setup>
import { ref, computed, onMounted } from 'vue';
import {
  XrayStatus, ListXray, AddXray, UpdateXray, DeleteXray, SetXrayEnabled,
  XrayRouting, SetXrayRouting, ParseXrayLink, TestXray,
  ListXraySubs, ListProxies,
  ListXraySets, AddXraySet, UpdateXraySet, DeleteXraySet, SetXraySetEnabled,
} from '../../wailsjs/go/main/App';
import MonacoJsonEditor from './MonacoJsonEditor.vue';
import DialerPicker from './DialerPicker.vue';
import XraySubscriptionsPanel from './XraySubscriptionsPanel.vue';

// Local mirrors of the DTOs so the file compiles even before
// `make run-app` regenerates wailsjs/models.ts.
type XrayRow = {
  id: number;
  name: string;
  outbound: string;
  socksPort: number;
  enabled: boolean;
  dialer: string;
  createdAt: string;
  updatedAt: string;
};

type XrayStatusRow = {
  enabled: boolean;
  running: boolean;
  version: string;
  portRangeStart: number;
  portRangeEnd: number;
  lastExit: string;
  recentLogs: string[];
};

// A set bundles several outbounds under one name. A rule binds to the
// NAME ("xrayset:NAME"), so changing the membership here updates every
// rule that uses the set — no re-picking chips per rule, no drift.
type XraySetRow = {
  id: number;
  name: string;
  members: string[];
  enabled: boolean;
  missingMembers: string[];
  usableCount: number;
  ruleCount: number;
  interface: string;
};

type SubTab = 'outbounds' | 'sets' | 'subscriptions' | 'routing' | 'status';

const status = ref<XrayStatusRow | null>(null);
const entries = ref<XrayRow[]>([]);
// Names available to the Dialer picker (loaded alongside entries).
const subNames = ref<string[]>([]);
const proxyNames = ref<string[]>([]);
const entryNames = computed(() => entries.value.map((e) => e.name));
const error = ref<string>('');
const busy = ref<boolean>(false);
const testing = ref<boolean>(false);
// IDs of entries with a probe currently in flight. Lets each row show
// its own "testing…" state during a concurrent "Test all" run, instead
// of the whole table looking idle until everything finishes.
const testingIds = ref<Set<number>>(new Set());
const subTab = ref<SubTab>('outbounds');

const defaultOutbound = `{
  "protocol": "freedom",
  "settings": {}
}`;

const draft = ref<{ open: boolean; name: string; outbound: string; enabled: boolean; dialer: string }>({
  open: false, name: '', outbound: defaultOutbound, enabled: true, dialer: '',
});

type EditState = { id: number; name: string; outbound: string; enabled: boolean; dialer: string };
const editing = ref<EditState | null>(null);

const pendingDelete = ref<number | null>(null);
let pendingDeleteTimer: number | undefined;

const testTarget = ref<string>('1.1.1.1:443');
const testResults = ref<Record<number, { ok: boolean; message: string; latencyMs: number; exitIp?: string; country?: string; region?: string; city?: string }>>({});
const revealedIPs = ref<Record<number, boolean>>({});

const sets = ref<XraySetRow[]>([]);
// One editor at a time: `null` = closed, `id: 0` = creating a new set.
const setDraft = ref<{ id: number; name: string; members: string[]; enabled: boolean } | null>(null);
const pendingSetDelete = ref<number | null>(null);
let pendingSetDeleteTimer: number | undefined;

const linkDialog = ref<{ open: boolean; link: string }>({ open: false, link: '' });

const routing = ref<{ raw: string; dirty: boolean; saving: boolean }>({
  raw: '', dirty: false, saving: false,
});

// ---------- Derived state ----------

const statusBadge = computed(() => {
  if (!status.value) return { label: 'unknown', color: 'var(--text-dim)' };
  if (!status.value.enabled) return { label: 'binary missing', color: 'var(--warn)' };
  if (status.value.running) return { label: 'running', color: 'var(--success)' };
  return { label: 'not running', color: 'var(--danger)' };
});

const enabledCount = computed(() => entries.value.filter((e) => e.enabled).length);

const draftIsValid = computed(() => {
  const d = draft.value;
  if (!d.name.trim() || !/^[a-z0-9_-]+$/.test(d.name.trim())) return false;
  return looksLikeOutbound(d.outbound);
});

const editingIsValid = computed(() => {
  const e = editing.value;
  if (!e || !e.name.trim() || !/^[a-z0-9_-]+$/.test(e.name.trim())) return false;
  return looksLikeOutbound(e.outbound);
});

function looksLikeOutbound(raw: string): boolean {
  try {
    const v = JSON.parse(raw);
    return v && typeof v === 'object' && !Array.isArray(v) && typeof v.protocol === 'string' && v.protocol.trim().length > 0;
  } catch {
    return false;
  }
}

// ---------- Loading ----------

async function refresh() {
  try {
    status.value = (await XrayStatus()) as unknown as XrayStatusRow;
    entries.value = ((await ListXray()) || []) as unknown as XrayRow[];
    // Names for the Dialer picker. Proxy list excludes the hidden internal
    // rows the supervisor mints (names start with "_").
    try {
      subNames.value = (((await ListXraySubs()) || []) as any[]).map((s) => s.name);
      proxyNames.value = (((await ListProxies()) || []) as any[]).map((p) => p.name).filter((n: string) => !n.startsWith('_'));
    } catch { /* non-fatal for the picker */ }
    try {
      sets.value = ((await ListXraySets()) || []) as unknown as XraySetRow[];
    } catch { /* non-fatal — the outbounds list still renders */ }
    if (!routing.value.dirty) {
      const r = await XrayRouting();
      routing.value.raw = r?.rules || '';
    }
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

// ---------- Outbound CRUD ----------

function openDraft() {
  draft.value = { open: true, name: '', outbound: defaultOutbound, enabled: true, dialer: '' };
}

function cancelDraft() {
  draft.value.open = false;
}

async function submitDraft() {
  if (!draftIsValid.value || busy.value) return;
  busy.value = true;
  try {
    await AddXray(draft.value.name.trim().toLowerCase(), draft.value.outbound, draft.value.enabled, draft.value.dialer.trim());
    draft.value.open = false;
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

function beginEdit(row: XrayRow) {
  editing.value = { id: row.id, name: row.name, outbound: row.outbound, enabled: row.enabled, dialer: row.dialer || '' };
}

function cancelEdit() {
  editing.value = null;
}

async function saveEdit() {
  const e = editing.value;
  if (!e || !editingIsValid.value || busy.value) return;
  busy.value = true;
  try {
    await UpdateXray(e.id, e.name.trim().toLowerCase(), e.outbound, e.enabled, e.dialer.trim());
    editing.value = null;
    await refresh();
  } catch (err: any) {
    error.value = err?.message || String(err);
  } finally {
    busy.value = false;
  }
}

function askDelete(row: XrayRow) {
  if (pendingDelete.value === row.id) {
    confirmDelete(row);
    return;
  }
  pendingDelete.value = row.id;
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDeleteTimer = window.setTimeout(() => { pendingDelete.value = null; }, 3000);
}

async function confirmDelete(row: XrayRow) {
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDelete.value = null;
  busy.value = true;
  try {
    await DeleteXray(row.id);
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function toggleEnabled(row: XrayRow) {
  busy.value = true;
  try {
    await SetXrayEnabled(row.id, !row.enabled);
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

// ---------- Outbound sets ----------

// Everything a set can be built from, as typed refs. Proxies are
// included because a set is just a fallback chain and the daemon can
// dial either kind; subscriptions are NOT — their nodes have no local
// inbound of their own (they're only reachable as a master's dialer).
const setMemberOptions = computed<string[]>(() => [
  ...entries.value.map((e) => `xray:${e.name}`),
  ...proxyNames.value.map((n) => `proxy:${n}`),
]);

const setDraftIsValid = computed(() => {
  const d = setDraft.value;
  if (!d) return false;
  if (!d.name.trim() || !/^[a-z0-9_-]+$/.test(d.name.trim())) return false;
  return d.members.filter(Boolean).length > 0;
});

function openSetDraft() {
  setDraft.value = { id: 0, name: '', members: [''], enabled: true };
}

function editSet(s: XraySetRow) {
  setDraft.value = { id: s.id, name: s.name, members: [...s.members], enabled: s.enabled };
}

function closeSetDraft() { setDraft.value = null; }

function addSetMemberRow() {
  setDraft.value?.members.push('');
}

function removeSetMemberRow(i: number) {
  setDraft.value?.members.splice(i, 1);
}

// Fallback order is meaningful — the daemon walks members top-down and
// uses the first that dials — so the editor lets the user reorder.
function moveSetMember(i: number, delta: number) {
  const d = setDraft.value;
  if (!d) return;
  const j = i + delta;
  if (j < 0 || j >= d.members.length) return;
  const [m] = d.members.splice(i, 1);
  d.members.splice(j, 0, m);
}

async function saveSet() {
  const d = setDraft.value;
  if (!d || !setDraftIsValid.value) return;
  busy.value = true;
  try {
    const members = d.members.filter(Boolean);
    if (d.id === 0) {
      await AddXraySet(d.name.trim(), members, d.enabled);
    } else {
      await UpdateXraySet(d.id, d.name.trim(), members, d.enabled);
    }
    setDraft.value = null;
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function toggleSet(s: XraySetRow) {
  busy.value = true;
  try {
    await SetXraySetEnabled(s.id, !s.enabled);
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

function askDeleteSet(s: XraySetRow) {
  if (pendingSetDelete.value === s.id) return;
  pendingSetDelete.value = s.id;
  if (pendingSetDeleteTimer) window.clearTimeout(pendingSetDeleteTimer);
  pendingSetDeleteTimer = window.setTimeout(() => { pendingSetDelete.value = null; }, 3000);
}

async function confirmDeleteSet(s: XraySetRow) {
  if (pendingSetDeleteTimer) window.clearTimeout(pendingSetDeleteTimer);
  pendingSetDelete.value = null;
  busy.value = true;
  try {
    await DeleteXraySet(s.id);
    await refresh();
  } catch (e: any) {
    // The daemon refuses while rules still bind the set — surface that
    // verbatim, it names the blast radius.
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

// ---------- Testing ----------

async function runTest(row: XrayRow) {
  // Mark this row in-flight (reassign the Set so Vue picks up the change).
  testingIds.value = new Set(testingIds.value).add(row.id);
  try {
    const r = await TestXray(row.id, testTarget.value.trim());
    testResults.value = {
      ...testResults.value,
      [row.id]: { ok: r.ok, message: r.message, latencyMs: r.latencyMs, exitIp: r.exitIp, country: r.country, region: r.region, city: r.city },
    };
  } catch (e: any) {
    testResults.value = {
      ...testResults.value,
      [row.id]: { ok: false, message: e?.message || String(e), latencyMs: 0 },
    };
  } finally {
    const next = new Set(testingIds.value);
    next.delete(row.id);
    testingIds.value = next;
  }
}

// testAll tests every enabled entry CONCURRENTLY (with a small cap), so
// each row's result lands the moment its own probe returns instead of
// waiting through the others one-by-one. runTest writes each row's
// result independently, so the table fills in progressively. The cap
// keeps the daemon from being slammed and keeps per-entry latency
// numbers roughly independent. Disabled entries are skipped because the
// daemon refuses them anyway.
const TEST_ALL_CONCURRENCY = 4;
async function testAll() {
  if (testing.value) return;
  testing.value = true;
  try {
    const queue = entries.value.filter(r => r.enabled);
    let i = 0;
    const worker = async () => {
      while (i < queue.length) {
        await runTest(queue[i++]);
      }
    };
    await Promise.all(
      Array.from({ length: Math.min(TEST_ALL_CONCURRENCY, queue.length) }, worker),
    );
  } finally {
    testing.value = false;
  }
}

// ---------- Country helpers ----------

// toFlagEmoji converts an ISO 3166-1 alpha-2 country code (e.g. "US")
// to its Unicode flag emoji (e.g. 🇺🇸) using Regional Indicator Symbols.
function toFlagEmoji(code: string): string {
  if (!code || code.length !== 2) return '';
  return [...code.toUpperCase()]
    .map(c => String.fromCodePoint(c.charCodeAt(0) + 0x1F1A5))
    .join('');
}

// maskIP hides the host-specific part of an IP address.
// IPv4: keeps first two octets  →  104.16.*.*
// IPv6: keeps first group only  →  2606:…
function maskIP(ip: string): string {
  if (!ip) return '';
  if (ip.includes(':')) {
    return ip.split(':')[0] + ':…';
  }
  const p = ip.split('.');
  return p.length === 4 ? `${p[0]}.${p[1]}.*.*` : ip;
}

function toggleIPReveal(id: number) {
  revealedIPs.value = { ...revealedIPs.value, [id]: !revealedIPs.value[id] };
}

// ---------- JSON helpers ----------

function formatOutbound(field: 'draft' | 'editing') {
  try {
    if (field === 'draft') {
      draft.value.outbound = JSON.stringify(JSON.parse(draft.value.outbound), null, 2);
    } else if (editing.value) {
      editing.value.outbound = JSON.stringify(JSON.parse(editing.value.outbound), null, 2);
    }
  } catch (e: any) {
    error.value = 'format: ' + (e?.message || String(e));
  }
}

function formatRouting() {
  try {
    const parsed = JSON.parse(routing.value.raw || '[]');
    routing.value.raw = JSON.stringify(parsed, null, 2);
    routing.value.dirty = true;
  } catch (e: any) {
    error.value = 'routing format: ' + (e?.message || String(e));
  }
}

// ---------- Routing rules ----------

function onRoutingInput(v: string) {
  routing.value.raw = v;
  routing.value.dirty = true;
}

async function saveRouting() {
  if (routing.value.saving) return;
  routing.value.saving = true;
  try {
    await SetXrayRouting(routing.value.raw);
    routing.value.dirty = false;
    await refresh();
  } catch (e: any) {
    error.value = 'routing: ' + (e?.message || String(e));
  } finally {
    routing.value.saving = false;
  }
}

function discardRouting() {
  routing.value.dirty = false;
  refresh();
}

// ---------- Link import ----------

function openImport() {
  linkDialog.value.open = true;
  linkDialog.value.link = '';
}

function cancelImport() {
  linkDialog.value.open = false;
}

async function applyImport() {
  if (!linkDialog.value.link.trim()) return;
  try {
    const r = await ParseXrayLink(linkDialog.value.link.trim());
    draft.value = { open: true, name: r.name, outbound: r.outbound, enabled: true, dialer: '' };
    linkDialog.value.open = false;
    linkDialog.value.link = '';
    subTab.value = 'outbounds';
  } catch (e: any) {
    error.value = 'import: ' + (e?.message || String(e));
  }
}

onMounted(refresh);
defineExpose({ refresh });
</script>

<template>
  <div class="panel col" style="gap: 12px">
    <!-- ============ Header (always visible) ============ -->
    <div class="col" style="gap: 8px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
      <div class="row" style="justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap">
        <div class="col" style="gap: 2px; min-width: 0">
          <strong style="font-size: 14px">Xray-core</strong>
          <code v-if="status?.version" style="font-size: 11px; color: var(--text-dim)">{{ status.version }}</code>
          <span v-else class="muted" style="font-size: 11px">version unknown</span>
        </div>
        <div class="row" style="gap: 8px; align-items: center">
          <span class="tag" :style="{
            backgroundColor: statusBadge.color === 'var(--success)' ? 'rgba(95,208,122,0.15)'
                            : statusBadge.color === 'var(--danger)' ? 'rgba(255,111,111,0.15)'
                            : statusBadge.color === 'var(--warn)' ? 'rgba(255,200,87,0.15)'
                            : 'rgba(141,141,160,0.15)',
            color: statusBadge.color,
            fontSize: '11px',
          }">● {{ statusBadge.label }}</span>
          <button @click="refresh" :disabled="busy">Refresh</button>
        </div>
      </div>
      <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap">
        <span class="muted" style="font-size: 11px">ports {{ status?.portRangeStart || '?' }}–{{ status?.portRangeEnd || '?' }}</span>
        <span class="muted" style="font-size: 11px">·</span>
        <span class="muted" style="font-size: 11px">{{ enabledCount }} / {{ entries.length }} enabled</span>
        <div style="flex: 1"></div>
        <span class="muted" style="font-size: 11px">Test target:</span>
        <input v-model="testTarget" placeholder="host:port" style="width: 160px" />
      </div>
      <div v-if="error" class="error" style="margin: 0">{{ error }}</div>
    </div>

    <!-- ============ Sub-tab nav ============ -->
    <div class="row" style="gap: 4px; padding: 0; border-bottom: 1px solid var(--border); margin: 0 -2px 4px">
      <button class="subtab" :class="{ active: subTab === 'outbounds' }" @click="subTab = 'outbounds'">
        Outbounds
        <span class="subtab-badge">{{ entries.length }}</span>
      </button>
      <button class="subtab" :class="{ active: subTab === 'sets' }" @click="subTab = 'sets'">
        Sets
        <span v-if="sets.length" class="subtab-badge">{{ sets.length }}</span>
      </button>
      <button class="subtab" :class="{ active: subTab === 'subscriptions' }" @click="subTab = 'subscriptions'">
        Subscriptions
        <span v-if="subNames.length" class="subtab-badge">{{ subNames.length }}</span>
      </button>
      <button class="subtab" :class="{ active: subTab === 'routing' }" @click="subTab = 'routing'">
        Routing
        <span v-if="routing.dirty" class="subtab-dot" style="background: var(--warn)" title="unsaved changes"></span>
      </button>
      <button class="subtab" :class="{ active: subTab === 'status' }" @click="subTab = 'status'">
        Status
        <span v-if="status?.enabled && !status.running" class="subtab-dot" style="background: var(--danger)" title="xray not running"></span>
      </button>
    </div>

    <!-- ============ Outbounds tab ============ -->
    <template v-if="subTab === 'outbounds'">
      <div class="row" style="gap: 8px; flex-wrap: wrap; align-items: center">
        <button v-if="!draft.open" @click="openDraft" :disabled="busy">+ Add outbound</button>
        <button @click="openImport" :disabled="busy">Import (URI or JSON)</button>
        <div style="flex: 1"></div>
        <button @click="testAll" :disabled="busy || testing || enabledCount === 0"
                :title="enabledCount === 0 ? 'no enabled entries to test' : ''">
          {{ testing ? 'Testing…' : `Test all (${enabledCount})` }}
        </button>
      </div>

      <div v-if="entries.length === 0 && !draft.open" class="col"
           style="padding: 24px; background: var(--panel); border: 1px dashed var(--border); border-radius: 8px; align-items: center; gap: 6px">
        <span style="font-size: 13px">No outbounds configured yet.</span>
        <span class="muted" style="font-size: 12px">Add one with the button above, or paste a share link to import.</span>
      </div>

      <div v-for="row in entries" :key="row.id"
           class="col" style="gap: 8px; padding: 12px 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
        <!-- View mode -->
        <template v-if="!editing || editing.id !== row.id">
          <div class="row" style="justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap">
            <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap; min-width: 0">
              <strong style="font-size: 13px">xray:{{ row.name }}</strong>
              <span class="tag" :class="row.enabled ? 'tag-route' : 'tag-off'" style="font-size: 11px">
                {{ row.enabled ? 'enabled' : 'disabled' }}
              </span>
              <span v-if="row.dialer" class="tag" style="font-size: 11px; background: rgba(110,168,255,0.15); color: var(--accent)"
                    :title="'dialer: ' + row.dialer">master · {{ row.dialer }}</span>
              <code style="font-size: 11px; color: var(--text-dim)">127.0.0.1:{{ row.socksPort }}</code>
              <span v-if="testingIds.has(row.id)" class="tag" style="font-size: 11px">testing…</span>
              <span v-else-if="testResults[row.id]" class="tag"
                    :class="testResults[row.id].ok ? 'tag-allow' : 'tag-block'"
                    style="font-size: 11px">
                {{ testResults[row.id].ok ? `${testResults[row.id].latencyMs} ms` : 'fail' }}
              </span>
              <span v-if="testResults[row.id]?.ok && testResults[row.id]?.country"
                    class="tag" style="font-size: 11px; background: var(--panel-2); color: var(--text-dim)">
                {{ toFlagEmoji(testResults[row.id].country!) }} {{ testResults[row.id].country }}
                <template v-if="testResults[row.id].city || testResults[row.id].region">
                  · {{ [testResults[row.id].city, testResults[row.id].region].filter(Boolean).join(', ') }}
                </template>
              </span>
              <span v-if="testResults[row.id]?.ok && testResults[row.id]?.exitIp"
                    class="tag"
                    style="font-size: 11px; background: var(--panel-2); color: var(--text-dim); cursor: pointer; font-family: monospace"
                    :title="revealedIPs[row.id] ? 'click to mask' : 'click to reveal full IP'"
                    @click.stop="toggleIPReveal(row.id)">
                {{ revealedIPs[row.id] ? testResults[row.id].exitIp : maskIP(testResults[row.id].exitIp!) }}
              </span>
            </div>
            <div class="row" style="gap: 6px">
              <button @click="runTest(row)" :disabled="busy || testing || testingIds.has(row.id) || !row.enabled">Test</button>
              <button @click="toggleEnabled(row)" :disabled="busy">{{ row.enabled ? 'Disable' : 'Enable' }}</button>
              <button @click="beginEdit(row)" :disabled="busy">Edit</button>
              <button @click="askDelete(row)" :disabled="busy"
                      :class="pendingDelete === row.id ? 'danger' : ''">
                {{ pendingDelete === row.id ? 'Confirm?' : 'Delete' }}
              </button>
            </div>
          </div>
          <div v-if="testResults[row.id] && !testResults[row.id].ok" class="muted"
               style="font-size: 11px; color: var(--danger)">
            ✗ {{ testResults[row.id].message }}
          </div>
          <details>
            <summary style="cursor: pointer; font-size: 11px; color: var(--text-dim)">show outbound JSON</summary>
            <pre style="margin: 6px 0 0; padding: 8px 10px; background: var(--panel-2); border: 1px solid var(--border); border-radius: 6px; font-size: 11px; max-height: 200px; overflow: auto">{{ row.outbound }}</pre>
          </details>
        </template>

        <!-- Edit mode -->
        <template v-else>
          <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap">
            <input v-model="editing.name" placeholder="name (a-z 0-9 _ -)" style="width: 200px" />
            <label class="row" style="gap: 6px; align-items: center; font-size: 12px">
              <input type="checkbox" v-model="editing.enabled" /> Enabled
            </label>
            <button @click="formatOutbound('editing')" :disabled="busy">Format JSON</button>
            <div style="flex: 1"></div>
            <button @click="cancelEdit" :disabled="busy">Cancel</button>
            <button class="primary" @click="saveEdit" :disabled="!editingIsValid || busy">Save</button>
          </div>
          <span class="muted" style="font-size: 11px">
            The <code>tag</code> field is auto-managed (forced to <code>out-{{ editing.name || 'NAME' }}</code> on save) — any value you set here is overwritten.
          </span>
          <DialerPicker v-model="editing.dialer" :xray-names="entryNames" :sub-names="subNames"
                        :proxy-names="proxyNames" :self-name="editing.name" />
          <MonacoJsonEditor v-model="editing.outbound" height="380px" />
        </template>
      </div>

      <!-- New-entry draft -->
      <div v-if="draft.open"
           class="col" style="gap: 8px; padding: 12px 14px; background: var(--panel); border: 1px dashed var(--accent); border-radius: 8px">
        <strong style="font-size: 12px; color: var(--text-dim); text-transform: uppercase; letter-spacing: 0.5px">New outbound</strong>
        <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap">
          <input v-model="draft.name" placeholder="name (a-z 0-9 _ -)" style="width: 200px" />
          <label class="row" style="gap: 6px; align-items: center; font-size: 12px">
            <input type="checkbox" v-model="draft.enabled" /> Enabled
          </label>
          <button @click="formatOutbound('draft')" :disabled="busy">Format JSON</button>
          <div style="flex: 1"></div>
          <button @click="cancelDraft" :disabled="busy">Cancel</button>
          <button class="primary" @click="submitDraft" :disabled="!draftIsValid || busy">Add</button>
        </div>
        <span class="muted" style="font-size: 11px">
          The <code>tag</code> field is auto-managed (forced to <code>out-{{ draft.name || 'NAME' }}</code> on save) — any value you set here is overwritten.
        </span>
        <DialerPicker v-model="draft.dialer" :xray-names="entryNames" :sub-names="subNames"
                      :proxy-names="proxyNames" :self-name="draft.name" />
        <MonacoJsonEditor v-model="draft.outbound" height="380px" />
      </div>

      <!-- Import dialog -->
      <div v-if="linkDialog.open"
           class="col" style="gap: 8px; padding: 12px 14px; background: var(--panel); border: 1px dashed var(--accent); border-radius: 8px">
        <strong style="font-size: 12px; color: var(--text-dim); text-transform: uppercase; letter-spacing: 0.5px">Import outbound</strong>
        <span class="muted" style="font-size: 11px; line-height: 1.5">
          Paste <strong>either</strong> a share URI (<code>vless://</code>, <code>vmess://</code>,
          <code>trojan://</code>, <code>ss://</code>; xhttp/splithttp transport supported)
          <strong>or</strong> a raw xray outbound JSON object copied from another panel —
          flat <code>settings.address</code>/<code>port</code>/<code>id</code> shapes are
          auto-reshaped into the canonical <code>vnext</code>/<code>servers</code> form.
        </span>
        <textarea v-model="linkDialog.link"
                  placeholder='vless://… OR { "protocol": "vless", "settings": { … }, "streamSettings": { … } }'
                  rows="6"
                  style="width: 100%; font-family: ui-monospace, monospace; font-size: 12px"></textarea>
        <div class="row" style="gap: 6px; justify-content: flex-end">
          <button @click="cancelImport" :disabled="busy">Cancel</button>
          <button class="primary" @click="applyImport" :disabled="!linkDialog.link.trim() || busy">Parse</button>
        </div>
      </div>
    </template>

    <!-- ============ Sets tab ============ -->
    <template v-else-if="subTab === 'sets'">
      <div class="col" style="gap: 10px">
        <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap">
          <button v-if="!setDraft" @click="openSetDraft" :disabled="busy">+ New set</button>
          <span class="muted" style="font-size: 11px; flex: 1">
            A set bundles outbounds under one name. Rules bind to
            <code style="font-size: 11px">xrayset:NAME</code>, so editing the set
            updates every rule that uses it.
          </span>
        </div>

        <!-- Editor (create + edit share it; id 0 means create) -->
        <div v-if="setDraft" class="col"
             style="gap: 10px; padding: 12px 14px; background: var(--panel); border: 1px solid var(--accent); border-radius: 8px">
          <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap">
            <strong style="font-size: 13px">{{ setDraft.id === 0 ? 'New set' : `Edit set: ${setDraft.name}` }}</strong>
            <div style="flex: 1"></div>
            <label class="toggle" :title="setDraft.enabled ? 'enabled' : 'disabled — bound rules stop resolving'">
              <input type="checkbox" v-model="setDraft.enabled" />
              <span class="track"></span>
            </label>
          </div>

          <div class="row" style="gap: 8px; align-items: center">
            <span class="muted" style="font-size: 11px; min-width: 60px">name:</span>
            <input v-model="setDraft.name" placeholder="iproute" style="flex: 1" />
            <code v-if="setDraft.name" style="font-size: 11px; color: var(--text-dim)">xrayset:{{ setDraft.name }}</code>
          </div>

          <div class="col" style="gap: 6px">
            <span class="muted" style="font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px">
              Members — tried in order, first that dials wins
            </span>
            <div v-for="(m, i) in setDraft.members" :key="i" class="row" style="gap: 6px; align-items: center">
              <span class="muted" style="font-size: 11px; width: 16px; text-align: right">{{ i + 1 }}</span>
              <select v-model="setDraft.members[i]" style="flex: 1">
                <option value="" disabled>— choose outbound —</option>
                <option v-for="opt in setMemberOptions" :key="opt" :value="opt">{{ opt }}</option>
                <!-- keep a stored ref visible even if its target is gone -->
                <option v-if="m && !setMemberOptions.includes(m)" :value="m">{{ m }} (missing)</option>
              </select>
              <button @click="moveSetMember(i, -1)" :disabled="i === 0" title="move up">↑</button>
              <button @click="moveSetMember(i, 1)" :disabled="i === setDraft.members.length - 1" title="move down">↓</button>
              <button @click="removeSetMemberRow(i)" title="remove">✕</button>
            </div>
            <div class="row" style="gap: 6px">
              <button @click="addSetMemberRow">+ Add member</button>
            </div>
          </div>

          <div class="row" style="gap: 6px; justify-content: flex-end">
            <button @click="closeSetDraft" :disabled="busy">Cancel</button>
            <button class="primary" @click="saveSet" :disabled="!setDraftIsValid || busy">
              {{ setDraft.id === 0 ? 'Create set' : 'Save' }}
            </button>
          </div>
        </div>

        <div v-if="sets.length === 0 && !setDraft" class="col"
             style="padding: 24px; background: var(--panel); border: 1px dashed var(--border); border-radius: 8px; align-items: center; gap: 6px">
          <span style="font-size: 13px">No sets yet.</span>
          <span class="muted" style="font-size: 12px">
            Group the outbounds you keep picking together, then bind rules to the set instead.
          </span>
        </div>

        <div v-for="s in sets" :key="s.id"
             class="col" style="gap: 8px; padding: 12px 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
          <div class="row" style="justify-content: space-between; align-items: center; gap: 8px; flex-wrap: wrap">
            <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap; min-width: 0">
              <strong style="font-size: 13px">xrayset:{{ s.name }}</strong>
              <span class="tag" :class="s.enabled ? 'tag-route' : 'tag-off'" style="font-size: 11px">
                {{ s.enabled ? 'enabled' : 'disabled' }}
              </span>
              <span class="tag" style="font-size: 11px"
                    :class="s.usableCount === 0 ? 'tag-block' : 'tag-allow'"
                    :title="s.usableCount === 0 ? 'no member can be dialled — bound rules return NXDOMAIN' : 'members currently usable'">
                {{ s.usableCount }}/{{ s.members.length }} usable
              </span>
              <span v-if="s.missingMembers.length" class="tag tag-block" style="font-size: 11px"
                    :title="'missing: ' + s.missingMembers.join(', ')">
                ⚠ {{ s.missingMembers.length }} missing
              </span>
              <span class="muted" style="font-size: 11px">
                used by {{ s.ruleCount }} rule{{ s.ruleCount === 1 ? '' : 's' }}
              </span>
            </div>
            <div class="row" style="gap: 6px">
              <label class="toggle" @click.prevent="toggleSet(s)">
                <input type="checkbox" :checked="s.enabled" />
                <span class="track"></span>
              </label>
              <button @click="editSet(s)" :disabled="busy">Edit</button>
              <button :class="pendingSetDelete === s.id ? 'danger primary' : 'danger'"
                      @click="pendingSetDelete === s.id ? confirmDeleteSet(s) : askDeleteSet(s)"
                      :disabled="busy">
                {{ pendingSetDelete === s.id ? 'Confirm?' : 'Delete' }}
              </button>
            </div>
          </div>
          <div class="row" style="gap: 4px; flex-wrap: wrap">
            <span v-for="(m, i) in s.members" :key="m"
                  class="row" style="gap: 4px; padding: 2px 6px; border: 1px solid var(--border); border-radius: 12px; font-size: 11px"
                  :class="{ 'not-running': s.missingMembers.includes(m) }"
                  :title="s.missingMembers.includes(m) ? 'target no longer exists' : ''">
              <span class="muted">{{ i + 1 }}</span>
              <span>{{ m }}</span>
            </span>
          </div>
          <code v-if="s.interface" style="font-size: 11px; color: var(--text-dim)">
            expands to {{ s.interface }}
          </code>
        </div>
      </div>
    </template>

    <!-- ============ Subscriptions tab ============ -->
    <template v-else-if="subTab === 'subscriptions'">
      <XraySubscriptionsPanel @changed="refresh" />
    </template>

    <!-- ============ Routing tab ============ -->
    <template v-else-if="subTab === 'routing'">
      <div class="col" style="gap: 10px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
        <div class="row" style="justify-content: space-between; align-items: flex-start; gap: 16px; flex-wrap: wrap">
          <div class="col" style="gap: 4px; flex: 1; min-width: 280px">
            <strong>Global routing rules</strong>
            <span class="muted" style="font-size: 12px; line-height: 1.5">
              JSON array, prepended to the per-entry inbound→outbound pairs (these match first).
              Geosite + geoip data are bundled with xray; reference them as
              <code>geosite:&lt;tag&gt;</code> / <code>geoip:&lt;code&gt;</code>.
            </span>
          </div>
          <div class="row" style="gap: 6px">
            <button @click="formatRouting" :disabled="routing.saving">Format</button>
            <button v-if="routing.dirty" @click="discardRouting" :disabled="routing.saving">Discard</button>
            <button class="primary" @click="saveRouting" :disabled="!routing.dirty || routing.saving">
              {{ routing.saving ? '…' : 'Save' }}
            </button>
          </div>
        </div>

        <div class="row" style="gap: 8px; flex-wrap: wrap">
          <span class="tag tag-allow" style="font-size: 11px">outboundTag: direct</span>
          <span class="muted" style="font-size: 11px">→ passthrough (freedom)</span>
          <span class="muted" style="font-size: 11px">·</span>
          <span class="tag tag-block" style="font-size: 11px">outboundTag: block</span>
          <span class="muted" style="font-size: 11px">→ blackhole (drop)</span>
        </div>

        <MonacoJsonEditor :model-value="routing.raw" @update:model-value="onRoutingInput" height="420px" />

        <details>
          <summary style="cursor: pointer; font-size: 11px; color: var(--text-dim)">examples</summary>
          <pre style="margin: 6px 0 0; padding: 8px 10px; background: var(--panel-2); border: 1px solid var(--border); border-radius: 6px; font-size: 11px; overflow: auto">[
  { "type": "field", "domain": ["geosite:ads-all"],  "outboundTag": "block"  },
  { "type": "field", "domain": ["geosite:cn"],       "outboundTag": "direct" },
  { "type": "field", "ip":     ["geoip:cn"],         "outboundTag": "direct" },
  { "type": "field", "ip":     ["geoip:private"],    "outboundTag": "block"  }
]</pre>
        </details>
      </div>
    </template>

    <!-- ============ Status tab ============ -->
    <template v-else-if="subTab === 'status'">
      <!-- Subprocess card -->
      <div class="col" style="gap: 10px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
        <div class="row" style="justify-content: space-between; align-items: center">
          <strong>Subprocess</strong>
          <span class="tag" :style="{
            backgroundColor: statusBadge.color === 'var(--success)' ? 'rgba(95,208,122,0.15)'
                            : statusBadge.color === 'var(--danger)' ? 'rgba(255,111,111,0.15)'
                            : 'rgba(255,200,87,0.15)',
            color: statusBadge.color,
            fontSize: '11px',
          }">● {{ statusBadge.label }}</span>
        </div>
        <div class="row" style="gap: 16px; flex-wrap: wrap; font-size: 12px">
          <div class="col" style="gap: 2px">
            <span class="muted" style="font-size: 10px; text-transform: uppercase; letter-spacing: 0.5px">Version</span>
            <code style="font-size: 12px">{{ status?.version || '—' }}</code>
          </div>
          <div class="col" style="gap: 2px">
            <span class="muted" style="font-size: 10px; text-transform: uppercase; letter-spacing: 0.5px">Listening on</span>
            <span>{{ enabledCount }} loopback SOCKS5 inbound{{ enabledCount === 1 ? '' : 's' }}, ports {{ status?.portRangeStart }}–{{ status?.portRangeEnd }}</span>
          </div>
          <div class="col" style="gap: 2px">
            <span class="muted" style="font-size: 10px; text-transform: uppercase; letter-spacing: 0.5px">Log file</span>
            <code style="font-size: 11px; color: var(--text-dim)">/usr/local/var/log/em-wall-xray-{access,error}.log</code>
          </div>
        </div>
        <div v-if="status?.lastExit" class="col" style="gap: 4px">
          <span class="muted" style="font-size: 10px; text-transform: uppercase; letter-spacing: 0.5px; color: var(--danger)">Last exit</span>
          <code style="font-size: 11px; color: var(--danger)">{{ status.lastExit }}</code>
        </div>
      </div>

      <!-- Recent output -->
      <div class="col" style="gap: 8px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
        <div class="row" style="justify-content: space-between; align-items: center">
          <strong>Recent xray output</strong>
          <span class="muted" style="font-size: 11px">
            last {{ status?.recentLogs?.length || 0 }} line{{ (status?.recentLogs?.length || 0) === 1 ? '' : 's' }} from stdout + stderr
          </span>
        </div>
        <pre v-if="status?.recentLogs?.length"
             style="margin: 0; padding: 10px; background: var(--panel-2); border: 1px solid var(--border); border-radius: 6px; font-size: 11px; max-height: 360px; overflow: auto; white-space: pre-wrap; font-family: ui-monospace, monospace">{{ status.recentLogs.join('\n') }}</pre>
        <span v-else class="muted" style="font-size: 12px; padding: 8px 0">
          {{ status?.enabled ? 'No output captured yet — xray may not have started.' : 'xray binary is not installed.' }}
        </span>
      </div>
    </template>
  </div>
</template>

<style scoped>
.subtab {
  background: transparent;
  border: none;
  border-bottom: 2px solid transparent;
  border-radius: 0;
  padding: 8px 14px;
  color: var(--text-dim);
  font-size: 12px;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  cursor: pointer;
}
.subtab:hover {
  color: var(--text);
}
.subtab.active {
  color: var(--text);
  border-bottom-color: var(--accent);
}
.subtab-badge {
  background: rgba(141, 141, 160, 0.15);
  color: var(--text-dim);
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 8px;
}
.subtab.active .subtab-badge {
  background: rgba(110, 168, 255, 0.15);
  color: var(--accent);
}
.subtab-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  display: inline-block;
}
</style>
