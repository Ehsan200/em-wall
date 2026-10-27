<script lang="ts" setup>
import { ref, computed, watch, nextTick, onMounted, onUnmounted } from 'vue';
import { startPoll } from '../poll';
import {
  SuggestList, SuggestDismiss, SuggestAccept, SuggestSplit,
  UpdateCustomGroup, DeleteCustomGroup,
} from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';
import GroupIcon from './GroupIcon.vue';

// The Suggestions page: geosite tags seen in recent DNS traffic that no
// rule covers, as a list (left) and the selected one's details (right).
// Nothing here creates rules — "Apply" and "Create group" hand a group to
// the parent, which opens the normal apply form on the Rules page
// (optionally preset, for "Use same set").
type Preset = { action: 'block' | 'route'; iface: string } | null;

const emit = defineEmits<{
  (e: 'open-group', key: string, preset: Preset): void;
  // Number of suggestions after each load (sidebar badge).
  (e: 'count', n: number): void;
  // Group definitions changed (split / undo).
  (e: 'changed'): void;
}>();

const items = ref<ipc.SuggestionDTO[]>([]);
const selectedTag = ref('');
const error = ref('');
const busy = ref(false);
const includeIPs = ref<Record<string, boolean>>({});
// Past this many IP ranges, "Include IP ranges" asks twice: every range
// becomes its own rule and, once routed, its own system route — geoip:GOOGLE
// alone is ~8.5k ranges, enough to bog down the daemon, the host's routing
// table and the Rules page.
const LARGE_IP_COUNT = 200;
const confirmIPs = ref(''); // tag whose large IP include awaits a second click
const showDomains = ref(false);
const splitOpen = ref(''); // collision group key whose split preview is open
const lastSplit = ref<{ name: string; moved: number; result: ipc.SuggestSplitResult } | null>(null);
const undoing = ref(false);
const listEl = ref<HTMLElement | null>(null);

const selected = computed(() => items.value.find(s => s.tag === selectedTag.value) || null);

async function load() {
  try {
    items.value = (await SuggestList()) || [];
    error.value = '';
    emit('count', items.value.length);
  } catch {
    // The daemon may predate suggestions; stay quiet rather than shout.
    items.value = [];
  }
  if (!selected.value) selectedTag.value = items.value[0]?.tag || '';
}

let stop: (() => void) | null = null;
onMounted(() => { load(); stop = startPoll(load, 60_000); });
onUnmounted(() => stop?.());

watch(selectedTag, () => { showDomains.value = false; splitOpen.value = ''; confirmIPs.value = ''; });
watch(includeIPs, () => { confirmIPs.value = ''; }, { deep: true });

function largeIPInclude(s: ipc.SuggestionDTO): boolean {
  return !!includeIPs.value[s.tag] && (s.cidrs || []).length > LARGE_IP_COUNT;
}

// ---- list ----

function select(tag: string) { selectedTag.value = tag; }

function onListKey(e: KeyboardEvent) {
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
  e.preventDefault();
  const i = items.value.findIndex(s => s.tag === selectedTag.value);
  const next = Math.min(items.value.length - 1, Math.max(0, i + (e.key === 'ArrowDown' ? 1 : -1)));
  if (items.value[next]) {
    selectedTag.value = items.value[next].tag;
    nextTick(() => listEl.value?.querySelector('.row-item.on')?.scrollIntoView({ block: 'nearest' }));
  }
}

function hostCount(s: ipc.SuggestionDTO): string {
  const n = (s.seenNames || []).length;
  return `seen ${n}${n >= 10 ? '+' : ''} ${n === 1 ? 'host' : 'hosts'}`;
}

function hint(s: ipc.SuggestionDTO): string {
  if (s.builtin) return `built-in: ${s.builtinName}`;
  return `+${plural((s.missing || []).length, 'domain')}`;
}

// ---- detail helpers ----

function plural(n: number, one: string, many = one + 's'): string {
  return `${n} ${n === 1 ? one : many}`;
}

function ago(iso: string): string {
  const t = Date.parse(iso);
  if (!t) return '';
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 90) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

// "github.com — geo data updates" lines, for the badge tooltip.
function usedByTitle(s: ipc.SuggestionDTO): string {
  return (s.usedBy || []).map(u => `${u.host} — ${u.purpose}`).join('\n');
}

