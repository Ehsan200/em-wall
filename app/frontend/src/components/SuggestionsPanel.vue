<script lang="ts" setup>
import { ref, onMounted } from 'vue';
import { Groups } from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';
import SuggestStrip from './SuggestStrip.vue';

// The Suggestions page. Creating a group here never creates rules: the
// group's apply form lives on the Rules page, so both "created" and
// "apply" hand the group to the parent, which switches pages and opens it.
type Preset = { action: 'block' | 'route'; iface: string } | null;

const emit = defineEmits<{
  (e: 'open-group', key: string, preset: Preset): void;
  (e: 'count', n: number): void;
  (e: 'changed'): void;
}>();

const groups = ref<ipc.GroupDTO[]>([]);
const strip = ref<InstanceType<typeof SuggestStrip> | null>(null);

async function reloadGroups() {
  try { groups.value = (await Groups()) || []; } catch { /* daemon starting */ }
}

async function onChanged() {
  await reloadGroups();
  strip.value?.reload();
  emit('changed');
}

onMounted(reloadGroups);
</script>

<template>
  <div class="panel">
    <div class="col" style="gap: 4px; margin-bottom: 14px">
      <strong style="font-size: 15px">Suggested groups</strong>
      <span class="muted" style="font-size: 12px; line-height: 1.5">
        Sites and apps you use that no rule covers yet, matched against the geosite lists.
        Creating a group adds no rules — you choose where it goes next, on the Rules page.
      </span>
    </div>
    <SuggestStrip ref="strip" page :groups="groups"
                  @created="(k, p) => emit('open-group', k, p)"
                  @apply="(k) => emit('open-group', k, null)"
                  @changed="onChanged"
                  @count="(n) => emit('count', n)" />
  </div>
</template>
