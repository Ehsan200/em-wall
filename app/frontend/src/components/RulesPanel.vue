<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import { ListRules, AddRule, UpdateRule, DeleteRule, Interfaces, Apps } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';
import AppIcon from './AppIcon.vue';

const emit = defineEmits<{ (e: 'changed'): void }>();

const rules = ref<ipc.RuleDTO[]>([]);
const interfaces = ref<ipc.InterfaceDTO[]>([]);
const apps = ref<ipc.AppDTO[]>([]);
const error = ref<string>('');
const pendingDelete = ref<number | null>(null);
let pendingDeleteTimer: number | undefined;
let ifaceTimer: number | undefined;

// Draft "binding" describes how the route is pinned:
//   ''        → default route (any interface)
//   'iface'   → a specific utun/en (stored as that name)
//   'app'     → one or more known apps (stored as 'app:KEY1,KEY2,...').
//              At resolve time, the daemon picks the first one that's
//              currently running.
const draft = ref({
  pattern: '',
  action: 'block' as 'block' | 'allow',
  binding: '' as '' | 'iface' | 'app',
  iface: '',
  apps: [] as string[],
  enabled: true,
});

function draftInterfaceField(): string {
  if (draft.value.binding === 'app' && draft.value.apps.length > 0) {
    return `app:${draft.value.apps.join(',')}`;
  }
  if (draft.value.binding === 'iface') return draft.value.iface;
  return '';
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
  action: 'block' | 'allow';
  binding: '' | 'iface' | 'app';
  iface: string;
  apps: string[];
  enabled: boolean;
};
const editing = ref<EditState | null>(null);

function beginEdit(r: ipc.RuleDTO) {
  let binding: '' | 'iface' | 'app' = '';
  let iface = '';
  let appKeys: string[] = [];
  if (r.interface.startsWith('app:')) {
    binding = 'app';
    appKeys = r.interface.substring(4).split(',').map(s => s.trim()).filter(Boolean);
  } else if (r.interface) {
    binding = 'iface';
    iface = r.interface;
  }
  editing.value = {
    id: r.id,
    pattern: r.pattern,
    action: (r.action === 'allow' ? 'allow' : 'block'),
    binding,
    iface,
    apps: appKeys,
    enabled: r.enabled,
  };
}

function cancelEdit() { editing.value = null; }

function editingInterfaceField(): string {
  const e = editing.value;
  if (!e || e.action !== 'allow') return '';
  if (e.binding === 'app' && e.apps.length > 0) return `app:${e.apps.join(',')}`;
  if (e.binding === 'iface') return e.iface;
  return '';
}

function toggleEditingApp(key: string) {
  if (!editing.value) return;
  const idx = editing.value.apps.indexOf(key);
  if (idx >= 0) editing.value.apps.splice(idx, 1);
  else editing.value.apps.push(key);
}

async function saveEdit() {
  const e = editing.value;
  if (!e || !e.pattern.trim()) return;
  try {
    await UpdateRule(e.id, e.pattern.trim(), e.action, editingInterfaceField(), e.enabled);
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
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function add() {
  if (!draft.value.pattern.trim()) return;
  try {
    const ifaceField = draft.value.action === 'allow' ? draftInterfaceField() : '';
    await AddRule(draft.value.pattern.trim(), draft.value.action,
      ifaceField,
      draft.value.enabled);
    draft.value.pattern = '';
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

function ruleAppDown(field: string): boolean {
  if (!ruleIsApp(field)) return false;
  const key = ruleAppKey(field);
  const a = apps.value.find(x => x.key === key);
  return !a || !a.currentInterface;
}

// True if a rule's saved binding can't be honoured right now.
function bindingDown(field: string): boolean {
  if (!field) return false;
  if (ruleIsApp(field)) return ruleAppDown(field);
  return !interfaces.value.some(i => i.name === field);
}

function ruleBindingLabel(field: string): string {
  if (!field) return '—';
  if (ruleIsApp(field)) {
    const key = ruleAppKey(field);
    const a = apps.value.find(x => x.key === key);
    if (!a) return `app:${key} (unknown)`;
    return a.currentInterface
      ? `${a.displayName} → ${a.currentInterface}`
      : `${a.displayName} (not running)`;
  }
  return field;
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

    <div class="add-form">
      <div class="row" style="gap: 8px">
        <input v-model="draft.pattern" placeholder="Pattern (e.g. *.bad.com)" style="flex: 1" @keyup.enter="add" />
        <select v-model="draft.action" style="width: 100px">
          <option value="block">block</option>
          <option value="allow">allow</option>
        </select>
        <label class="toggle">
          <input type="checkbox" v-model="draft.enabled" />
          <span class="track"></span>
        </label>
        <button class="primary" @click="add" :disabled="!draft.pattern.trim()">Add</button>
      </div>
      <div v-if="draft.action === 'allow'" class="col" style="gap: 10px; margin-top: 10px">
        <div class="row" style="gap: 8px; align-items: center">
          <span class="muted" style="font-size: 11px; min-width: 60px">route via:</span>
          <div class="row" style="gap: 0">
            <button :class="['seg', {active: draft.binding === ''}]" @click="draft.binding = ''">Default</button>
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
              :class="{'rule-iface-down': r.action === 'allow' && r.interface && bindingDown(r.interface)}">
            <td><code>{{ r.pattern }}</code></td>
            <td>
              <span :class="['tag', r.action === 'block' ? 'tag-block' : (r.interface ? 'tag-route' : 'tag-allow')]">
                {{ r.action === 'block' ? 'block' : (r.interface ? 'route' : 'allow') }}
              </span>
              <span v-if="r.action === 'allow' && r.interface && bindingDown(r.interface)"
                    class="tag tag-block" style="margin-left: 6px"
                    :title="ruleIsApp(r.interface) ? 'App not running — queries return NXDOMAIN until it connects' : 'Configured interface is not up — queries return NXDOMAIN until it comes back'">
                ⚠ {{ ruleIsApp(r.interface) ? 'app down' : 'iface down' }}
              </span>
            </td>
            <td>
              <div v-if="r.action === 'allow'" class="row" style="gap: 6px; align-items: center">
                <AppIcon v-if="ruleIsApp(r.interface)" :app-key="ruleAppKey(r.interface)" :size="18" />
                <code style="font-size: 12px">{{ ruleBindingLabel(r.interface) }}</code>
              </div>
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
                    <option value="allow">allow</option>
                  </select>
                  <label class="toggle">
                    <input type="checkbox" v-model="editing!.enabled" />
                    <span class="track"></span>
                  </label>
                  <button class="primary" @click="saveEdit" :disabled="!editing!.pattern.trim()">Save</button>
                  <button @click="cancelEdit">Cancel</button>
                </div>
                <div v-if="editing!.action === 'allow'" class="col" style="gap: 10px; margin-top: 10px">
                  <div class="row" style="gap: 8px; align-items: center">
                    <span class="muted" style="font-size: 11px; min-width: 60px">route via:</span>
                    <div class="row" style="gap: 0">
                      <button :class="['seg', {active: editing!.binding === ''}]" @click="editing!.binding = ''">Default</button>
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
