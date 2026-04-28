<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import {
  GetSetting, SetSetting,
  SystemDNSStatus, ActivateSystemDNS, DeactivateSystemDNS,
} from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';

const emit = defineEmits<{ (e: 'changed'): void }>();

const blockEncrypted = ref<boolean>(false);
const sysStatus = ref<ipc.SystemDNSStatus | null>(null);
const error = ref<string>('');
const busy = ref<boolean>(false);
const lastRefresh = ref<Date | null>(null);
let timer: number | undefined;

async function loadSettings() {
  try {
    blockEncrypted.value = (await GetSetting('block_encrypted_dns', 'false')) === 'true';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
}

async function refreshStatus() {
  try {
    sysStatus.value = await SystemDNSStatus();
    lastRefresh.value = new Date();
    if (!busy.value) error.value = '';
  } catch (e: any) {
    if (!busy.value) error.value = e?.message || String(e);
  }
}

async function toggleEncrypted() {
  busy.value = true;
  try {
    blockEncrypted.value = !blockEncrypted.value;
    await SetSetting('block_encrypted_dns', String(blockEncrypted.value));
    error.value = '';
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
    blockEncrypted.value = !blockEncrypted.value;
  } finally {
    busy.value = false;
  }
}

async function activate() {
  busy.value = true;
  error.value = '';
  try {
    sysStatus.value = await ActivateSystemDNS();
    lastRefresh.value = new Date();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function deactivate() {
  busy.value = true;
  error.value = '';
  try {
    sysStatus.value = await DeactivateSystemDNS();
    lastRefresh.value = new Date();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

onMounted(() => {
  loadSettings();
  refreshStatus();
  timer = window.setInterval(refreshStatus, 1500);
});
onUnmounted(() => { if (timer) window.clearInterval(timer); });
</script>

<template>
  <div class="panel">
    <h2>Settings</h2>
    <div v-if="error" class="error">{{ error }}</div>

    <div class="col" style="max-width: 820px; gap: 20px">

      <!-- Hijack panel -->
      <div class="col" style="gap: 10px; padding: 16px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
        <div class="row" style="justify-content: space-between">
          <div class="col" style="gap: 2px">
            <strong>System DNS hijack</strong>
            <span class="muted" style="font-size: 11px">
              Refreshes every 1.5s ·
              {{ lastRefresh ? `last update ${lastRefresh.toLocaleTimeString()}` : '—' }}
            </span>
          </div>
          <span :class="sysStatus?.active ? 'tag tag-route' : 'tag tag-off'">
            {{ sysStatus?.active ? 'ACTIVE' : 'inactive' }}
          </span>
        </div>

        <span class="muted" style="font-size: 12px">
          When active, every enabled service uses <code>127.0.0.1</code> as DNS
          so this firewall sees every query. Original settings are saved before
          activation and restored on deactivate.
        </span>
        <span class="muted" style="font-size: 11px; color: var(--warn)">
          ⚠ Limitation: VPN apps that push their own DNS via NetworkExtension
          (v2box, Tailscale, etc.) bypass this hijack while connected — those
          queries never reach the daemon, so <strong>no rules apply and no log
          entries appear for them</strong>. If your Logs tab is empty (or
          missing entries for domains you visited), set the VPN app's DNS
          upstream to <code>127.0.0.1</code> in the VPN's own settings.
        </span>

        <div v-if="sysStatus" class="col" style="gap: 6px; margin-top: 8px; padding-top: 10px; border-top: 1px solid var(--border)">
          <div class="row" style="gap: 12px"><span class="label muted" style="min-width: 140px">Upstream (validated)</span>
            <code v-if="sysStatus.upstream && sysStatus.upstream.length">{{ sysStatus.upstream.join('  •  ') }}</code>
            <span v-else class="muted" style="color: var(--warn)">none — daemon cannot resolve</span>
          </div>
          <div class="row" style="gap: 12px"><span class="label muted" style="min-width: 140px">Kernel sees</span>
            <code v-if="sysStatus.detectedResolvers && sysStatus.detectedResolvers.length">{{ sysStatus.detectedResolvers.join('  •  ') }}</code>
            <span v-else class="muted">—</span>
          </div>
          <div v-for="(ips, svc) in (sysStatus.perService || {})" :key="svc"
               class="row" style="gap: 12px">
            <span class="label muted" style="min-width: 140px">{{ svc }}</span>
            <code v-if="ips && ips.length">{{ ips.join('  •  ') }}</code>
            <span v-else class="muted">DHCP-supplied</span>
          </div>
        </div>

        <div class="row" style="margin-top: 8px; gap: 10px">
          <button class="primary" :disabled="busy || sysStatus?.active" @click="activate">
            {{ busy ? '…' : 'Activate' }}
          </button>
          <button :disabled="busy || !sysStatus?.active" @click="deactivate">
            {{ busy ? '…' : 'Deactivate' }}
          </button>
        </div>
      </div>

      <!-- Encrypted DNS toggle -->
      <div class="col" style="gap: 8px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
        <label class="row" style="justify-content: space-between">
          <div class="col" style="gap: 4px">
            <strong>Block encrypted DNS (DoH/DoT)</strong>
            <span class="muted" style="font-size: 12px">
              Drops TCP/853 (DoT) and TCP/443 to known DoH endpoints. Forces
              apps that use encrypted DNS — like Chrome's Secure DNS — to fall
              back to the system resolver, so this firewall can see them.
            </span>
            <span class="muted" style="font-size: 11px">
              Trade-off: your queries become visible to your ISP again.
            </span>
          </div>
          <label class="toggle" @click.prevent="toggleEncrypted">
            <input type="checkbox" :checked="blockEncrypted" :disabled="busy" />
            <span class="track"></span>
          </label>
        </label>
      </div>

    </div>
  </div>
</template>
