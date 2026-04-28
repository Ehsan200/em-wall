<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import { Interfaces, SystemRoutes, ActiveRoutes } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

type Section = 'interfaces' | 'routes' | 'pinned';
const section = ref<Section>('interfaces');

const interfaces = ref<ipc.InterfaceDTO[]>([]);
const sysRoutes = ref<ipc.SystemRouteDTO[]>([]);
const ourRoutes = ref<ipc.ActiveRouteDTO[]>([]);
const lastRefresh = ref<Date | null>(null);
const error = ref<string>('');
let timer: number | undefined;

async function refresh() {
  try {
    const [ifs, srs, prs] = await Promise.all([
      Interfaces(),
      SystemRoutes(),
      ActiveRoutes(),
    ]);
    interfaces.value = ifs || [];
    sysRoutes.value = srs || [];
    ourRoutes.value = prs || [];
    lastRefresh.value = new Date();
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

function fmtTime(s: string): string {
  try { return new Date(s).toLocaleTimeString(); } catch { return s; }
}

function isTunnel(name: string): boolean {
  return /^(utun|ipsec|ppp|tun|tap)/.test(name);
}

onMounted(() => {
  refresh();
  timer = window.setInterval(refresh, 5000);
});
onUnmounted(() => { if (timer) window.clearInterval(timer); });
</script>

<template>
  <div class="panel">
    <div v-if="error" class="error">{{ error }}</div>

    <div class="row" style="justify-content: space-between; align-items: baseline; margin-bottom: 12px">
      <div class="row" style="gap: 0">
        <button :class="{active: section==='interfaces'}" class="seg" @click="section='interfaces'">
          Interfaces <span class="count">{{ interfaces.length }}</span>
        </button>
        <button :class="{active: section==='routes'}" class="seg" @click="section='routes'">
          System routes <span class="count">{{ sysRoutes.length }}</span>
        </button>
        <button :class="{active: section==='pinned'}" class="seg" @click="section='pinned'">
          Pinned routes <span class="count">{{ ourRoutes.length }}</span>
        </button>
      </div>
      <span class="muted" style="font-size: 11px">
        refreshing every 5s · {{ lastRefresh ? lastRefresh.toLocaleTimeString() : '—' }}
      </span>
    </div>

    <div v-show="section==='interfaces'">
      <table>
        <thead>
          <tr><th>Name</th><th>Owner</th><th>Index</th><th>MTU</th><th>Flags</th></tr>
        </thead>
        <tbody>
          <tr v-for="i in interfaces" :key="i.name">
            <td>
              <code>{{ i.name }}</code>
              <span v-if="isTunnel(i.name)" class="tag tag-route" style="margin-left: 8px; font-size: 10px">tunnel</span>
            </td>
            <td>
              <span v-if="i.owner" class="tag tag-route">{{ i.owner }}</span>
              <span v-else class="muted">—</span>
            </td>
            <td class="muted">{{ i.index }}</td>
            <td class="muted">{{ i.mtu }}</td>
            <td class="muted" style="font-size: 11px">{{ i.flags }}</td>
          </tr>
          <tr v-if="!interfaces.length">
            <td colspan="5" class="muted" style="text-align:center; padding: 24px">No active interfaces.</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-show="section==='routes'">
      <table>
        <thead>
          <tr><th>Family</th><th>Destination</th><th>Gateway</th><th>Flags</th><th>Interface</th></tr>
        </thead>
        <tbody>
          <tr v-for="(r, i) in sysRoutes" :key="r.family + ':' + r.destination + ':' + r.interface + ':' + i">
            <td class="muted">{{ r.family }}</td>
            <td><code>{{ r.destination }}</code></td>
            <td><code>{{ r.gateway }}</code></td>
            <td class="muted" style="font-size: 11px">{{ r.flags }}</td>
            <td><code>{{ r.interface }}</code></td>
          </tr>
          <tr v-if="!sysRoutes.length">
            <td colspan="5" class="muted" style="text-align:center; padding: 24px">No routes.</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-show="section==='pinned'">
      <p class="muted" style="margin-top: 0">
        Per-host routes em-wall installed for <code>allow → interface</code> rules.
        These come from DNS answers and live until their TTL expires.
      </p>
      <table>
        <thead>
          <tr><th>Host</th><th>Interface</th><th>Rule</th><th>Expires</th></tr>
        </thead>
        <tbody>
          <tr v-for="r in ourRoutes" :key="r.host">
            <td><code>{{ r.host }}</code></td>
            <td><span class="tag tag-route">{{ r.interface }}</span></td>
            <td class="muted">#{{ r.ruleId }}</td>
            <td class="muted">{{ fmtTime(r.expiresAt) }}</td>
          </tr>
          <tr v-if="!ourRoutes.length">
            <td colspan="4" class="muted" style="text-align:center; padding: 24px">
              No pinned routes — em-wall hasn't installed any yet.
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
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
.count {
  display: inline-block;
  margin-left: 6px;
  padding: 1px 6px;
  background: var(--border);
  border-radius: 8px;
  font-size: 10px;
  color: var(--text-dim);
}
.seg.active .count { background: var(--accent); color: #fff; }
</style>
