<script lang="ts" setup>
import { ref, onMounted } from 'vue';
import { GetSetting, SetSetting } from '../../wailsjs/go/main/App';

// The daemon's update reminder for when the app isn't running (daemon/
// update_notify.go). Self-contained; mount it anywhere.
const enabled = ref(true);
const loaded = ref(false);
const busy = ref(false);
const error = ref('');

async function load() {
  try {
    enabled.value = (await GetSetting('update_notify', 'true')) !== 'false';
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  }
  loaded.value = true;
}

async function toggle() {
  if (busy.value || !loaded.value) return;
  busy.value = true;
  const next = !enabled.value;
  try {
    await SetSetting('update_notify', String(next));
    enabled.value = next;
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

onMounted(load);
</script>

<template>
  <div data-tour="settings-updates" class="col" style="gap: 8px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
    <label class="row" style="justify-content: space-between; gap: 16px">
      <div class="col" style="gap: 4px">
        <strong>Update reminders when the app isn’t open</strong>
        <span class="muted" style="font-size: 12px; line-height: 1.5">
          em-wall keeps running in the background after you quit the app, and checks for a
          new version every few hours. When one is out and the app isn’t open,
          it asks once with an <b>Open em-wall</b> button, which opens the app
          so you can install it. <b>Later</b> asks again the next day.
        </span>
      </div>
      <label class="toggle" :class="{ stale: !loaded }" @click.prevent="toggle">
        <input type="checkbox" :checked="enabled" :disabled="busy || !loaded" />
        <span class="track"></span>
      </label>
    </label>
    <div v-if="error" class="error" style="margin: 0">{{ error }}</div>
  </div>
</template>
