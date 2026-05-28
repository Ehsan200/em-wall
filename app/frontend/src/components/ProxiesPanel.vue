<script lang="ts" setup>
import { ref, onMounted, computed } from 'vue';
import {
  ListProxies, AddProxy, UpdateProxy, DeleteProxy, TestProxy,
} from '../../wailsjs/go/main/App';

// Local mirror of the proxy DTO so the component compiles even if
// wailsjs/models.ts hasn't been regenerated yet. The runtime values come
// from the daemon so any extra fields are ignored gracefully.
type ProxyRow = {
  id: number;
  name: string;
  protocol: string;
  host: string;
  port: number;
  username: string;
  hasPassword: boolean;
  createdAt: string;
  updatedAt: string;
};

const proxies = ref<ProxyRow[]>([]);
const error = ref<string>('');
const busy = ref<boolean>(false);
const testResult = ref<Record<number, { ok: boolean; message: string }>>({});

const draft = ref<{
  open: boolean;
  name: string;
  protocol: 'socks5' | 'http';
  host: string;
  port: number;
  username: string;
  password: string;
}>({
  open: false,
  name: '',
  protocol: 'socks5',
  host: '',
  port: 1080,
  username: '',
  password: '',
});

type EditState = {
  id: number;
  name: string;
  protocol: 'socks5' | 'http';
  host: string;
  port: number;
  username: string;
  password: string;
};
const editing = ref<EditState | null>(null);

// Two-click delete confirmation (mirrors RulesPanel pattern).
const pendingDelete = ref<number | null>(null);
let pendingDeleteTimer: number | undefined;

const draftIsValid = computed(() => {
  const d = draft.value;
  return !!d.name.trim()
      && !!d.host.trim()
      && d.port >= 1 && d.port <= 65535
      && (d.protocol === 'socks5' || d.protocol === 'http');
});

const editingIsValid = computed(() => {
  const e = editing.value;
  if (!e) return false;
  return !!e.name.trim()
      && !!e.host.trim()
      && e.port >= 1 && e.port <= 65535;
});

