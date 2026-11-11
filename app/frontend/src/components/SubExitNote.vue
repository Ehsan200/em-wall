<script lang="ts" setup>
import { computed } from 'vue';
import type { ipc } from '../../wailsjs/go/models';
import { countries, checkSubExits } from '../subExits';

// The exit-IP verdict for a subscription a rule is about to route through
// (docs/xray-sub-exit.md). Shown under the Subscription picker in the rule
// forms, before the user saves.
const props = defineProps<{ exit?: ipc.XraySubExitDTO; name: string }>();

const cs = computed(() => (props.exit ? countries(props.exit) : []));
const checking = computed(() => !!props.exit?.checking);
</script>

<template>
  <div v-if="!exit" class="note dim">Subscription “{{ name }}” not found.</div>
  <div v-else-if="exit.verdict === 'multi'" class="note danger" data-tour="sub-exit-warning">
    <strong>⚠ Several exit IPs.</strong>
    “{{ exit.name }}” leaves the internet from <strong>{{ exit.exits.length }}</strong> different IPs<span
      v-if="cs.length"> in {{ cs.length }} {{ cs.length === 1 ? 'country' : 'countries' }} ({{ cs.join(', ') }})</span>.
    Sites will see your IP change between connections, and within one page load, because the balancer spreads
    connections over 2 nodes. Logins, banking and anti-fraud checks may flag or block you.
    <div v-if="exit.rotatingNodes.length" style="margin-top: 4px">
      Rotating exit on: {{ exit.rotatingNodes.join(', ') }}.
    </div>
    <div class="ips">
      <code v-for="x in exit.exits" :key="x.ip">{{ x.ip }}<span v-if="x.country"> {{ x.country }}</span> ×{{ x.nodes }}</code>
    </div>
    <div v-if="!exit.needsAck" class="muted" style="margin-top: 4px">You accepted these exits.</div>
    <div v-else class="muted" style="margin-top: 4px">Saving will ask you to confirm.</div>
  </div>
  <div v-else-if="exit.verdict === 'single'" class="note ok">
    ✓ One exit IP<span v-if="exit.exits[0]?.country"> ({{ exit.exits[0].country }})</span> across
    {{ exit.measured }} node{{ exit.measured === 1 ? '' : 's' }}<span v-if="exit.unmeasured">
      · {{ exit.unmeasured }} not measured yet</span>.
  </div>
  <div v-else class="note dim">
    Exit IPs not checked yet<span v-if="checking"> — checking…</span>.
    <button v-if="!checking" class="link" @click="checkSubExits(exit.subId)">Check now</button>
    <span v-else class="muted"> This takes up to a minute.</span>
  </div>
</template>

<style scoped>
.note { font-size: 11px; line-height: 1.5; padding: 6px 8px; border-radius: 6px; border: 1px solid var(--border); }
.note.danger { border-color: rgba(255, 111, 111, 0.5); background: rgba(255, 111, 111, 0.08); color: var(--text); }
.note.danger strong:first-child { color: var(--danger); }
.note.ok { border-color: rgba(95, 208, 122, 0.4); background: rgba(95, 208, 122, 0.06); }
.note.dim { color: var(--text-dim); }
.ips { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 6px; }
.ips code { font-size: 10px; background: var(--panel-2); border: 1px solid var(--border); border-radius: 4px; padding: 1px 5px; }
button.link { background: none; border: none; padding: 0 2px; color: var(--accent); cursor: pointer; font-size: 11px; }
</style>
