<script lang="ts" setup>
import { ref, watch, onMounted } from 'vue';
import { AppIcon as FetchAppIcon } from '../../wailsjs/go/main/App';

const props = defineProps<{
  appKey: string;
  size?: number;
}>();

const dataUrl = ref<string>('');
const installed = ref<boolean>(false);

// Module-level cache so multiple uses of <AppIcon :appKey="x" /> in
// one render share a single Promise per key.
const cache = new Map<string, Promise<{ dataUrl: string; installed: boolean }>>();

async function load(key: string) {
  if (!key) return;
  let p = cache.get(key);
  if (!p) {
    p = (async () => {
      const r = await FetchAppIcon(key);
      const url = `data:${r.mime};base64,${r.data}`;
      return { dataUrl: url, installed: r.installed };
    })();
    cache.set(key, p);
  }
  try {
    const r = await p;
    dataUrl.value = r.dataUrl;
    installed.value = r.installed;
  } catch {
    cache.delete(key);
  }
}

onMounted(() => load(props.appKey));
watch(() => props.appKey, (k) => load(k));
</script>

<template>
  <img
    v-if="dataUrl"
    :src="dataUrl"
    :width="size || 24"
    :height="size || 24"
    :class="{ 'icon-not-installed': !installed }"
    style="border-radius: 5px; flex-shrink: 0; object-fit: cover"
  />
  <span v-else :style="{ width: (size || 24) + 'px', height: (size || 24) + 'px' }" class="icon-placeholder"></span>
</template>

<style scoped>
.icon-not-installed { opacity: 0.45; filter: grayscale(0.7); }
.icon-placeholder { display: inline-block; background: var(--border); border-radius: 5px; }
</style>
