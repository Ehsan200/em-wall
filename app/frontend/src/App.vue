<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import StatusBar from './components/StatusBar.vue';
import RulesPanel from './components/RulesPanel.vue';
import LogsPanel from './components/LogsPanel.vue';
import NetworkPanel from './components/NetworkPanel.vue';
import SettingsPanel from './components/SettingsPanel.vue';
import InstallPanel from './components/InstallPanel.vue';
import { Status, InstallStatus, IsPackaged } from '../wailsjs/go/main/App';
import type { ipc, installer } from '../wailsjs/go/models';

type Tab = 'rules' | 'logs' | 'network' | 'settings';
const tab = ref<Tab>('rules');
const status = ref<ipc.StatusResult | null>(null);
const error = ref<string>('');
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
let timer: number | undefined;

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

async function onInstalled() {
  // Force an immediate refresh so the UI flips to the regular tabs.
  await refresh();
}

onMounted(async () => {
  try { packaged.value = await IsPackaged(); } catch { packaged.value = false; }
  await refresh();
  timer = window.setInterval(refresh, 2000);
});
onUnmounted(() => {
  if (timer) window.clearInterval(timer);
});
</script>

<template>
  <StatusBar :status="status" :error="error" />

  <template v-if="showInstallGate">
    <InstallPanel :status="install" :packaged="packaged" @installed="onInstalled" />
  </template>
  <template v-else>
    <div class="tabs">
      <button :class="{active: tab==='rules'}"    @click="tab='rules'">Rules</button>
      <button :class="{active: tab==='logs'}"     @click="tab='logs'">Logs</button>
      <button :class="{active: tab==='network'}"  @click="tab='network'">Network</button>
      <button :class="{active: tab==='settings'}" @click="tab='settings'">Settings</button>
    </div>

    <RulesPanel    v-if="tab==='rules'"    @changed="refresh" />
    <LogsPanel     v-else-if="tab==='logs'" />
    <NetworkPanel  v-else-if="tab==='network'" />
    <SettingsPanel v-else
                   @changed="refresh"
                   @reinstalling="(v: boolean) => reinstalling = v" />
  </template>
</template>
