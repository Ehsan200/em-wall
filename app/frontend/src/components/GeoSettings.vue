<script lang="ts" setup>
// Settings → Geo data. Self-contained card: the geosite/geoip .dat files
// (xray rules, group suggestions) and the DB-IP City/ASN files (map).
// Shows each file's provenance and next refresh, and edits the four
// geo_* settings. Updates run in the daemon's background; while one is
// running the card polls GeoStatus every 2 s.
import { ref, computed, onMounted, onUnmounted } from 'vue';
import {
  GeoStatus, GeoUpdate, GeoRollback, SetSetting,
  ListXraySets, ListXray, ListProxies,
} from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';
import SearchSelect from './SearchSelect.vue';
import { startPoll } from '../poll';
import { fmtDateTime } from '../timezone';

const status = ref<ipc.GeoStatusResult | null>(null);
const error = ref('');
const busy = ref(false);        // a request from this card is in flight
const confirmRollback = ref(false);
const customDraft = ref('');
const customSaved = ref(false);
const viaOptions = ref<SelectOption[]>([]);
const sourceChanged = ref(false);
let stopPoll: (() => void) | undefined;
// Set while the custom URL field holds unsaved typing, so a status poll
// doesn't overwrite it.
let customDirty = false;

// Same shape as SearchSelect's option type.
type SelectOption = { value: string; label: string; hint?: string; disabled?: boolean };

const FILE_LABELS: Record<string, { name: string; use: string }> = {
  geosite: { name: 'geosite.dat', use: 'domain lists — xray rules, group suggestions' },
  geoip: { name: 'geoip.dat', use: 'IP lists — xray rules, country fallback' },
  city: { name: 'DB-IP City Lite', use: 'city locations for the world map' },
  asn: { name: 'DB-IP ASN Lite', use: 'network owner (ASN) on the map' },
};

const SOURCES: SelectOption[] = [
  { value: 'loyalsoldier', label: 'Loyalsoldier', hint: 'v2ray-rules-dat (default)' },
  { value: 'chocolate4u', label: 'Chocolate4U', hint: 'Iran-v2ray-rules' },
  { value: 'v2fly', label: 'v2fly', hint: 'domain-list-community + geoip' },
  { value: 'custom', label: 'Custom URL' },
];

const updating = computed(() => !!status.value?.updating);
const anyBackup = computed(() => (status.value?.files || []).some((f) => f.hasBackup));
const loaded = computed(() => status.value !== null);

function setStatus(s: ipc.GeoStatusResult) {
  const wasUpdating = status.value?.updating;
  status.value = s;
  if (!customDirty) customDraft.value = s.customUrl || '';
  // Poll only while the daemon is working; a finished update stops it.
  if (s.updating && !stopPoll) {
    stopPoll = startPoll(refresh, 2000);
  } else if (!s.updating && stopPoll) {
    stopPoll();
    stopPoll = undefined;
  }
  if (wasUpdating && !s.updating) busy.value = false;
}

async function refresh() {
  try {
    setStatus(await GeoStatus());
    error.value = '';
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  }
}

async function loadViaOptions() {
  const opts: SelectOption[] = [{ value: '', label: 'Direct only' }];
  try {
    const [sets, entries, proxies] = await Promise.all([ListXraySets(), ListXray(), ListProxies()]);
    for (const s of sets || []) {
      if (s.enabled) opts.push({ value: `xrayset:${s.name}`, label: s.name, hint: 'set' });
    }
    for (const x of entries || []) {
      if (x.enabled) opts.push({ value: `xray:${x.name}`, label: x.name, hint: 'xray' });
    }
    for (const p of proxies || []) {
      opts.push({ value: `proxy:${p.name}`, label: p.name, hint: 'proxy' });
    }
  } catch {
    // The picker still offers "Direct only" and the stored value.
  }
  const cur = status.value?.fetchVia;
  if (cur && !opts.some((o) => o.value === cur)) opts.push({ value: cur, label: cur, hint: 'missing' });
  viaOptions.value = opts;
}