async function refresh() {
  try {
    proxies.value = ((await ListProxies()) || []) as unknown as ProxyRow[];
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

function openDraft() {
  draft.value.open = true;
  draft.value.name = '';
  draft.value.protocol = 'socks5';
  draft.value.host = '';
  draft.value.port = 1080;
  draft.value.username = '';
  draft.value.password = '';
}

function cancelDraft() {
  draft.value.open = false;
}

async function submitDraft() {
  if (!draftIsValid.value || busy.value) return;
  busy.value = true;
  try {
    await AddProxy(
      draft.value.name.trim(),
      draft.value.protocol,
      draft.value.host.trim(),
      Number(draft.value.port),
      draft.value.username.trim(),
      draft.value.password,
    );
    draft.value.open = false;
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

function beginEdit(p: ProxyRow) {
  editing.value = {
    id: p.id,
    name: p.name,
    protocol: (p.protocol === 'http' ? 'http' : 'socks5'),
    host: p.host,
    port: p.port,
    username: p.username,
    password: '',
  };
}

function cancelEdit() {
  editing.value = null;
}

async function saveEdit() {
  const e = editing.value;
  if (!e || !editingIsValid.value || busy.value) return;
  busy.value = true;
  try {
    await UpdateProxy(
      e.id, e.name.trim(), e.protocol, e.host.trim(),
      Number(e.port), e.username.trim(), e.password,
    );
    editing.value = null;
    await refresh();
  } catch (err: any) {
    error.value = err?.message || String(err);
  } finally {
    busy.value = false;
  }
}

function askDelete(p: ProxyRow) {
  if (pendingDelete.value === p.id) {
    confirmDelete(p);
    return;
  }
  pendingDelete.value = p.id;
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDeleteTimer = window.setTimeout(() => {
    pendingDelete.value = null;
  }, 3000);
}

async function confirmDelete(p: ProxyRow) {
  if (pendingDeleteTimer) window.clearTimeout(pendingDeleteTimer);
  pendingDelete.value = null;
  busy.value = true;
  try {
    await DeleteProxy(p.id);
    await refresh();
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function test(p: ProxyRow) {
  try {
    const r = await TestProxy(p.id);
    testResult.value = { ...testResult.value, [p.id]: { ok: r.ok, message: r.message } };
  } catch (e: any) {
    testResult.value = { ...testResult.value, [p.id]: { ok: false, message: e?.message || String(e) } };
  }
}

onMounted(refresh);
defineExpose({ refresh });
</script>

<template>
  <div class="panel">
  <div class="col" style="gap: 10px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
    <div class="row" style="justify-content: space-between; align-items: flex-start; gap: 16px">
      <div class="col" style="gap: 4px; flex: 1">
        <strong>Proxies</strong>
        <span class="muted" style="font-size: 12px; line-height: 1.5">
          Upstream HTTP or SOCKS5 proxies. Rules can route their resolved
          IPs through one of these instead of a network interface — set
          a rule's target to <code>proxy:NAME</code> in the Rules tab.
        </span>
        <span class="muted" style="font-size: 11px">
          Traffic for proxy-routed rules is tunneled through the upstream
          proxy via a daemon-owned interface — works even when a VPN owns
          the default route. TCP works for SOCKS5 and HTTP proxies; UDP
          (e.g. QUIC) is tunneled through SOCKS5 proxies only.
        </span>
      </div>
      <button v-if="!draft.open" @click="openDraft" :disabled="busy">+ Add proxy</button>
    </div>

    <div v-if="error" class="error" style="margin: 6px 0">{{ error }}</div>

    <!-- Existing proxies -->
    <div v-if="proxies.length === 0 && !draft.open" class="muted" style="font-size: 12px; padding: 8px 0">
      No proxies configured.
    </div>

    <div v-for="p in proxies" :key="p.id"
         class="col" style="gap: 6px; padding: 10px 12px; background: var(--panel-2); border: 1px solid var(--border); border-radius: 6px">
      <!-- View mode -->
      <template v-if="!editing || editing.id !== p.id">
        <div class="row" style="justify-content: space-between; gap: 8px; flex-wrap: wrap">
          <div class="col" style="gap: 2px; min-width: 0; flex: 1">
            <div class="row" style="gap: 8px; flex-wrap: wrap">
              <strong style="font-size: 13px">{{ p.name }}</strong>
              <span class="tag tag-route">{{ p.protocol }}</span>
              <code style="font-size: 12px">{{ p.host }}:{{ p.port }}</code>
              <span v-if="p.username" class="muted" style="font-size: 11px">user={{ p.username }}</span>
              <span v-if="p.hasPassword" class="muted" style="font-size: 11px">•••</span>
            </div>
          </div>
          <div class="row" style="gap: 6px">
            <button @click="test(p)" :disabled="busy">Test</button>
            <button @click="beginEdit(p)" :disabled="busy">Edit</button>
            <button @click="askDelete(p)" :disabled="busy"
                    :class="pendingDelete === p.id ? 'danger' : ''">
              {{ pendingDelete === p.id ? 'Confirm?' : 'Delete' }}
            </button>
          </div>
        </div>
        <div v-if="testResult[p.id]" class="muted" style="font-size: 11px"
             :style="{ color: testResult[p.id].ok ? 'var(--success)' : 'var(--text-dim)' }">
          {{ testResult[p.id].ok ? '✓' : '·' }} {{ testResult[p.id].message }}
        </div>
      </template>

      <!-- Edit mode -->
      <template v-else>
        <div class="row" style="gap: 8px; flex-wrap: wrap">
          <input v-model="editing.name" placeholder="name" style="width: 140px" />
          <select v-model="editing.protocol">
            <option value="socks5">socks5</option>
            <option value="http">http</option>
          </select>
          <input v-model="editing.host" placeholder="host" style="flex: 1; min-width: 140px" />
          <input type="number" v-model.number="editing.port" placeholder="port" style="width: 80px" min="1" max="65535" />
        </div>
        <div class="row" style="gap: 8px; flex-wrap: wrap">
          <input v-model="editing.username" placeholder="username (optional)" style="flex: 1; min-width: 140px" />
          <input v-model="editing.password" type="password"
                 :placeholder="p.hasPassword ? 'password (blank = keep)' : 'password (optional)'"
                 style="flex: 1; min-width: 140px" />
        </div>
        <div class="row" style="gap: 6px; justify-content: flex-end">
          <button @click="cancelEdit" :disabled="busy">Cancel</button>
          <button class="primary" @click="saveEdit" :disabled="!editingIsValid || busy">Save</button>
        </div>
      </template>
    </div>

    <!-- Add draft form -->
    <div v-if="draft.open"
         class="col" style="gap: 8px; padding: 10px 12px; background: var(--panel-2); border: 1px dashed var(--border); border-radius: 6px">
      <strong style="font-size: 12px; color: var(--text-dim); text-transform: uppercase; letter-spacing: 0.5px">New proxy</strong>
      <div class="row" style="gap: 8px; flex-wrap: wrap">
        <input v-model="draft.name" placeholder="name (e.g. work)" style="width: 140px" />
        <select v-model="draft.protocol">
          <option value="socks5">socks5</option>
          <option value="http">http</option>
        </select>
        <input v-model="draft.host" placeholder="host" style="flex: 1; min-width: 140px" />
        <input type="number" v-model.number="draft.port" placeholder="port" style="width: 80px" min="1" max="65535" />
      </div>
      <div class="row" style="gap: 8px; flex-wrap: wrap">
        <input v-model="draft.username" placeholder="username (optional)" style="flex: 1; min-width: 140px" />
        <input v-model="draft.password" type="password" placeholder="password (optional)" style="flex: 1; min-width: 140px" />
      </div>
      <div class="row" style="gap: 6px; justify-content: flex-end">
        <button @click="cancelDraft" :disabled="busy">Cancel</button>
        <button class="primary" @click="submitDraft" :disabled="!draftIsValid || busy">Add proxy</button>
      </div>
    </div>
  </div>
  </div>
</template>
