<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import {
  GetSetting, SetSetting,
  SystemDNSStatus, ActivateSystemDNS, DeactivateSystemDNS,
  InstallStatus, Uninstall,
} from '../../wailsjs/go/main/App';
import type { ipc, installer } from '../../wailsjs/go/models';

const emit = defineEmits<{ (e: 'changed'): void }>();

const blockEncrypted = ref<boolean>(false);
const sysStatus = ref<ipc.SystemDNSStatus | null>(null);
const error = ref<string>('');
const busy = ref<boolean>(false);
const lastRefresh = ref<Date | null>(null);
let timer: number | undefined;

// ---- Uninstall flow state ----
const installStatus = ref<installer.Status | null>(null);
const uninstallExpanded = ref<boolean>(false);
const purgeData = ref<boolean>(false);
const confirmText = ref<string>('');
const uninstallError = ref<string>('');
const uninstallBusy = ref<boolean>(false);

// The DNS hijack is auto-deactivated by App.Uninstall before the
// daemon is torn down, so we just show a hint when it's currently on
// — the user doesn't have to do anything manually.
const dnsActiveHint = computed(() => sysStatus.value?.active === true);
const confirmRequired = computed(() => purgeData.value ? 'delete everything' : 'uninstall');
const canUninstall = computed(() =>
  !uninstallBusy.value
  && confirmText.value.trim() === confirmRequired.value
);

function bytes(n: number): string {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB'];
  let i = 0; let v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i ? 1 : 0)} ${u[i]}`;
}

async function loadInstallStatus() {
  try { installStatus.value = await InstallStatus(); } catch { /* ignore */ }
}

async function doUninstall() {
  if (!canUninstall.value) return;
  uninstallBusy.value = true;
  uninstallError.value = '';
  try {
    await Uninstall(purgeData.value);
    // The whole UI is about to be unusable (no daemon, no socket). Reset
    // local state so the tab grid disappears and the install gate shows
    // up on the next App.vue refresh tick.
    confirmText.value = '';
    purgeData.value = false;
    uninstallExpanded.value = false;
    emit('changed');
  } catch (e: any) {
    const msg = e?.message || String(e);
    if (msg !== 'cancelled') uninstallError.value = msg;
  } finally {
    uninstallBusy.value = false;
    await loadInstallStatus();
  }
}

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
  loadInstallStatus();
  timer = window.setInterval(() => {
    refreshStatus();
    loadInstallStatus();
  }, 1500);
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

      <!-- Uninstall (danger zone) -->
      <div class="col" style="gap: 10px; padding: 14px; background: var(--panel); border: 1px solid rgba(255, 111, 111, 0.3); border-radius: 8px">
        <div class="row" style="justify-content: space-between; cursor: pointer" @click="uninstallExpanded = !uninstallExpanded">
          <strong style="color: var(--danger)">Uninstall em-wall</strong>
          <span class="muted" style="font-size: 11px">{{ uninstallExpanded ? '▾' : '▸' }}</span>
        </div>

        <template v-if="uninstallExpanded">
          <span class="muted" style="font-size: 12px; line-height: 1.55">
            Stops the daemon, removes <code>/usr/local/bin/em-walld</code>, the
            LaunchDaemon plist, and the pf anchor. Reverts the
            <code>anchor "em-wall"</code> line in <code>/etc/pf.conf</code>
            (a backup is written next to it). macOS will ask for your
            password.
          </span>
          <span class="muted" style="font-size: 11px; line-height: 1.5">
            <strong>Run this before dragging the app to Trash.</strong> The
            daemon binary and LaunchDaemon plist live outside the .app
            bundle, so deleting the app alone leaves a privileged process
            running with no UI to control it.
          </span>

          <!-- DNS hijack will be auto-deactivated; just a heads-up. -->
          <div v-if="dnsActiveHint"
               class="col" style="gap: 4px; padding: 10px 12px; background: rgba(255, 200, 110, 0.08); border: 1px solid rgba(255, 200, 110, 0.3); border-radius: 6px">
            <strong style="font-size: 12px">DNS hijack will be deactivated</strong>
            <span class="muted" style="font-size: 11px; line-height: 1.5">
              Every active network service currently has <code>127.0.0.1</code>
              as its DNS server. The uninstall will ask the daemon to restore
              the original DNS settings before removing it — your network
              shouldn't drop.
            </span>
          </div>

          <!-- What's about to disappear -->
          <div v-if="installStatus" class="col" style="gap: 3px; padding: 10px 12px; background: var(--bg-2, rgba(255,255,255,0.03)); border: 1px solid var(--border); border-radius: 6px">
            <div class="muted" style="font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px">Will be removed</div>
            <div class="row" v-if="installStatus.binaryPresent" style="gap: 8px"><code>/usr/local/bin/em-walld</code></div>
            <div class="row" v-if="installStatus.plistPresent" style="gap: 8px"><code>/Library/LaunchDaemons/com.em-wall.daemon.plist</code></div>
            <div class="row" style="gap: 8px"><code>/etc/pf.anchors/em-wall</code></div>

            <div class="muted" style="font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px; margin-top: 8px">User data</div>
            <div class="row" style="gap: 8px">
              <code>/usr/local/var/em-wall/rules.db</code>
              <span class="muted" style="font-size: 11px">
                ({{ installStatus.dbExists ? bytes(installStatus.dbSizeBytes) : 'absent' }})
              </span>
            </div>
            <div class="row" style="gap: 8px">
              <code>/usr/local/var/log/em-wall.log</code>
              <span class="muted" style="font-size: 11px">
                ({{ installStatus.logSizeBytes ? bytes(installStatus.logSizeBytes) : 'absent' }})
              </span>
            </div>
          </div>

          <!-- Purge toggle -->
          <label class="row" style="gap: 10px; align-items: flex-start">
            <input type="checkbox" v-model="purgeData" :disabled="uninstallBusy" style="margin-top: 3px" />
            <div class="col" style="gap: 2px">
              <span style="font-size: 13px"><strong>Also delete my rules and logs</strong></span>
              <span class="muted" style="font-size: 11px">
                Off by default — keeping the DB lets a future re-install
                pick up where you left off.
              </span>
            </div>
          </label>

          <!-- Typed-confirmation gate -->
          <div class="col" style="gap: 6px">
            <label class="muted" style="font-size: 11px">
              Type <code>{{ confirmRequired }}</code> to confirm:
            </label>
            <input type="text" v-model="confirmText"
                   :disabled="uninstallBusy"
                   :placeholder="confirmRequired"
                   style="padding: 6px 8px; background: var(--bg); border: 1px solid var(--border); border-radius: 4px; color: var(--text); font-size: 13px" />
          </div>

          <div v-if="uninstallError" class="error">{{ uninstallError }}</div>

          <div class="row" style="gap: 10px">
            <button :disabled="!canUninstall" @click="doUninstall"
                    style="background: var(--danger); color: white; border-color: var(--danger)">
              {{ uninstallBusy ? 'Uninstalling…' : (purgeData ? 'Uninstall and delete data' : 'Uninstall (keep data)') }}
            </button>
            <span class="muted" style="font-size: 11px">macOS will prompt for your password.</span>
          </div>
        </template>
      </div>

    </div>
  </div>
</template>