// "em-wall uses github.com for geo data updates and api.github.com for
// app updates — routing this group also routes those."
function usedBySentence(s: ipc.SuggestionDTO): string {
  const byPurpose = new Map<string, string[]>();
  for (const u of s.usedBy || []) {
    const hosts = byPurpose.get(u.purpose) || [];
    if (!hosts.includes(u.host)) hosts.push(u.host);
    byPurpose.set(u.purpose, hosts);
  }
  const parts = [...byPurpose].map(([purpose, hosts]) => {
    const shown = hosts.slice(0, 3).join(', ') + (hosts.length > 3 ? ` +${hosts.length - 3}` : '');
    return `${shown} for ${purpose}`;
  });
  if (!parts.length) return '';
  const list = parts.length === 1 ? parts[0] : `${parts.slice(0, -1).join(', ')} and ${parts[parts.length - 1]}`;
  return `em-wall uses ${list} — routing this group also routes those.`;
}

// The single binding an overlap's rules share, if they share one.
function sameBinding(c: ipc.SuggestCollisionDTO): Preset {
  const actions = c.actions || [];
  const ifaces = c.interfaces || [];
  if (actions.length !== 1 || ifaces.length !== 1) return null;
  if (actions[0] === 'route' && ifaces[0]) return { action: 'route', iface: ifaces[0] };
  if (actions[0] === 'block') return { action: 'block', iface: '' };
  return null;
}

function bindingLabel(c: ipc.SuggestCollisionDTO): string {
  const actions = c.actions || [];
  const parts: string[] = [];
  for (const i of c.interfaces || []) if (i) parts.push(`via ${i}`);
  if (actions.includes('block')) parts.push('blocks');
  if (actions.includes('allow')) parts.push('allows');
  return parts.join(', ') || actions.join(', ');
}

function collisionVerb(c: ipc.SuggestCollisionDTO): string {
  const routes = (c.actions || []).includes('route');
  return routes ? `already routes ${c.patterns.length} of these ${bindingLabel(c)}` : `already ${bindingLabel(c)} ${c.patterns.length} of these`;
}

// ---- actions ----

