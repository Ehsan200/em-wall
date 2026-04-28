<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import { ListRules, AddRule, UpdateRule, DeleteRule, Interfaces, Apps, Groups, ApplyGroup } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';
import AppIcon from './AppIcon.vue';
import GroupIcon from './GroupIcon.vue';

const emit = defineEmits<{ (e: 'changed'): void }>();

const rules = ref<ipc.RuleDTO[]>([]);
const interfaces = ref<ipc.InterfaceDTO[]>([]);
const apps = ref<ipc.AppDTO[]>([]);
const knownGroups = ref<ipc.GroupDTO[]>([]);
const error = ref<string>('');
const pendingDelete = ref<number | null>(null);
let pendingDeleteTimer: number | undefined;
let ifaceTimer: number | undefined;

// Group-apply working state. When the user clicks a group card, this
// gets populated with the same shape as `draft` so the same binding
// picker UI can be reused.
const groupApply = ref<{
  key: string;
  action: 'block' | 'route';
  binding: 'iface' | 'app';
  iface: string;
  apps: string[];
  enabled: boolean;
} | null>(null);

function openGroupForm(g: ipc.GroupDTO) {
  groupApply.value = {
    key: g.key,
    action: 'block',
    binding: 'iface',
    iface: '',
    apps: [],
    enabled: true,
  };
}

function closeGroupForm() { groupApply.value = null; }

function toggleGroupApp(key: string) {
  if (!groupApply.value) return;
  const idx = groupApply.value.apps.indexOf(key);
  if (idx >= 0) groupApply.value.apps.splice(idx, 1);
  else groupApply.value.apps.push(key);
}

function groupInterfaceField(): string {
  const g = groupApply.value;
  if (!g || g.action !== 'route') return '';
  if (g.binding === 'app' && g.apps.length > 0) return `app:${g.apps.join(',')}`;
  if (g.binding === 'iface') return g.iface;
  return '';
}

function groupApplyValid(): boolean {
  const g = groupApply.value;
  if (!g) return false;
  if (g.action === 'block') return true;
  if (g.binding === 'iface') return !!g.iface;
  return g.apps.length > 0;
}

