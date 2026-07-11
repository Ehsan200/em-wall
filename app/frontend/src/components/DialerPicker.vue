<script lang="ts" setup>
import { ref, watch } from 'vue';

// A master entry's Dialer is a comma-separated list of typed refs
// (xraysub:NAME / xray:NAME / proxy:NAME). All referenced nodes merge into
// ONE leastPing balancer, so the master's transport is tunneled through
// whichever is currently fastest. This control edits that list as rows of
// (kind, name) dropdowns and emits the composed string.

const props = defineProps<{
  modelValue: string;
  xrayNames: string[];
  subNames: string[];
  proxyNames: string[];
  selfName?: string; // exclude from xray options to avoid a self-cycle
}>();
const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>();

type Kind = 'xraysub' | 'xray' | 'proxy';
type Row = { kind: Kind; name: string };

const rows = ref<Row[]>([]);
const KINDS: Kind[] = ['xraysub', 'xray', 'proxy'];

function kindLabel(k: Kind): string {
  return k === 'xraysub' ? 'subscription (fastest)' : k === 'xray' ? 'xray node' : 'proxy';
}

function parse(s: string): Row[] {
  return (s || '')
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean)
    .map((item) => {
      const i = item.indexOf(':');
      const kind = (i >= 0 ? item.slice(0, i) : '').trim().toLowerCase();
      const name = (i >= 0 ? item.slice(i + 1) : '').trim().toLowerCase();
      return {
        kind: (KINDS.includes(kind as Kind) ? kind : 'xraysub') as Kind,
        name,
      };
    });
}

function compose(rs: Row[]): string {
  return rs.filter((r) => r.name).map((r) => `${r.kind}:${r.name}`).join(',');
}

// Keep local rows in sync when the parent resets the value (e.g. opening a
// fresh draft), but don't clobber in-progress edits that round-trip equal.
watch(
  () => props.modelValue,
  (v) => {
    if (compose(rows.value) !== (v || '')) rows.value = parse(v);
  },
  { immediate: true },
);

function namesFor(kind: Kind): string[] {
  if (kind === 'xray') return props.xrayNames.filter((n) => n !== (props.selfName || ''));
  if (kind === 'xraysub') return props.subNames;
  return props.proxyNames;
}

function commit() {
  emit('update:modelValue', compose(rows.value));
}
function addRow() {
  rows.value.push({ kind: 'xraysub', name: '' });
}
function removeRow(i: number) {
  rows.value.splice(i, 1);
  commit();
}
function onKindChange(i: number) {
  rows.value[i].name = '';
  commit();
}
</script>

<template>
  <div class="col" style="gap: 6px">
    <div class="row" style="gap: 6px; align-items: center">
      <span class="muted" style="font-size: 11px; text-transform: uppercase; letter-spacing: 0.5px">
        Dialer — tunnel this entry through the fastest of:
      </span>
    </div>

    <div v-if="rows.length === 0" class="muted" style="font-size: 11px">
      None — this entry connects directly (no dialer chain).
    </div>

    <div v-for="(r, i) in rows" :key="i" class="row" style="gap: 6px; align-items: center; flex-wrap: wrap">
      <select v-model="r.kind" @change="onKindChange(i)" style="width: 190px">
        <option v-for="k in KINDS" :key="k" :value="k">{{ kindLabel(k) }}</option>
      </select>
      <select v-model="r.name" @change="commit" style="min-width: 180px; flex: 1">
        <option value="" disabled>— choose {{ r.kind === 'xraysub' ? 'subscription' : r.kind }} —</option>
        <option v-for="n in namesFor(r.kind)" :key="n" :value="n">{{ n }}</option>
        <!-- keep a dangling stored name visible even if it no longer exists -->
        <option v-if="r.name && !namesFor(r.kind).includes(r.name)" :value="r.name">
          {{ r.name }} (missing)
        </option>
      </select>
      <button @click="removeRow(i)" title="remove">✕</button>
    </div>

    <div class="row" style="gap: 6px; align-items: center">
      <button @click="addRow">+ Add dialer ref</button>
      <span v-if="rows.length > 1" class="muted" style="font-size: 11px">
        all merged into one leastPing balancer — global fastest wins
      </span>
    </div>
  </div>
</template>
