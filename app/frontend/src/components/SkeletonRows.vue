<script lang="ts" setup>
// Placeholder table rows shown until a table's first fetch lands, so the
// "nothing here" row never flashes for a list that isn't empty.
withDefaults(defineProps<{ cols: number; rows?: number }>(), { rows: 4 });
</script>

<template>
  <tr v-for="r in rows" :key="r" class="skeleton-row">
    <td :colspan="cols"><span class="skeleton" :style="{ width: `${92 - (r % 3) * 14}%` }"></span></td>
  </tr>
</template>
