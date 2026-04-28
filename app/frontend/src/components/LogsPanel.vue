<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import { RecentLogs, ActiveRoutes, SystemRoutes, Interfaces } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

const logs = ref<ipc.LogDTO[]>([]);
const ourRoutes = ref<ipc.ActiveRouteDTO[]>([]);
const sysRoutes = ref<ipc.SystemRouteDTO[]>([]);
const interfaces = ref<ipc.InterfaceDTO[]>([]);
const error = ref<string>('');
const lastRefresh = ref<Date | null>(null);

let logsTimer: number | undefined;
let networkTimer: number | undefined;

async function refreshLogs() {
  try {
    logs.value = (await RecentLogs(200)) || [];
    ourRoutes.value = (await ActiveRoutes()) || [];
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function refreshNetwork() {
  try {
    sysRoutes.value = (await SystemRoutes()) || [];
    interfaces.value = (await Interfaces()) || [];
    lastRefresh.value = new Date();
  } catch (e: any) {
    // Don't clobber log errors with network errors.
  }
}

function fmtTime(s: string): string {
  try { return new Date(s).toLocaleTimeString(); } catch { return s; }
}

function actionTag(action: string): string {
  if (action === 'block') return 'tag tag-block';
  if (action === 'route') return 'tag tag-route';
  if (action === 'block-iface-down') return 'tag tag-block';
  return 'tag tag-off';
}

onMounted(() => {
  refreshLogs();
  refreshNetwork();
  logsTimer = window.setInterval(refreshLogs, 1000);
  networkTimer = window.setInterval(refreshNetwork, 5000);
});
onUnmounted(() => {
  if (logsTimer) window.clearInterval(logsTimer);
  if (networkTimer) window.clearInterval(networkTimer);
});
</script>

<template>
  <div class="panel">
    <div v-if="error" class="error">{{ error }}</div>

    <h2>Network interfaces
      <span class="muted" style="font-weight:400; font-size: 12px">
        ({{ interfaces.length }}) · refreshed every 5s
        <span v-if="lastRefresh"> · last {{ lastRefresh.toLocaleTimeString() }}</span>
      </span>
    </h2>
    <table v-if="interfaces.length">
      <thead>
        <tr><th>Name</th><th>Owner</th><th>Index</th><th>MTU</th><th>Flags</th></tr>
      </thead>
      <tbody>
        <tr v-for="i in interfaces" :key="i.name">
          <td><code>{{ i.name }}</code></td>
          <td>
            <span v-if="i.owner" class="tag tag-route">{{ i.owner }}</span>
            <span v-else class="muted">—</span>
          </td>
          <td class="muted">{{ i.index }}</td>
          <td class="muted">{{ i.mtu }}</td>
          <td class="muted" style="font-size: 11px">{{ i.flags }}</td>
        </tr>
      </tbody>
    </table>

    <h2 style="margin-top:24px">System routes
      <span class="muted" style="font-weight:400; font-size: 12px">({{ sysRoutes.length }})</span>
    </h2>
    <table v-if="sysRoutes.length">
      <thead>
        <tr><th>Family</th><th>Destination</th><th>Gateway</th><th>Flags</th><th>Interface</th></tr>
      </thead>
      <tbody>
        <tr v-for="(r, i) in sysRoutes" :key="r.family + ':' + r.destination + ':' + i">
          <td class="muted">{{ r.family }}</td>
          <td><code>{{ r.destination }}</code></td>
          <td><code>{{ r.gateway }}</code></td>
          <td class="muted" style="font-size: 11px">{{ r.flags }}</td>
          <td><code>{{ r.interface }}</code></td>
        </tr>
      </tbody>
    </table>

    <h2 style="margin-top:24px">Active per-host routes (installed by em-wall)
      <span class="muted" style="font-weight:400; font-size: 12px">({{ ourRoutes.length }})</span>
    </h2>
    <table v-if="ourRoutes.length">
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
      </tbody>
    </table>
    <div v-else class="muted">No per-host routes installed by em-wall yet.</div>

    <h2 style="margin-top:24px">Recent decisions
      <span class="muted" style="font-weight:400; font-size: 12px">({{ logs.length }}) · live</span>
    </h2>
    <table>
      <thead>
        <tr><th>Time</th><th>Domain</th><th>Action</th><th>Interface</th><th>Client</th></tr>
      </thead>
      <tbody>
        <tr v-for="l in logs" :key="l.id">
          <td class="muted">{{ fmtTime(l.timestamp) }}</td>
          <td><code>{{ l.queryName }}</code></td>
          <td>
            <span :class="actionTag(l.action)">{{ l.action }}</span>
          </td>
          <td class="muted">{{ l.interface || '—' }}</td>
          <td class="muted">{{ l.clientIp || '—' }}</td>
        </tr>
        <tr v-if="!logs.length">
          <td colspan="5" class="muted" style="text-align:center; padding:24px">
            No decisions logged yet. Plain allows are not logged.
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
