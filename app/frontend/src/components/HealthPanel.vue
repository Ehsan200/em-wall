<script lang="ts" setup>
import ConnectionHealth from './ConnectionHealth.vue';
import PoolTimeline from './PoolTimeline.vue';

// Health: how proxied connections are doing and how each master's
// subscription pool behaves over time. Kept apart from Usage, which is
// about data volume, not reliability.
</script>

<template>
  <div class="panel">
    <ConnectionHealth />
    <PoolTimeline />
  </div>
</template>
