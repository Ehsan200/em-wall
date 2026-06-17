<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import StatusBar from './components/StatusBar.vue';
import RulesPanel from './components/RulesPanel.vue';
import LogsPanel from './components/LogsPanel.vue';
import DashboardPanel from './components/DashboardPanel.vue';
import NetworkPanel from './components/NetworkPanel.vue';
import ProxiesPanel from './components/ProxiesPanel.vue';
import XrayPanel from './components/XrayPanel.vue';
import SettingsPanel from './components/SettingsPanel.vue';
import InstallPanel from './components/InstallPanel.vue';
import { Status, InstallStatus, IsPackaged, AppVersion, CheckForUpdate, DownloadAndApplyUpdate, PublicIP } from '../wailsjs/go/main/App';
import { BrowserOpenURL, EventsOn, EventsOff } from '../wailsjs/runtime/runtime';
import type { ipc, installer, main } from '../wailsjs/go/models';

type Tab = 'rules' | 'logs' | 'usage' | 'network' | 'proxies' | 'xray' | 'settings';
const tab = ref<Tab>('rules');
const status = ref<ipc.StatusResult | null>(null);
const error = ref<string>('');
const appVersion = ref<string>('');
const install = ref<installer.Status | null>(null);
const packaged = ref<boolean>(true);
// True while Settings → Reinstall is mid-flight. Reinstall bootouts the
// LaunchDaemon and re-bootstraps it; in between, our 2s poll observes
// daemonRunning=false and would flip the whole UI back to the install
// gate, losing the user's context inside Settings. Suppressing the
// gate while this flag is set keeps them on the Settings tab end to
// end. SettingsPanel is the only emitter — it sets true before
// calling Install() and clears it after WaitForDaemon returns.
const reinstalling = ref<boolean>(false);
const updateInfo = ref<main.UpdateInfo | null>(null);
const updateDismissed = ref(false);
// Self-update download state. `updating` gates the banner buttons while
// the .dmg streams down; `updateProgress` (0..1) drives the bar. On
// success the app quits and relaunches itself, so these never reset —
// the process is gone. `updateError` surfaces a pre-handoff failure
// (download/mount) where the running app is left untouched.
const updating = ref(false);
const updateProgress = ref(0);
const updateError = ref('');

async function startUpdate() {
  if (!updateInfo.value) return;
  updateError.value = '';
  updateProgress.value = 0;
  updating.value = true;
  try {
    // Resolves to nothing: the app quits on success. A thrown error
    // means the download or mount failed before any swap — safe to retry.
    await DownloadAndApplyUpdate(updateInfo.value.downloadUrl);
  } catch (e: any) {
    updateError.value = e?.message || String(e);
    updating.value = false;
  }
}
// Public egress identity (default-route IP + geo). Polled on a slow
// cadence — the daemon caches the ip-api.com probe for a few minutes,
// and a public IP rarely changes — so this stays cheap.
const egress = ref<ipc.ExitIPResult | null>(null);
let egressTimer: number | undefined;
// Bumped each time the user clicks "Reinstall" in the compatibility
// banner. SettingsPanel watches it to scroll the Reinstall card into
// view and flash it — a counter (not a boolean) so repeated clicks
// re-trigger without needing a reset handshake.
const focusReinstallSeq = ref<number>(0);
let timer: number | undefined;

// The "dev" sentinel is what an unversioned build reports (core/version
// default, used by `wails dev` / `make run-*`). Never flag compatibility
// against it.
const DEV_VERSION = 'dev';
const daemonVersion = computed(() => status.value?.version || '');

// The daemon binary and this app are built from the same core/version in
// a single `make app-bundle`, so a version mismatch means the on-disk
// daemon is stale — the app was updated but the daemon was never
// reinstalled. Such a daemon may be missing IPC methods this app calls
// (surfacing as "unknown method" errors deeper in the UI). We compare
// exact strings, and skip the check whenever either side is "dev" (the
// normal dev loop, or a dev build deliberately pointed at an installed
// daemon). status is non-null only when the daemon answered Status(), so
// this is implicitly false while the install gate is up.
const daemonIncompatible = computed(() => {
  const app = appVersion.value;
  const dae = daemonVersion.value;
  if (!app || !dae) return false;
  if (app === DEV_VERSION || dae === DEV_VERSION) return false;
  return app !== dae;
});

function goToReinstall() {
  tab.value = 'settings';
  focusReinstallSeq.value++;
}

// Show the install gate when:
//  - this is a packaged build (so install can actually do anything), AND
//  - we're not currently in the middle of a Reinstall (transient
//    daemon-down windows shouldn't kick the user out of Settings), AND
//  - the daemon isn't fully installed and running.
// The dev path (unpackaged build) skips the gate so devs can hit a
// separately-running daemon — they're expected to know what they're doing.
const showInstallGate = computed(() =>
  packaged.value && !reinstalling.value &&
  (!install.value || !install.value.daemonRunning || !install.value.binaryPresent || !install.value.plistPresent)
);