async function save(key: string, value: string) {
  error.value = '';
  try {
    await SetSetting(key, value);
    await refresh();
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  }
}

async function saveCustom() {
  customDirty = false;
  await save('geo_custom_url', customDraft.value.trim());
  customSaved.value = !error.value;
}

async function update(file = '') {
  if (file === '') sourceChanged.value = false;
  busy.value = true;
  error.value = '';
  try {
    setStatus(await GeoUpdate(file));
    if (!status.value?.updating) busy.value = false;
  } catch (e: any) {
    error.value = String(e?.message ?? e);
    busy.value = false;
  }
}

async function rollback() {
  confirmRollback.value = false;
  busy.value = true;
  error.value = '';
  try {
    setStatus(await GeoRollback());
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  } finally {
    busy.value = false;
  }
}

function fmtWhen(s: string): string {
  return s ? fmtDateTime(new Date(s)) : '—';
}

function fmtSize(n: number): string {
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${Math.round(n / 1024)} KB`;
  return `${n} B`;
}

function sourceLabel(f: ipc.GeoFileDTO): string {
  switch (f.source) {
    case 'bundled': return 'bundled with the app';
    case 'dbip': return 'DB-IP';
    case '': return '—';
    default: return SOURCES.find((s) => s.value === f.source)?.label ?? f.source;
  }
}

onMounted(async () => {
  await refresh();
  loadViaOptions();
});
onUnmounted(() => stopPoll?.());
</script>

<template>
  <div class="geo-card col" data-tour="settings-geo">
    <div class="row" style="justify-content: space-between; align-items: flex-start">
      <div class="col" style="gap: 4px">
        <strong>Geo data</strong>
        <span class="muted" style="font-size: 12px; line-height: 1.5">
          Offline domain and IP lists used by xray routing rules and group
          suggestions, plus city/ASN databases for the world map. Updates are
          verified (published checksum, full parse) and refused if they would
          drop a <code>geosite:</code>/<code>geoip:</code> tag your xray rules
          use. Your rules and groups are never changed by an update.
        </span>
      </div>
      <span v-if="updating" class="tag tag-route" style="white-space: nowrap">
        <span class="spinner"></span> updating
      </span>
    </div>

    <div v-if="error" class="error" style="margin: 0">{{ error }}</div>

    <div v-if="!loaded" class="skeleton-stack">
      <span v-for="i in 4" :key="i" class="skeleton" style="width: 80%"></span>
    </div>
    <div v-else class="files">
      <div v-for="f in status!.files" :key="f.file" class="file">
        <div class="col" style="gap: 2px; min-width: 0">
          <div class="row" style="gap: 8px; align-items: baseline">
            <strong style="font-size: 13px">{{ FILE_LABELS[f.file]?.name ?? f.file }}</strong>
            <span v-if="!f.present" class="tag tag-off">not downloaded</span>
            <span v-else class="muted" style="font-size: 11px">
              {{ fmtSize(f.sizeBytes) }}<template v-if="f.entries"> ·
                {{ f.entries.toLocaleString() }} {{ f.file === 'city' || f.file === 'asn' ? 'nodes' : 'tags' }}</template>
            </span>
          </div>
          <span class="muted" style="font-size: 11px">{{ FILE_LABELS[f.file]?.use }}</span>
          <span v-if="f.present" class="muted" style="font-size: 11px">
            {{ sourceLabel(f) }}<template v-if="f.version"> · {{ f.version }}</template>
            · updated {{ fmtWhen(f.updatedAt) }}
            <template v-if="f.nextDue"> · next check {{ fmtWhen(f.nextDue) }}</template>
            <template v-if="f.hasBackup"> · previous version kept</template>
          </span>
          <span v-else-if="f.file === 'city' || f.file === 'asn'" class="muted" style="font-size: 11px">
            Downloaded the first time the map opens (about 130 MB for City).
          </span>
          <span v-if="f.lastError" class="warn-line">{{ f.lastError }}</span>
        </div>
        <button :disabled="updating || busy" @click="update(f.file)">
          {{ f.present ? 'Update' : 'Download' }}
        </button>
      </div>
    </div>

    <div v-if="loaded" class="col settings">
      <div class="row setting">
        <span class="label muted">Domain lists from</span>
        <SearchSelect
          :model-value="status!.geositeSource || 'loyalsoldier'"
          :options="SOURCES"
          :disabled="busy"
          style="min-width: 260px"
          @change="(v: string) => { sourceChanged = true; save('geo_source', v) }"
        />
      </div>
      <span v-if="sourceChanged" class="muted hint">
        Takes effect at the next update — use <strong>Update all now</strong> to switch right away.
      </span>
      <div v-if="status!.geositeSource === 'custom'" class="row setting">
        <span class="label muted">Base URL</span>
        <input
          v-model="customDraft"
          type="url"
          spellcheck="false"
          placeholder="https://example.com/geo/"
          style="flex: 1"
          @input="customSaved = false; customDirty = true"
        />
        <button :disabled="busy" @click="saveCustom">Save</button>
        <span v-if="customSaved" class="muted" style="font-size: 11px; color: var(--success)">✓ Saved</span>
      </div>
      <span v-if="status!.geositeSource === 'custom'" class="muted hint">
        Must serve <code>geosite.dat</code> and <code>geoip.dat</code>; a
        <code>.sha256sum</code> next to each is checked when present.
      </span>

      <div class="row setting">
        <span class="label muted">If direct fails, fetch via</span>
        <SearchSelect
          :model-value="status!.fetchVia"
          :options="viaOptions"
          placeholder="Direct only"
          :disabled="busy"
          style="min-width: 260px"
          @change="(v: string) => save('geo_fetch_via', v)"
        />
      </div>

      <label class="row setting" style="justify-content: space-between">
        <div class="col" style="gap: 2px">
          <span>Update automatically</span>
          <span class="muted hint">Domain/IP lists weekly, DB-IP monthly.</span>
        </div>
        <label class="toggle" @click.prevent="save('geo_auto_update', String(!status!.autoUpdate))">
          <input type="checkbox" :checked="status!.autoUpdate" :disabled="busy" />
          <span class="track"></span>
        </label>
      </label>
    </div>

    <div v-if="loaded" class="row" style="gap: 10px; flex-wrap: wrap; align-items: center">
      <button class="primary" :disabled="updating || busy" @click="update('')">
        <span v-if="updating" class="spinner"></span>{{ updating ? 'Updating…' : 'Update all now' }}
      </button>
      <template v-if="!confirmRollback">
        <button :disabled="updating || busy || !anyBackup" @click="confirmRollback = true"
                :title="anyBackup ? '' : 'No previous versions kept yet'">
          Roll back
        </button>
      </template>
      <template v-else>
        <span class="muted" style="font-size: 12px">Swap every file with its previous version?</span>
        <button class="danger" :disabled="busy" @click="rollback">Roll back</button>
        <button :disabled="busy" @click="confirmRollback = false">Cancel</button>
      </template>
    </div>

    <span class="muted hint">
      IP geolocation by <a href="https://db-ip.com" target="_blank" rel="noopener">DB-IP</a>,
      licensed under CC BY 4.0.
    </span>
  </div>
</template>

<style scoped>
.geo-card {
  gap: 12px;
  padding: 14px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 8px;
}
.files {
  display: flex;
  flex-direction: column;
  border-top: 1px solid var(--border);
}
.file {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  padding: 10px 0;
  border-bottom: 1px solid var(--border);
}
.file button { flex: none; }
.settings { gap: 10px; }
.setting { gap: 12px; align-items: center; }
.setting .label { min-width: 170px; font-size: 12px; }
.hint { font-size: 11px; line-height: 1.5; }
.warn-line { font-size: 11px; color: var(--warn); line-height: 1.4; }
input[type="url"] {
  font-family: var(--mono, monospace);
  font-size: 12px;
  padding: 6px 8px;
  background: var(--bg);
  color: var(--fg);
  border: 1px solid var(--border);
  border-radius: 6px;
}
</style>