async function create(s: ipc.SuggestionDTO, preset: Preset) {
  if (largeIPInclude(s) && confirmIPs.value !== s.tag) {
    confirmIPs.value = s.tag;
    return;
  }
  confirmIPs.value = '';
  busy.value = true;
  try {
    const cidrs = s.cidrs || [];
    const withIPs = !!includeIPs.value[s.tag] && cidrs.length > 0;
    // Past the daemon's own cap the IP ranges have to be passed explicitly.
    const explicit = withIPs && cidrs.length > 64;
    const key = await SuggestAccept({
      tag: s.tag,
      displayName: s.displayName,
      patterns: explicit ? [...(s.missing || []), ...cidrs] : [],
      includeIps: withIPs && !explicit,
    } as ipc.SuggestAcceptParams);
    error.value = '';
    await load();
    emit('open-group', key, preset);
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

function applyBuiltin(s: ipc.SuggestionDTO) {
  emit('open-group', s.builtin, null);
}

async function dismiss(s: ipc.SuggestionDTO, forever: boolean) {
  busy.value = true;
  try {
    await SuggestDismiss(s.tag, forever);
    const i = items.value.findIndex(x => x.tag === s.tag);
    items.value = items.value.filter(x => x.tag !== s.tag);
    selectedTag.value = items.value[Math.min(i, items.value.length - 1)]?.tag || '';
    emit('count', items.value.length);
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function split(s: ipc.SuggestionDTO, c: ipc.SuggestCollisionDTO) {
  busy.value = true;
  try {
    const result = await SuggestSplit({
      tag: s.tag,
      displayName: s.displayName,
      patterns: c.patterns,
      fromGroups: [c.groupKey],
    } as ipc.SuggestSplitParams);
    lastSplit.value = { name: s.displayName, moved: c.patterns.length, result };
    splitOpen.value = '';
    error.value = '';
    await load();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = false;
  }
}

async function undoSplit() {
  const last = lastSplit.value;
  if (!last) return;
  undoing.value = true;
  try {
    for (const b of last.result.before || []) {
      await UpdateCustomGroup(b.key, b.displayName, b.description, b.patterns, b.color);
    }
    if (last.result.newGroupKey) {
      await DeleteCustomGroup(last.result.newGroupKey, false);
    }
    lastSplit.value = null;
    error.value = '';
    await load();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    undoing.value = false;
  }
}

const DOMAIN_PREVIEW = 200;
</script>

<template>
  <div class="panel sp">
    <div class="col" style="gap: 4px; margin-bottom: 12px">
      <strong style="font-size: 15px">Suggested groups</strong>
      <span class="muted" style="font-size: 12px">
        Sites you use that no rule covers yet. Nothing changes until you apply a group on the Rules page.
      </span>
    </div>

    <div v-if="lastSplit" class="undo-bar">
      <span style="flex: 1">
        Moved {{ plural(lastSplit.moved, 'pattern') }} into <strong>{{ lastSplit.name }}</strong>. Rules didn't change.
      </span>
      <button class="small" :disabled="undoing" @click="undoSplit">{{ undoing ? 'Undoing…' : 'Undo' }}</button>
      <button class="quiet" title="Dismiss" @click="lastSplit = null">✕</button>
    </div>

    <div v-if="error" class="error">{{ error }}</div>

    <div v-if="!items.length" class="empty" data-tour="suggest-list">
      <strong>No suggestions right now</strong>
      <span class="muted">
        When you keep using a site a geosite list knows and no rule covers, it shows up here as a ready-made group.
      </span>
    </div>

    <div v-else class="md" data-tour="suggest-list">
      <!-- master -->
      <div ref="listEl" class="list" role="listbox" tabindex="0" aria-label="Suggestions"
           :aria-activedescendant="selectedTag ? `sg-${selectedTag}` : undefined"
           @keydown="onListKey">
        <div v-for="s in items" :id="`sg-${s.tag}`" :key="s.tag" role="option"
             :aria-selected="s.tag === selectedTag"
             class="row-item" :class="{ on: s.tag === selectedTag }" @click="select(s.tag)">
          <GroupIcon v-if="s.builtin" :group-key="s.builtin" :size="20" />
          <span v-else class="badge">{{ s.displayName.slice(0, 1) }}</span>
          <div class="col" style="gap: 1px; flex: 1; min-width: 0">
            <span class="ellipsis name">{{ s.displayName }}</span>
            <span class="muted ellipsis sub">{{ hostCount(s) }} · {{ hint(s) }}</span>
          </div>
          <span v-if="(s.usedBy || []).length" class="self-dot" :title="`Used by em-wall\n${usedByTitle(s)}`"></span>
        </div>
      </div>

      <!-- detail -->
      <div v-if="selected" class="detail">
        <div class="row" style="gap: 10px; align-items: center">
          <GroupIcon v-if="selected.builtin" :group-key="selected.builtin" :size="28" />
          <span v-else class="badge big">{{ selected.displayName.slice(0, 1) }}</span>
          <div class="col" style="gap: 2px; flex: 1; min-width: 0">
            <strong class="ellipsis" style="font-size: 14px">{{ selected.displayName }}</strong>
            <span class="muted sub">
              geosite:{{ selected.tag.toLowerCase() }} · {{ plural(selected.hits, 'query', 'queries') }} · last {{ ago(selected.lastSeen) }}
            </span>
          </div>
          <span v-if="(selected.usedBy || []).length" class="self-badge" :title="usedByTitle(selected)">used by em-wall</span>
        </div>

        <section>
          <div class="label">Seen</div>
          <div class="chips">
            <code v-for="n in selected.seenNames || []" :key="n" class="pat">{{ n }}</code>
          </div>
        </section>

        <section>
          <button class="disclose" @click="showDomains = !showDomains" :aria-expanded="showDomains">
            <span class="caret">{{ showDomains ? '▾' : '▸' }}</span>
            {{ selected.builtin ? `This tag lists ${plural((selected.missing || []).length, 'domain')} no rule covers`
                                : `Adds ${plural((selected.missing || []).length, 'domain')}` }}
            <span v-if="selected.skipped" class="muted"> · {{ selected.skipped }} keyword/regex skipped</span>
          </button>
          <div v-if="showDomains" class="chips domains">
            <code v-for="p in (selected.missing || []).slice(0, DOMAIN_PREVIEW)" :key="p" class="pat">{{ p }}</code>
            <span v-if="(selected.missing || []).length > DOMAIN_PREVIEW" class="muted sub">
              +{{ (selected.missing || []).length - DOMAIN_PREVIEW }} more
            </span>
          </div>
        </section>

        <p v-if="(selected.usedBy || []).length" class="note">{{ usedBySentence(selected) }}</p>

        <section v-if="(selected.collisions || []).length">
          <div class="label">Already routed</div>
          <div v-for="c in selected.collisions" :key="c.groupKey" class="collision">
            <div class="row" style="gap: 8px; align-items: center">
              <span style="flex: 1; min-width: 0">
                <strong>{{ c.groupName }}</strong> <span class="muted">{{ collisionVerb(c) }}</span>
              </span>
              <button v-if="sameBinding(c)" class="small" :disabled="busy"
                      :title="`Create this group and apply it the way ${c.groupName} is`"
                      @click="create(selected, sameBinding(c))">
                {{ sameBinding(c)!.action === 'route' ? 'Use same set' : 'Block too' }}
              </button>
              <button v-if="c.custom" class="quiet" :disabled="busy"
                      @click="splitOpen = splitOpen === c.groupKey ? '' : c.groupKey">
                Split overlap…
              </button>
            </div>
            <div v-if="splitOpen === c.groupKey" class="split">
              <span class="sub">
                Moves {{ plural(c.patterns.length, 'pattern') }} out of <strong>{{ c.groupName }}</strong>
                into a new <strong>{{ selected.displayName }}</strong> group. Rules stay, so routing doesn't change.
              </span>
              <div class="chips">
                <code v-for="p in c.patterns" :key="p" class="pat">{{ p }}</code>
              </div>
              <div class="row" style="gap: 6px; justify-content: flex-end">
                <button class="small" @click="splitOpen = ''">Cancel</button>
                <button class="small primary" :disabled="busy" @click="split(selected, c)">
                  Move {{ plural(c.patterns.length, 'pattern') }}
                </button>
              </div>
            </div>
          </div>
        </section>

        <label v-if="!selected.builtin && (selected.cidrs || []).length" class="row ip-opt">
          <input type="checkbox" v-model="includeIPs[selected.tag]" />
          <span>Include {{ plural(selected.cidrs.length, 'IP range') }}</span>
        </label>
        <div v-if="!selected.builtin && largeIPInclude(selected)" class="ip-warn">
          <strong>This adds {{ plural(selected.cidrs.length, 'IP rule') }}.</strong>
          Each routed range installs its own system route, so this many will slow
          down em-wall, your Mac's networking and the Rules page. The domain patterns
          usually cover the service on their own — only include IP ranges if an app
          connects by raw IP.
        </div>

        <div class="row actions">
          <button v-if="selected.builtin" class="primary" :disabled="busy" @click="applyBuiltin(selected)"
                  :title="`Open the built-in ${selected.builtinName} group on the Rules page`">
            Apply {{ selected.builtinName }}
          </button>
          <button v-else-if="confirmIPs === selected.tag" class="primary danger-fill" :disabled="busy"
                  @click="create(selected, null)">
            <span v-if="busy" class="spinner"></span>Create anyway ({{ plural(selected.cidrs.length, 'IP rule') }})
          </button>
          <button v-else class="primary" :disabled="busy" @click="create(selected, null)"
                  title="Create a custom group with these domains, then choose where it goes">
            <span v-if="busy" class="spinner"></span>Create group
          </button>
          <button v-if="confirmIPs === selected.tag" :disabled="busy"
                  @click="includeIPs[selected.tag] = false">Domains only</button>
          <button :disabled="busy" title="Hide for 7 days" @click="dismiss(selected, false)">Not now</button>
          <span style="flex: 1"></span>
          <button v-if="selected.builtin" class="quiet" :disabled="busy" @click="create(selected, null)"
                  title="Create a custom group from the geosite tag instead">Custom group instead</button>
          <button class="quiet" :disabled="busy" title="Never suggest this again (undo in Settings → Suggestions)"
                  @click="dismiss(selected, true)">Never</button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.md {
  display: grid;
  grid-template-columns: minmax(220px, 280px) 1fr;
  gap: 12px;
  align-items: start;
}
@media (max-width: 720px) { .md { grid-template-columns: 1fr; } }
.list {
  display: flex; flex-direction: column;
  border: 1px solid var(--border); border-radius: 8px; background: var(--panel);
  max-height: calc(100vh - 170px); overflow: auto; outline: none;
}
.list:focus-visible { border-color: var(--accent); }
.row-item {
  display: flex; align-items: center; gap: 8px;
  padding: 7px 10px; cursor: pointer; border-bottom: 1px solid var(--border);
}
.row-item:last-child { border-bottom: none; }
.row-item:hover { background: var(--panel-2); }
.row-item.on { background: var(--panel-2); box-shadow: inset 2px 0 0 var(--accent); }
.name { font-size: 13px; }
.sub { font-size: 11px; }
.ellipsis { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.badge {
  width: 20px; height: 20px; border-radius: 5px; flex-shrink: 0;
  display: inline-flex; align-items: center; justify-content: center;
  background: var(--border); font-size: 11px; font-weight: 600;
}
.badge.big { width: 28px; height: 28px; font-size: 14px; }
.self-dot { width: 6px; height: 6px; border-radius: 50%; background: var(--warn); flex-shrink: 0; }
.self-badge {
  font-size: 10px; padding: 2px 7px; border-radius: 10px; white-space: nowrap;
  color: var(--warn); border: 1px solid rgba(255, 200, 87, 0.4); cursor: help;
}
.detail {
  display: flex; flex-direction: column; gap: 12px;
  padding: 14px; border: 1px solid var(--border); border-radius: 8px; background: var(--panel);
  min-width: 0;
}
.label { font-size: 11px; color: var(--text-dim); margin-bottom: 4px; }
.chips { display: flex; flex-wrap: wrap; gap: 4px; align-items: center; }
.domains { margin-top: 6px; max-height: 220px; overflow: auto; }
.pat {
  font-size: 10px; background: var(--panel-2); border: 1px solid var(--border);
  border-radius: 4px; padding: 1px 5px;
}
.disclose {
  border: none; background: transparent; padding: 0; font-size: 12px;
  color: var(--text); text-align: left; cursor: pointer;
}
.disclose:hover { background: transparent; color: var(--accent); }
.caret { display: inline-block; width: 12px; color: var(--text-dim); }
.note { margin: 0; font-size: 11px; line-height: 1.5; color: var(--text-dim); }
.collision {
  font-size: 12px; padding: 6px 8px; border-radius: 6px;
  border: 1px solid var(--border); background: var(--panel-2);
}
.collision + .collision { margin-top: 4px; }
.split {
  display: flex; flex-direction: column; gap: 6px; margin-top: 6px;
  padding-top: 6px; border-top: 1px solid var(--border);
}
.ip-opt { gap: 6px; align-items: center; font-size: 12px; cursor: pointer; }
.ip-warn {
  padding: 8px 10px;
  border: 1px solid rgba(255, 200, 87, 0.35);
  border-radius: 6px;
  background: rgba(255, 200, 87, 0.08);
  color: var(--text);
  font-size: 12px;
  line-height: 1.5;
}
.ip-warn strong { color: var(--warn); }
button.danger-fill { background: var(--danger); border-color: var(--danger); }
.actions { gap: 8px; align-items: center; border-top: 1px solid var(--border); padding-top: 12px; }
button.small { padding: 3px 9px; font-size: 11px; }
button.quiet {
  border: none; background: transparent; padding: 3px 6px; font-size: 11px; color: var(--text-dim);
}
button.quiet:hover { background: transparent; color: var(--text); text-decoration: underline; }
.undo-bar {
  display: flex; align-items: center; gap: 8px; margin-bottom: 10px;
  padding: 6px 10px; border-radius: 6px; font-size: 12px;
  background: rgba(95, 208, 122, 0.08); border: 1px solid rgba(95, 208, 122, 0.4);
}
.empty {
  display: flex; flex-direction: column; gap: 6px;
  max-width: 460px; margin: 48px auto; text-align: center; line-height: 1.5;
}
</style>