async function refresh() {
  // installStatus is local to the UI process — never fails.
  try { install.value = await InstallStatus(); } catch { /* ignore */ }

  // Status() goes over IPC and may fail when daemon isn't running yet.
  try {
    status.value = await Status();
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
    status.value = null;
  }
}

async function refreshEgress() {
  let probed = false;
  try {
    const r = await PublicIP();
    egress.value = r;
    probed = !!r?.probed;
  } catch { /* daemon may be down / starting; leave last value */ }
  // Self-scheduling: retry quickly until we have a real exit IP (covers
  // daemon-still-starting and transient probe failures), then back off
  // to a slow poll since a public IP rarely changes.
  if (egressTimer) window.clearTimeout(egressTimer);
  egressTimer = window.setTimeout(refreshEgress, probed ? 120000 : 12000);
}

async function onInstalled() {
  // Force an immediate refresh so the UI flips to the regular tabs.
  await refresh();
}

onMounted(async () => {
  try { packaged.value = await IsPackaged(); } catch { packaged.value = false; }
  try { appVersion.value = await AppVersion(); } catch { appVersion.value = ''; }
  await refresh();
  timer = window.setInterval(refresh, 2000);
  refreshEgress(); // self-schedules its own next run
  // Check for updates once on startup; runs in background so it never
  // delays the initial UI render.
  CheckForUpdate().then(info => { if (info.hasUpdate) updateInfo.value = info; }).catch(() => {});
  EventsOn('update:progress', (p: number) => { updateProgress.value = p; });
});
onUnmounted(() => {
  if (timer) window.clearInterval(timer);
  if (egressTimer) window.clearTimeout(egressTimer);
  EventsOff('update:progress');
});
</script>

<template>
  <StatusBar :status="status" :error="error" :appVersion="appVersion"
             :incompatible="daemonIncompatible" :daemonVersion="daemonVersion"
             :egress="egress" @refresh-egress="refreshEgress" />

  <div v-if="daemonIncompatible && !reinstalling" class="compat-banner">
    <div class="col" style="gap: 2px">
      <strong>Daemon version mismatch — reinstall required</strong>
      <span style="font-size: 12px; opacity: 0.9">
        The installed daemon is <code>v{{ daemonVersion }}</code> but this app is
        <code>v{{ appVersion }}</code>. They must match — a daemon out of step with
        the app may not understand its requests, so some features will fail until
        you reinstall it from this app.
      </span>
    </div>
    <button class="primary" @click="goToReinstall">Reinstall daemon</button>
  </div>

  <div v-if="updateInfo && !updateDismissed" class="update-banner">
    <span v-if="!updating">
      Update available: <strong>{{ updateInfo.version }}</strong>
      <span v-if="updateError" style="color: #c0392b; font-size: 12px; margin-left: 8px">{{ updateError }}</span>
    </span>
    <span v-else>
      Downloading {{ updateInfo.version }}… {{ Math.round(updateProgress * 100) }}%
      — the app will restart automatically.
    </span>
    <div v-if="!updating" class="row" style="gap: 8px">
      <button v-if="updateInfo.downloadUrl" class="primary" @click="startUpdate">Download &amp; install</button>
      <button :class="{ primary: !updateInfo.downloadUrl }" @click="BrowserOpenURL(updateInfo!.url)">View release</button>
      <button @click="updateDismissed = true">Dismiss</button>
    </div>
    <div v-else class="update-progress">
      <div class="update-progress-fill" :style="{ width: (updateProgress * 100) + '%' }"></div>
    </div>
  </div>

  <template v-if="showInstallGate">
    <InstallPanel :status="install" :packaged="packaged" @installed="onInstalled" />
  </template>
  <template v-else>
    <div class="tabs">
      <button :class="{active: tab==='rules'}"    @click="tab='rules'">Rules</button>
      <button :class="{active: tab==='logs'}"     @click="tab='logs'">Logs</button>
      <button :class="{active: tab==='usage'}"    @click="tab='usage'">Usage</button>
      <button :class="{active: tab==='network'}"  @click="tab='network'">Network</button>
      <button :class="{active: tab==='proxies'}"  @click="tab='proxies'">Proxies</button>
      <button :class="{active: tab==='xray'}"     @click="tab='xray'">Xray</button>
      <button :class="{active: tab==='settings'}" @click="tab='settings'">Settings</button>
    </div>

    <RulesPanel    v-if="tab==='rules'"    @changed="refresh" />
    <LogsPanel     v-else-if="tab==='logs'" />
    <DashboardPanel v-else-if="tab==='usage'" />
    <NetworkPanel  v-else-if="tab==='network'" />
    <ProxiesPanel  v-else-if="tab==='proxies'" />
    <XrayPanel     v-else-if="tab==='xray'" />
    <SettingsPanel v-else
                   :focusReinstallSeq="focusReinstallSeq"
                   @changed="refresh"
                   @reinstalling="(v: boolean) => reinstalling = v" />
  </template>
</template>
