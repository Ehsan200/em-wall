<script setup lang="ts">
// Fragment opt-in for one xray entry: a toggle, plus the three freedom
// fragment fields once it's on. null = off. Mirrors core/xray/fragment.go.
import { computed } from 'vue';

export type FragmentValue = { packets: string; length: string; interval: string };

const DEFAULTS: FragmentValue = { packets: 'tlshello', length: '100-200', interval: '10-20' };

const props = defineProps<{ modelValue: FragmentValue | null; master?: boolean; note?: string }>();
const emit = defineEmits<{ (e: 'update:modelValue', v: FragmentValue | null): void }>();

const HELP =
  'Split the TLS ClientHello into small pieces sent a few milliseconds apart, so DPI that ' +
  'blocks by SNI can\'t read it from one packet. Your server needs no change. ' +
  '"tlshello" needs a TLS or REALITY outbound; for one without TLS use a packet range like "1-3". ' +
  'TCP only — no effect on KCP/QUIC/WireGuard/Hysteria.';

const on = computed({
  get: () => props.modelValue !== null,
  set: (v: boolean) => emit('update:modelValue', v ? { ...DEFAULTS } : null),
});

function set(field: keyof FragmentValue, v: string) {
  if (!props.modelValue) return;
  emit('update:modelValue', { ...props.modelValue, [field]: v });
}
</script>

<template>
  <div class="row" style="gap: 8px; align-items: center; flex-wrap: wrap; font-size: 12px">
    <label class="row" style="gap: 6px; align-items: center" :title="HELP">
      <input type="checkbox" v-model="on" /> Fragment
    </label>
    <template v-if="modelValue">
      <label class="row" style="gap: 4px; align-items: center" title='"tlshello", or a packet range like "1-3"'>
        packets <input :value="modelValue.packets" @input="set('packets', ($event.target as HTMLInputElement).value)" style="width: 80px" />
      </label>
      <label class="row" style="gap: 4px; align-items: center" title="Bytes per fragment, MIN-MAX">
        length <input :value="modelValue.length" @input="set('length', ($event.target as HTMLInputElement).value)" style="width: 70px" />
      </label>
      <label class="row" style="gap: 4px; align-items: center" title="Milliseconds between fragments, MIN-MAX">
        interval <input :value="modelValue.interval" @input="set('interval', ($event.target as HTMLInputElement).value)" style="width: 60px" />
      </label>
      <span v-if="master" class="muted" style="font-size: 11px">applied to this master's pool nodes</span>
    </template>
    <span v-if="modelValue && note" class="muted" style="font-size: 11px; color: var(--warn); flex-basis: 100%">
      Fragmentation won't apply to this outbound: {{ note }}.
    </span>
  </div>
</template>
