<script lang="ts" setup>
import { computed } from 'vue';
import type { Cluster } from './geometry';
import { ROLE_LABEL } from './types';

// The card for one city cluster: every distinct hop placed there, with
// what we know about it and how sure we are.

const props = defineProps<{
  cluster: Cluster;
  pinned: boolean;
  flowLabel: (key: string) => string;
}>();

const MAX_HOPS = 6;

const shown = computed(() => props.cluster.hops.slice(0, MAX_HOPS));
const more = computed(() => Math.max(0, props.cluster.hops.length - MAX_HOPS));

function flowsText(keys: string[]): string {
  const names = [...new Set(keys.map(props.flowLabel))];
  if (names.length <= 3) return names.join(', ');
  return `${names.slice(0, 3).join(', ')} +${names.length - 3}`;
}
</script>

<template>
  <div class="card" :class="{ pinned }" role="dialog" :aria-label="cluster.place">
    <div class="head">
      <strong>{{ cluster.place || 'Unknown location' }}</strong>
      <span class="muted">
        {{ cluster.hops.length }} {{ cluster.hops.length === 1 ? 'hop' : 'hops' }}
        · {{ cluster.flows.length }} {{ cluster.flows.length === 1 ? 'flow' : 'flows' }}
      </span>
    </div>
    <div v-for="(ch, i) in shown" :key="i" class="hop">
      <div class="line">
        <span class="role" :class="'r-' + ch.hop.role">{{ ROLE_LABEL[ch.hop.role] || ch.hop.role }}</span>
        <span class="label" :title="ch.hop.label">{{ ch.hop.label || '—' }}</span>
      </div>
      <div v-if="ch.hop.host && ch.hop.host !== ch.hop.label" class="kv"><span>Host</span><code>{{ ch.hop.host }}</code></div>
      <div v-if="ch.hop.ip && ch.hop.ip !== ch.hop.label" class="kv"><span>IP</span><code>{{ ch.hop.ip }}</code></div>
      <div v-if="ch.hop.org || ch.hop.asn" class="kv">
        <span>Network</span>
        <span>{{ ch.hop.org || 'Unknown org' }}<template v-if="ch.hop.asn"> · AS{{ ch.hop.asn }}</template></span>
      </div>
      <div class="kv"><span>Used by</span><span>{{ flowsText(ch.flows) }}</span></div>
      <div v-if="ch.hop.precision === 'country'" class="note">
        Country precision: placed at the centre of {{ ch.hop.countryName || ch.hop.country }}.
      </div>
      <div v-if="ch.hop.uncertain" class="note warn">
        Best guess. xray picks a pool node per connection; this is the one picked most for new connections in the last 30 s (or, when xray logged no picks, the one carrying the most bytes).
        <template v-if="ch.hop.alternatives?.length">Also picked: {{ ch.hop.alternatives.join(', ') }}.</template>
      </div>
      <div v-if="ch.hop.anycast" class="note">
        Anycast, nearest edge. The location is where the address is registered; you reach whichever edge is closest to the exit.
      </div>
    </div>
    <div v-if="more" class="muted more">and {{ more }} more</div>
    <div v-if="!pinned" class="muted hint">Click the marker to pin this card</div>
  </div>
</template>

<style scoped>
.card {
  width: 290px;
  max-height: 360px;
  overflow: auto;
  padding: 10px 12px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
  font-size: 12px;
  pointer-events: auto;
}
.card.pinned { border-color: var(--accent); }
.head { display: flex; flex-direction: column; gap: 2px; margin-bottom: 8px; }
.head .muted { font-size: 11px; }
.hop { padding: 8px 0; border-top: 1px solid var(--border); display: flex; flex-direction: column; gap: 3px; }
.line { display: flex; align-items: center; gap: 8px; min-width: 0; }
.label { font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.role {
  flex: none;
  padding: 1px 7px;
  border-radius: 9px;
  font-size: 10px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  color: var(--role-c, var(--text-dim));
  border: 1px solid currentColor;
}
.r-you { --role-c: var(--map-you); }
.r-pool { --role-c: var(--map-pool); }
.r-server { --role-c: var(--map-server); }
.r-exit { --role-c: var(--map-exit); }
.r-dest { --role-c: var(--map-dest); }
.kv { display: grid; grid-template-columns: 62px minmax(0, 1fr); gap: 6px; color: var(--text); }
.kv > span:first-child { color: var(--text-dim); }
.kv code { font-size: 11px; word-break: break-all; }
.note { color: var(--text-dim); font-size: 11px; line-height: 1.4; }
.note.warn { color: var(--warn); }
.more, .hint { font-size: 11px; padding-top: 6px; }
</style>
