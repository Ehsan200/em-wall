<script lang="ts" setup>
import { ref, onMounted } from 'vue';
import {
  GetSetting, SetSetting, SuggestDismissed, SuggestUndismiss,
} from '../../wailsjs/go/main/App';

// Settings → Suggestions: turn group suggestions on/off and bring back
// tags dismissed with "Never". Self-contained; mount it anywhere.
const enabled = ref(true);
const loaded = ref(false);
const busy = ref(false);
const dismissed = ref<string[]>([]);
const restoring = ref<string | null>(null);
const error = ref('');

async function load() {
  try {
    const [v, list] = await Promise.all([
      GetSetting('suggest_enabled', 'true'),
      SuggestDismissed(),
    ]);
    enabled.value = v !== 'false';
    dismissed.value = list || [];
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
    await SetSetting('suggest_enabled', String(next));
    enabled.value = next;
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function restore(tag: string) {
  restoring.value = tag;
  try {
    await SuggestUndismiss(tag);
    dismissed.value = dismissed.value.filter(t => t !== tag);
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    restoring.value = null;
  }
}

onMounted(load);
</script>

<template>
  <div class="col" style="gap: 10px; padding: 14px; background: var(--panel); border: 1px solid var(--border); border-radius: 8px">
    <label class="row" style="justify-content: space-between; gap: 16px">
      <div class="col" style="gap: 4px">
        <strong>Group suggestions</strong>
        <span class="muted" style="font-size: 12px; line-height: 1.5">
          Suggests a group when you visit a site that no rule covers yet,
          using the geosite list. Matching happens on this Mac; nothing is
          sent anywhere. Suggestions never create rules on their own.
        </span>
      </div>
      <label class="toggle" :class="{ stale: !loaded }" @click.prevent="toggle">
        <input type="checkbox" :checked="enabled" :disabled="busy || !loaded" />
        <span class="track"></span>
      </label>
    </label>

    <div class="col" style="gap: 6px">
      <span class="muted" style="font-size: 11px">
        Hidden for good{{ dismissed.length ? ` (${dismissed.length})` : '' }}
      </span>
      <div v-if="dismissed.length" class="row" style="gap: 6px; flex-wrap: wrap">
        <span v-for="t in dismissed" :key="t" class="dismissed-chip">
          <code>{{ t }}</code>
          <button class="link-btn" :disabled="restoring === t" @click="restore(t)"
                  :title="`Show suggestions for ${t} again`">
            {{ restoring === t ? 'Restoring…' : 'Undo' }}
          </button>
        </span>
      </div>
      <span v-else-if="loaded" class="muted" style="font-size: 11px">
        Nothing hidden. Choosing <em>Never</em> on a suggestion puts it here.
      </span>
    </div>

    <span v-if="error" class="error" style="margin: 0">{{ error }}</span>
  </div>
</template>

<style scoped>
.dismissed-chip {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 2px 4px 2px 8px; border: 1px solid var(--border);
  border-radius: 6px; background: var(--panel-2); font-size: 11px;
}
.link-btn {
  border: none; background: transparent; padding: 2px 4px;
  cursor: pointer; color: var(--accent); font-size: 11px;
}
.link-btn:hover { text-decoration: underline; }
</style>
