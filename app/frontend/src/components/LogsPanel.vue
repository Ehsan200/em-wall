<script lang="ts" setup>
import { ref } from 'vue';
import DnsLogView from './DnsLogView.vue';
import TextLogView from './TextLogView.vue';
import { onTourView } from '../tour';

// DNS decisions come from the rules DB; the other views tail the daemon's
// and xray's text log files. Each view owns its own poll, and v-if
// unmounts the hidden ones so only the visible view polls.
type View = 'dns' | 'daemon' | 'xray-error' | 'xray-access';
const view = ref<View>('dns');
onTourView('logs', (v) => { view.value = v as View; });
</script>

<template>
  <div class="panel">
    <div class="row" style="gap: 4px; padding: 0; border-bottom: 1px solid var(--border); margin: 0 -2px 12px" data-tour="logs-tabs">
      <button class="subtab" :class="{ active: view === 'dns' }" @click="view = 'dns'">DNS decisions</button>
      <button class="subtab" :class="{ active: view === 'daemon' }" @click="view = 'daemon'">Daemon</button>
      <button class="subtab" :class="{ active: view === 'xray-error' }" @click="view = 'xray-error'">Xray errors</button>
      <button class="subtab" :class="{ active: view === 'xray-access' }" @click="view = 'xray-access'">Xray access</button>
    </div>
    <DnsLogView v-if="view === 'dns'" />
    <TextLogView v-else :key="view" :source="view" />
  </div>
</template>

<style scoped>
.subtab {
  background: transparent;
  border: none;
  border-bottom: 2px solid transparent;
  border-radius: 0;
  padding: 8px 14px;
  color: var(--text-dim);
  font-size: 12px;
  cursor: pointer;
}
.subtab:hover { color: var(--text); }
.subtab.active {
  color: var(--text);
  border-bottom-color: var(--accent);
}
</style>
