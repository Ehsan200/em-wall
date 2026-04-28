<script lang="ts" setup>
import { ref, watch, onMounted } from 'vue';
import { GroupIcon as FetchGroupIcon } from '../../wailsjs/go/main/App';

const props = defineProps<{
  groupKey: string;
  size?: number;
}>();

const dataUrl = ref<string>('');

const cache = new Map<string, Promise<string>>();

async function load(key: string) {
  if (!key) return;
  let p = cache.get(key);
  if (!p) {
    p = (async () => {
      const r = await FetchGroupIcon(key);
      return `data:${r.mime};base64,${r.data}`;
    })();
    cache.set(key, p);
  }
  try {
    dataUrl.value = await p;
  } catch {
    cache.delete(key);
  }
}

onMounted(() => load(props.groupKey));
watch(() => props.groupKey, (k) => load(k));
</script>

<template>
  <img
    v-if="dataUrl"
    :src="dataUrl"
    :width="size || 24"
    :height="size || 24"
    style="border-radius: 5px; flex-shrink: 0; object-fit: cover; background: var(--panel-2)"
  />
  <span v-else :style="{ width: (size || 24) + 'px', height: (size || 24) + 'px' }" class="placeholder"></span>
</template>

<style scoped>
.placeholder { display: inline-block; background: var(--border); border-radius: 5px; }
</style>
