<script lang="ts" setup>
import type { ipc } from '../../wailsjs/go/models';

defineProps<{
  status: ipc.StatusResult | null;
  error: string;
}>();
</script>

<template>
  <div class="status-bar">
    <template v-if="status">
      <div><span class="label">Daemon</span> <span class="tag tag-allow">connected</span></div>
      <div><span class="label">Listen</span> {{ status.listenAddr }}</div>
      <div><span class="label">Upstream</span> {{ status.upstreamDns }}</div>
      <div><span class="label">DoH/DoT block</span>
        <span :class="status.blockEncryptedDns ? 'tag tag-block' : 'tag tag-off'">
          {{ status.blockEncryptedDns ? 'on' : 'off' }}
        </span>
      </div>
      <div><span class="label">Rules</span> {{ status.ruleCount }}</div>
      <div><span class="label">Uptime</span> {{ status.uptime }}</div>
      <div style="margin-left:auto" class="muted">v{{ status.version }}</div>
    </template>
    <template v-else>
      <div><span class="tag tag-block">daemon unreachable</span> <span class="muted">{{ error || 'connecting…' }}</span></div>
    </template>
  </div>
</template>