async function applyGroup() {
  const g = groupApply.value;
  if (!g || !groupApplyValid()) return;
  try {
    const result = await ApplyGroup(g.key, g.action, groupInterfaceField(), g.enabled);
    const created = result.created?.length || 0;
    const skipped = result.skipped?.length || 0;
    error.value = '';
    if (skipped > 0) {
      error.value = `Created ${created} rule(s); ${skipped} pattern(s) skipped (already exist).`;
    }
    groupApply.value = null;
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

function groupByKey(key: string): ipc.GroupDTO | undefined {
  return knownGroups.value.find(g => g.key === key);
}

// Action model:
//   block — return NXDOMAIN
//   route — let through and pin the resolved IPs to a binding
//
// allow exists in the daemon for "explicit pass-through that
// overrides a broader block" but is not exposed in the UI — the
// implicit default for unmatched domains is already "allow".
const draft = ref({
  pattern: '',
  action: 'block' as 'block' | 'route',
  // Only meaningful when action === 'route':
  binding: 'iface' as 'iface' | 'app',
  iface: '',
  apps: [] as string[],
  enabled: true,
});

function draftInterfaceField(): string {
  if (draft.value.action !== 'route') return '';
  if (draft.value.binding === 'app' && draft.value.apps.length > 0) {
    return `app:${draft.value.apps.join(',')}`;
  }
  if (draft.value.binding === 'iface') return draft.value.iface;
  return '';
}

function draftIsValid(): boolean {
  if (!draft.value.pattern.trim()) return false;
  if (draft.value.action === 'block') return true;
  // route requires a non-empty binding
  if (draft.value.binding === 'iface') return !!draft.value.iface;
  if (draft.value.binding === 'app') return draft.value.apps.length > 0;
  return false;
}

function toggleDraftApp(key: string) {
  const idx = draft.value.apps.indexOf(key);
  if (idx >= 0) {
    draft.value.apps.splice(idx, 1);
  } else {
    draft.value.apps.push(key);
  }
}

// ---- Inline edit ------------------------------------------------------

type EditState = {
  id: number;
  pattern: string;
  action: 'block' | 'route';
  binding: 'iface' | 'app';
  iface: string;
  apps: string[];
  enabled: boolean;
};

const editing = ref<EditState | null>(null);

function beginEdit(r: ipc.RuleDTO) {
  let binding: 'iface' | 'app' = 'iface';
  let iface = '';
  let appKeys: string[] = [];
  if (r.interface.startsWith('app:')) {
    binding = 'app';
    appKeys = r.interface.substring(4).split(',').map(s => s.trim()).filter(Boolean);
  } else if (r.interface) {
    binding = 'iface';
    iface = r.interface;
  }
  // Legacy `allow` rows (pre-action-model split) collapse to `route`
  // when they had an interface, otherwise to `block` for editing.
  const action: 'block' | 'route' = r.action === 'route' ? 'route'
    : (r.action === 'allow' && r.interface) ? 'route'
    : (r.action === 'block') ? 'block'
    : 'block';
  editing.value = {
    id: r.id,
    pattern: r.pattern,
    action,
    binding,
    iface,
    apps: appKeys,
    enabled: r.enabled,
  };
}

function cancelEdit() { editing.value = null; }

function editingInterfaceField(): string {
  const e = editing.value;
  if (!e || e.action !== 'route') return '';
  if (e.binding === 'app' && e.apps.length > 0) return `app:${e.apps.join(',')}`;
  if (e.binding === 'iface') return e.iface;
  return '';
}

function editingIsValid(): boolean {
  const e = editing.value;
  if (!e || !e.pattern.trim()) return false;
  if (e.action === 'block') return true;
  if (e.binding === 'iface') return !!e.iface;
  if (e.binding === 'app') return e.apps.length > 0;
  return false;
}

function toggleEditingApp(key: string) {
  if (!editing.value) return;
  const idx = editing.value.apps.indexOf(key);
  if (idx >= 0) editing.value.apps.splice(idx, 1);
  else editing.value.apps.push(key);
}

async function saveEdit() {
  const e = editing.value;
  if (!editingIsValid()) return;
  try {
    await UpdateRule(e!.id, e!.pattern.trim(), e!.action, editingInterfaceField(), e!.enabled);
    editing.value = null;
    await refresh();
    emit('changed');
  } catch (err: any) {
    error.value = err?.message || String(err);
  }
}

async function refresh() {
  try {
    rules.value = (await ListRules()) || [];
    interfaces.value = (await Interfaces()) || [];
    apps.value = (await Apps()) || [];
    if (knownGroups.value.length === 0) {
      knownGroups.value = (await Groups()) || [];
    }
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function add() {
  if (!draftIsValid()) return;
  try {
    await AddRule(draft.value.pattern.trim(), draft.value.action,
      draftInterfaceField(),
      draft.value.enabled);
    draft.value.pattern = '';
    draft.value.iface = '';
    draft.value.apps = [];
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function toggle(r: ipc.RuleDTO) {
  try {
    await UpdateRule(r.id, r.pattern, r.action, r.interface, !r.enabled);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

function askDelete(r: ipc.RuleDTO) {
  if (pendingDelete.value === r.id) {
    confirmDelete(r);
    return;
  }
  pendingDelete.value = r.id;
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDeleteTimer = window.setTimeout(() => {
    pendingDelete.value = null;
  }, 3000);
}

async function confirmDelete(r: ipc.RuleDTO) {
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDelete.value = null;
  try {
    await DeleteRule(r.id);
    await refresh();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}


async function refreshLive() {
  // Polled every few seconds: interfaces + apps so chips reflect
  // live "running" state without requiring a rule edit. Rules
  // themselves don't refresh here — they only change on user action.
  try {
    const [ifs, ap] = await Promise.all([Interfaces(), Apps()]);
    interfaces.value = ifs || [];
    apps.value = ap || [];
  } catch (_) { /* ignore — keep last good */ }
}

function ifaceLabel(i: ipc.InterfaceDTO): string {
  return i.owner ? `${i.name} — ${i.owner}` : i.name;
}

// Rule binding helpers. The `interface` field on a Rule can be:
//   ''         → default route
//   'utunN'    → fixed interface
//   'app:KEY'  → bound to an app (resolved live)
function ruleIsApp(field: string): boolean { return field.startsWith('app:'); }
function ruleAppKey(field: string): string { return field.replace(/^app:/, ''); }

// "app:v2box,hiddify" → ["v2box","hiddify"]
function ruleAppKeys(field: string): string[] {
  return ruleAppKey(field).split(',').map(s => s.trim()).filter(Boolean);
}

function appByKey(key: string): ipc.AppDTO | undefined {
  return apps.value.find(a => a.key === key);
}

function appDisplayName(key: string): string {
  return appByKey(key)?.displayName || key;
}

// One-word live-state badge: "utun4" / "off" / "—"
function appStatusBadge(key: string): string {
  const a = appByKey(key);
  if (!a) return '?';
  if (a.currentInterface) return a.currentInterface;
  return a.installed ? 'off' : '—';
}

function appStatusLabel(key: string): string {
  const a = appByKey(key);
  if (!a) return `${key} — unknown app`;
  if (a.currentInterface) return `connected via ${a.currentInterface}`;
  return a.installed ? 'installed but not running' : 'not installed';
}

// True if a rule's saved binding can't be honoured right now.
// For multi-app rules ("app:v2box,hiddify"), down means NONE of the
// listed apps is currently running — matches the daemon's behaviour
// of trying each in order and returning NXDOMAIN if all are down.
function bindingDown(field: string): boolean {
  if (!field) return false;
  if (ruleIsApp(field)) {
    const keys = ruleAppKeys(field);
    return !keys.some(k => !!appByKey(k)?.currentInterface);
  }
  return !interfaces.value.some(i => i.name === field);
}


onMounted(() => {
  refresh();
  ifaceTimer = window.setInterval(refreshLive, 2000);
});
onUnmounted(() => { if (ifaceTimer) window.clearInterval(ifaceTimer); });
</script>

<template>
  <div class="panel">
    <h2>Rules</h2>
    <div v-if="error" class="error">{{ error }}</div>

    <!-- Quick add from a known service group -->
    <div class="groups-bar">
      <div class="muted" style="font-size: 11px; margin-bottom: 8px">
        Quick add — one click creates rules for every domain of a service:
      </div>
      <div class="row" style="gap: 6px; flex-wrap: wrap">
        <button v-for="g in knownGroups" :key="g.key"
                class="group-card"
                @click="openGroupForm(g)"
                :title="g.description + '\n\n' + g.patterns.join('\n')">
          <GroupIcon :group-key="g.key" :size="20" />
          <span class="group-name">{{ g.displayName }}</span>
          <span class="muted" style="font-size: 10px">{{ g.patterns.length }} domain{{ g.patterns.length === 1 ? '' : 's' }}</span>
        </button>
      </div>
    </div>

    <!-- Inline group-apply form, opens when a group card is clicked -->
    <div v-if="groupApply" class="add-form" style="border-left: 3px solid var(--accent)">
      <div class="row" style="gap: 10px; align-items: center; margin-bottom: 8px">
        <GroupIcon :group-key="groupApply.key" :size="22" />
        <strong>{{ groupByKey(groupApply.key)?.displayName }}</strong>
        <span class="muted" style="font-size: 11px">
          {{ groupByKey(groupApply.key)?.patterns.length }} pattern(s):
          <code style="font-size: 11px">{{ groupByKey(groupApply.key)?.patterns.join(', ') }}</code>
        </span>
      </div>
      <div class="row" style="gap: 8px">
        <select v-model="groupApply.action" style="width: 100px">
          <option value="block">block</option>
          <option value="route">route</option>
        </select>
        <label class="toggle">
          <input type="checkbox" v-model="groupApply.enabled" />
          <span class="track"></span>
        </label>
        <button class="primary" @click="applyGroup" :disabled="!groupApplyValid()">Create rules</button>
        <button @click="closeGroupForm">Cancel</button>
      </div>
      <div v-if="groupApply.action === 'route'" class="col" style="gap: 10px; margin-top: 10px">
        <div class="row" style="gap: 8px; align-items: center">
          <span class="muted" style="font-size: 11px; min-width: 60px">via:</span>
          <div class="row" style="gap: 0">
            <button :class="['seg', {active: groupApply.binding === 'iface'}]" @click="groupApply.binding = 'iface'">Interface</button>
            <button :class="['seg', {active: groupApply.binding === 'app'}]" @click="groupApply.binding = 'app'">App</button>
          </div>
          <select v-if="groupApply.binding === 'iface'" v-model="groupApply.iface" style="flex: 1">
            <option value="">— pick interface —</option>
            <option v-for="i in interfaces" :key="i.name" :value="i.name">{{ ifaceLabel(i) }} (mtu {{ i.mtu }})</option>
          </select>
          <span v-else class="muted" style="font-size: 11px; flex: 1">
            select one or more — daemon uses the first one that's running
            <span v-if="groupApply.apps.length" style="color: var(--accent); font-weight: 600">
              · {{ groupApply.apps.length }} selected
            </span>
          </span>
        </div>
        <div v-if="groupApply.binding === 'app'" class="chip-grid">
          <button v-for="a in apps" :key="a.key"
                  :class="['app-chip', {active: groupApply.apps.includes(a.key), 'not-installed': !a.installed, 'not-running': a.installed && !a.currentInterface}]"
                  @click="toggleGroupApp(a.key)">
            <AppIcon :app-key="a.key" :size="20" />
            <span>{{ a.displayName }}</span>
            <span v-if="groupApply.apps.includes(a.key)" class="chip-rank">{{ groupApply.apps.indexOf(a.key) + 1 }}</span>
          </button>
        </div>
      </div>
    </div>

    <div class="add-form">
      <div class="row" style="gap: 8px">
        <input v-model="draft.pattern" placeholder="Pattern (e.g. *.bad.com)" style="flex: 1" @keyup.enter="add" />
        <select v-model="draft.action" style="width: 100px">
          <option value="block">block</option>
          <option value="route">route</option>
        </select>
        <label class="toggle">
          <input type="checkbox" v-model="draft.enabled" />
          <span class="track"></span>
        </label>
        <button class="primary" @click="add" :disabled="!draftIsValid()">Add</button>
      </div>
      <div v-if="draft.action === 'route'" class="col" style="gap: 10px; margin-top: 10px">
        <div class="row" style="gap: 8px; align-items: center">
          <span class="muted" style="font-size: 11px; min-width: 60px">via:</span>
          <div class="row" style="gap: 0">
            <button :class="['seg', {active: draft.binding === 'iface'}]" @click="draft.binding = 'iface'">Interface</button>
            <button :class="['seg', {active: draft.binding === 'app'}]" @click="draft.binding = 'app'">App</button>
          </div>
          <select v-if="draft.binding === 'iface'" v-model="draft.iface" style="flex: 1">
            <option value="">— pick interface —</option>
            <option v-for="i in interfaces" :key="i.name" :value="i.name">{{ ifaceLabel(i) }} (mtu {{ i.mtu }})</option>
          </select>
          <span v-else-if="draft.binding === 'app'" class="muted" style="font-size: 11px; flex: 1">
            select one or more — daemon uses the first one that's running
            <span v-if="draft.apps.length" style="color: var(--accent); font-weight: 600">
              · {{ draft.apps.length }} selected
            </span>
          </span>
        </div>
        <div v-if="draft.binding === 'app'" class="chip-grid">
          <button v-for="a in apps" :key="a.key"
                  :class="['app-chip', {active: draft.apps.includes(a.key), 'not-installed': !a.installed, 'not-running': a.installed && !a.currentInterface}]"
                  @click="toggleDraftApp(a.key)"
                  :title="a.installed ? (a.currentInterface ? `connected via ${a.currentInterface}` : 'installed but not running — rule will block matching domains until app connects') : 'not installed — rule won\'t resolve until you install the app'">
            <AppIcon :app-key="a.key" :size="20" />
            <span>{{ a.displayName }}</span>
            <span v-if="draft.apps.includes(a.key)" class="chip-rank">{{ draft.apps.indexOf(a.key) + 1 }}</span>
          </button>
        </div>
      </div>
    </div>

    <table>
      <thead>
        <tr>
          <th>Pattern</th>
          <th>Action</th>
          <th>Interface</th>
          <th>Enabled</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        <template v-for="r in rules" :key="r.id">
          <!-- Compact display row -->
          <tr v-if="editing?.id !== r.id"
              :class="{'rule-iface-down': r.action === 'route' && bindingDown(r.interface)}">
            <td><code>{{ r.pattern }}</code></td>
            <td>
              <span :class="['tag', r.action === 'block' ? 'tag-block' : 'tag-route']">
                {{ r.action }}
              </span>
              <span v-if="r.action === 'route' && bindingDown(r.interface)"
                    class="tag tag-block" style="margin-left: 6px"
                    :title="ruleIsApp(r.interface) ? 'App not running — queries return NXDOMAIN until it connects' : 'Configured interface is not up — queries return NXDOMAIN until it comes back'">
                ⚠ {{ ruleIsApp(r.interface) ? 'app down' : 'iface down' }}
              </span>
            </td>
            <td>
              <!-- Multi-app: render one chip per key with its live state -->
              <div v-if="r.action === 'route' && ruleIsApp(r.interface)" class="row" style="gap: 4px; flex-wrap: wrap">
                <span v-for="(k, i) in ruleAppKeys(r.interface)" :key="k"
                      class="row" style="gap: 4px; padding: 2px 6px; border: 1px solid var(--border); border-radius: 12px; font-size: 11px"
                      :title="appStatusLabel(k)">
                  <AppIcon :app-key="k" :size="14" />
                  <span>{{ appDisplayName(k) }}</span>
                  <span class="muted">{{ appStatusBadge(k) }}</span>
                  <span v-if="i < ruleAppKeys(r.interface).length - 1" class="muted">·</span>
                </span>
              </div>
              <!-- Fixed interface -->
              <code v-else-if="r.action === 'route'" style="font-size: 12px">{{ r.interface }}</code>
              <span v-else class="muted">—</span>
            </td>
            <td>
              <label class="toggle" @click.prevent="toggle(r)">
                <input type="checkbox" :checked="r.enabled" />
                <span class="track"></span>
              </label>
            </td>
            <td style="white-space: nowrap">
              <button @click="beginEdit(r)" style="margin-right: 4px">Edit</button>
              <button
                :class="pendingDelete === r.id ? 'danger primary' : 'danger'"
                @click="askDelete(r)"
              >
                {{ pendingDelete === r.id ? 'Confirm?' : 'Delete' }}
              </button>
            </td>
          </tr>

          <!-- Edit row: full-width form, same shape as Add -->
          <tr v-else class="edit-row">
            <td colspan="5">
              <div class="edit-card">
                <div class="row" style="gap: 8px">
                  <input v-model="editing!.pattern"
                         placeholder="Pattern (e.g. *.bad.com)"
                         style="flex: 1"
                         @keyup.enter="saveEdit"
                         @keyup.esc="cancelEdit" />
                  <select v-model="editing!.action" style="width: 100px">
                    <option value="block">block</option>
                    <option value="route">route</option>
                  </select>
                  <label class="toggle">
                    <input type="checkbox" v-model="editing!.enabled" />
                    <span class="track"></span>
                  </label>
                  <button class="primary" @click="saveEdit" :disabled="!editingIsValid()">Save</button>
                  <button @click="cancelEdit">Cancel</button>
                </div>
                <div v-if="editing!.action === 'route'" class="col" style="gap: 10px; margin-top: 10px">
                  <div class="row" style="gap: 8px; align-items: center">
                    <span class="muted" style="font-size: 11px; min-width: 60px">via:</span>
                    <div class="row" style="gap: 0">
                      <button :class="['seg', {active: editing!.binding === 'iface'}]" @click="editing!.binding = 'iface'">Interface</button>
                      <button :class="['seg', {active: editing!.binding === 'app'}]" @click="editing!.binding = 'app'">App</button>
                    </div>
                    <select v-if="editing!.binding === 'iface'" v-model="editing!.iface" style="flex: 1">
                      <option value="">— pick interface —</option>
                      <option v-for="i in interfaces" :key="i.name" :value="i.name">{{ ifaceLabel(i) }} (mtu {{ i.mtu }})</option>
                      <option v-if="editing!.iface && !interfaces.some(i => i.name === editing!.iface)"
                              :value="editing!.iface" disabled>{{ editing!.iface }} — down (saved)</option>
                    </select>
                    <span v-else-if="editing!.binding === 'app'" class="muted" style="font-size: 11px; flex: 1">
                      select one or more — daemon uses the first one that's running
                      <span v-if="editing!.apps.length" style="color: var(--accent); font-weight: 600">
                        · {{ editing!.apps.length }} selected
                      </span>
                    </span>
                  </div>
                  <div v-if="editing!.binding === 'app'" class="chip-grid">
                    <button v-for="a in apps" :key="a.key"
                            :class="['app-chip', {active: editing!.apps.includes(a.key), 'not-installed': !a.installed, 'not-running': a.installed && !a.currentInterface}]"
                            @click="toggleEditingApp(a.key)"
                            :title="a.installed ? (a.currentInterface ? `connected via ${a.currentInterface}` : 'installed but not running') : 'not installed'">
                      <AppIcon :app-key="a.key" :size="20" />
                      <span>{{ a.displayName }}</span>
                      <span v-if="editing!.apps.includes(a.key)" class="chip-rank">{{ editing!.apps.indexOf(a.key) + 1 }}</span>
                    </button>
                  </div>
                </div>
              </div>
            </td>
          </tr>
        </template>
        <tr v-if="!rules.length">
          <td colspan="5" class="muted" style="text-align:center; padding: 24px">
            No rules yet. Unmatched domains are allowed by default.
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>

<style scoped>
tr.rule-iface-down td { background: rgba(255, 111, 111, 0.05); }
tr.rule-iface-down code { opacity: 0.85; }

tr.edit-row td {
  padding: 0;
  background: rgba(110, 168, 255, 0.04);
}
.edit-card {
  border-left: 3px solid var(--accent);
  padding: 12px 14px;
}

.add-form {
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 12px;
  margin-bottom: 16px;
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

.chip-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  max-height: 220px;
  overflow-y: auto;
}

.app-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px 4px 6px;
  border: 1px solid var(--border);
  border-radius: 16px;
  background: var(--panel-2);
  cursor: pointer;
  font-size: 12px;
  color: var(--text);
  position: relative;
}
.app-chip:hover { background: var(--border); }
.app-chip.active {
  border-color: var(--accent);
  background: rgba(110, 168, 255, 0.15);
}
.app-chip.not-installed { opacity: 0.45; }
.app-chip.not-running { color: var(--warn); }

.groups-bar {
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 12px;
  margin-bottom: 12px;
}
.group-card {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 6px 12px 6px 8px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--panel-2);
  cursor: pointer;
  font-size: 12px;
  color: var(--text);
}
.group-card:hover { background: var(--border); border-color: var(--accent); }
.group-card .group-name { font-weight: 600; }

.chip-rank {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 16px;
  height: 16px;
  border-radius: 50%;
  background: var(--accent);
  color: #fff;
  font-size: 10px;
  font-weight: 700;
  margin-left: 2px;
}
</style>
