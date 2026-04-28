<script lang="ts" setup>
import { ref, onMounted, onUnmounted } from 'vue';
import StatusBar from './components/StatusBar.vue';
import RulesPanel from './components/RulesPanel.vue';
import LogsPanel from './components/LogsPanel.vue';
import NetworkPanel from './components/NetworkPanel.vue';
import SettingsPanel from './components/SettingsPanel.vue';
import { Status } from '../wailsjs/go/main/App';
import type { ipc } from '../wailsjs/go/models';

type Tab = 'rules' | 'logs' | 'network' | 'settings';
const tab = ref<Tab>('rules');
const status = ref<ipc.StatusResult | null>(null);
const error = ref<string>('');
let timer: number | undefined;

async function refreshStatus() {
  try {
    status.value = await Status();
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
    status.value = null;
  }
}

onMounted(() => {
  refreshStatus();
  timer = window.setInterval(refreshStatus, 2000);
});
onUnmounted(() => {
  if (timer) window.clearInterval(timer);
});
</script>

<template>
  <StatusBar :status="status" :error="error" />

  <div class="tabs">
    <button :class="{active: tab==='rules'}"    @click="tab='rules'">Rules</button>
    <button :class="{active: tab==='logs'}"     @click="tab='logs'">Logs</button>
    <button :class="{active: tab==='network'}"  @click="tab='network'">Network</button>
    <button :class="{active: tab==='settings'}" @click="tab='settings'">Settings</button>
  </div>

  <RulesPanel    v-if="tab==='rules'"    @changed="refreshStatus" />
  <LogsPanel     v-else-if="tab==='logs'" />
  <NetworkPanel  v-else-if="tab==='network'" />
  <SettingsPanel v-else                  @changed="refreshStatus" />
</template>
