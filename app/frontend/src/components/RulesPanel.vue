<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import { ListRules, AddRule, UpdateRule, DeleteRule, Interfaces } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

const emit = defineEmits<{ (e: 'changed'): void }>();

const rules = ref<ipc.RuleDTO[]>([]);
const interfaces = ref<ipc.InterfaceDTO[]>([]);
const error = ref<string>('');
const pendingDelete = ref<number | null>(null);
let pendingDeleteTimer: number | undefined;
let ifaceTimer: number | undefined;

const draft = ref({ pattern: '', action: 'block', interface: '', enabled: true });

async function refresh() {
  try {
    rules.value = (await ListRules()) || [];
    interfaces.value = (await Interfaces()) || [];
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function add() {
  if (!draft.value.pattern.trim()) return;
  try {
    await AddRule(draft.value.pattern.trim(), draft.value.action,
      draft.value.action === 'allow' ? draft.value.interface : '',
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

async function changeIface(r: ipc.RuleDTO, iface: string) {
  try {
    await UpdateRule(r.id, r.pattern, r.action, iface, r.enabled);
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function refreshInterfaces() {
  try {
    interfaces.value = (await Interfaces()) || [];
  } catch (_) { /* ignore — keep last good */ }
}

function ifaceLabel(i: ipc.InterfaceDTO): string {
  return i.owner ? `${i.name} — ${i.owner}` : i.name;
}

// True if the rule's chosen interface is not currently up. Used to
// surface a warning while preserving the saved value.
function ifaceDown(name: string): boolean {
  if (!name) return false;
  return !interfaces.value.some(i => i.name === name);
}

// Returns the dropdown options for a given rule, always including the
// rule's currently saved interface even if it's missing/down.
function optionsFor(saved: string): { name: string; label: string; down: boolean }[] {
  const opts = interfaces.value.map(i => ({ name: i.name, label: ifaceLabel(i), down: false }));
  if (saved && !interfaces.value.some(i => i.name === saved)) {
    opts.unshift({ name: saved, label: `${saved} — down`, down: true });
  }
  return opts;
}

onMounted(() => {
  refresh();
  ifaceTimer = window.setInterval(refreshInterfaces, 3000);
});
onUnmounted(() => { if (ifaceTimer) window.clearInterval(ifaceTimer); });
</script>

<template>
  <div class="panel">
    <h2>Rules</h2>
    <div v-if="error" class="error">{{ error }}</div>

    <div class="form-row">
      <input v-model="draft.pattern" placeholder="Pattern (e.g. *.bad.com)" @keyup.enter="add" />
      <select v-model="draft.action">
        <option value="block">block</option>
        <option value="allow">allow</option>
      </select>
      <select v-model="draft.interface" :disabled="draft.action !== 'allow'">
        <option value="">default route</option>
        <option v-for="i in interfaces" :key="i.name" :value="i.name">{{ ifaceLabel(i) }} (mtu {{ i.mtu }})</option>
      </select>
      <label class="toggle">
        <input type="checkbox" v-model="draft.enabled" />
        <span class="track"></span>
      </label>
      <button class="primary" @click="add" :disabled="!draft.pattern.trim()">Add</button>
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
        <tr v-for="r in rules" :key="r.id" :class="{'rule-iface-down': r.action === 'allow' && r.interface && ifaceDown(r.interface)}">
          <td><code>{{ r.pattern }}</code></td>
          <td>
            <span :class="['tag', r.action === 'block' ? 'tag-block' : (r.interface ? 'tag-route' : 'tag-allow')]">
              {{ r.action === 'block' ? 'block' : (r.interface ? 'route' : 'allow') }}
            </span>
            <span v-if="r.action === 'allow' && r.interface && ifaceDown(r.interface)"
                  class="tag tag-block" style="margin-left: 6px"
                  title="Configured interface is not up — queries to this domain return NXDOMAIN until the interface comes back">
              ⚠ iface down
            </span>
          </td>
          <td>
            <select v-if="r.action === 'allow'" :value="r.interface" @change="changeIface(r, ($event.target as HTMLSelectElement).value)">
              <option value="">default route</option>
              <option v-for="o in optionsFor(r.interface)" :key="o.name" :value="o.name" :disabled="o.down">{{ o.label }}</option>
            </select>
            <span v-else class="muted">—</span>
          </td>
          <td>
            <label class="toggle" @click.prevent="toggle(r)">
              <input type="checkbox" :checked="r.enabled" />
              <span class="track"></span>
            </label>
          </td>
          <td>
            <button
              :class="pendingDelete === r.id ? 'danger primary' : 'danger'"
              @click="askDelete(r)"
            >
              {{ pendingDelete === r.id ? 'Confirm?' : 'Delete' }}
            </button>
          </td>
        </tr>
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
</style>
