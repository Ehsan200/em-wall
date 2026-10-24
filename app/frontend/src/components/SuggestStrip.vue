<script lang="ts" setup>
import { ref, computed, onMounted, onUnmounted } from 'vue';
import { startPoll } from '../poll';
import {
  SuggestList, SuggestDismiss, SuggestAccept, SuggestSplit,
  UpdateCustomGroup, DeleteCustomGroup,
} from '../../wailsjs/go/main/App';
import type { ipc } from '../../wailsjs/go/models';
import GroupIcon from './GroupIcon.vue';

// Suggestion cards on the Rules tab: geosite tags seen in recent DNS
// traffic that no rule covers. Creating a group here never creates rules —
// the parent opens the normal apply form (with a binding preselected for
// "Use same set").
const props = defineProps<{ groups: ipc.GroupDTO[] }>();
const emit = defineEmits<{
  // A group was created: open the apply form for it, optionally preset.
  (e: 'created', key: string, preset: { action: 'block' | 'route'; iface: string } | null): void;
  // Open the apply form for an existing (built-in) group.
  (e: 'apply', key: string): void;
  // Group definitions changed (split / undo): reload groups.
  (e: 'changed'): void;
}>();

const items = ref<ipc.SuggestionDTO[]>([]);
const error = ref('');
const busy = ref<string | null>(null); // tag being worked on
const includeIPs = ref<Record<string, boolean>>({});
// Open split preview: tag + collision group key.
const splitPreview = ref<{ tag: string; groupKey: string } | null>(null);
// Last split, kept for Undo.
const lastSplit = ref<{
  name: string;
  moved: number;
  result: ipc.SuggestSplitResult;
} | null>(null);
const undoing = ref(false);

const COLLAPSE_KEY = 'em-wall.suggest.collapsed';
function readCollapsed(): boolean {
  try { return localStorage.getItem(COLLAPSE_KEY) === '1'; } catch { return false; }
}
const collapsed = ref(readCollapsed());
function toggleCollapsed() {
  collapsed.value = !collapsed.value;
  try { localStorage.setItem(COLLAPSE_KEY, collapsed.value ? '1' : '0'); } catch { /* ignore */ }
}

async function load() {
  try {
    items.value = (await SuggestList()) || [];
    error.value = '';
  } catch (e: any) {
    // The daemon may predate suggestions; stay quiet rather than shout.
    items.value = [];
  }
}

let stop: (() => void) | null = null;
onMounted(() => { load(); stop = startPoll(load, 60_000); });
onUnmounted(() => stop?.());
defineExpose({ reload: load });

// ---- helpers ----

// Built-in group that shares the tag's name (SPOTIFY → spotify).
function builtinFor(s: ipc.SuggestionDTO): ipc.GroupDTO | undefined {
  const aliases: Record<string, string> = { TWITTER: 'x', FACEBOOK: 'meta', CLAUDE: 'anthropic' };
  const key = aliases[s.tag] || s.tag.toLowerCase();
  return props.groups.find(g => !g.custom && g.key === key);
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

function plural(n: number, one: string, many = one + 's'): string {
  return `${n} ${n === 1 ? one : many}`;
}

// The single binding an overlap's rules share, if they share one.
function sameBinding(c: ipc.SuggestCollisionDTO): { action: 'block' | 'route'; iface: string } | null {
  const actions = c.actions || [];
  const ifaces = c.interfaces || [];
  if (actions.length !== 1 || ifaces.length !== 1) return null;
  if (actions[0] === 'route' && ifaces[0]) return { action: 'route', iface: ifaces[0] };
  if (actions[0] === 'block') return { action: 'block', iface: '' };
  return null;
}

function bindingLabel(c: ipc.SuggestCollisionDTO): string {
  const actions = c.actions || [];
  if (!actions.length) return 'no rules yet';
  const parts: string[] = [];
  for (const i of c.interfaces || []) parts.push(i ? `via ${i}` : '');
  if (actions.includes('block')) parts.push('blocked');
  if (actions.includes('allow')) parts.push('allowed');
  return parts.filter(Boolean).join(', ') || actions.join(', ');
}

// ---- actions ----

async function create(s: ipc.SuggestionDTO, preset: { action: 'block' | 'route'; iface: string } | null) {
  busy.value = s.tag;
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
    emit('created', key, preset);
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = null;
  }
}

async function dismiss(s: ipc.SuggestionDTO, forever: boolean) {
  busy.value = s.tag;
  try {
    await SuggestDismiss(s.tag, forever);
    items.value = items.value.filter(x => x.tag !== s.tag);
    error.value = '';
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = null;
  }
}

function openSplit(s: ipc.SuggestionDTO, c: ipc.SuggestCollisionDTO) {
  const same = splitPreview.value?.tag === s.tag && splitPreview.value?.groupKey === c.groupKey;
  splitPreview.value = same ? null : { tag: s.tag, groupKey: c.groupKey };
}

async function split(s: ipc.SuggestionDTO, c: ipc.SuggestCollisionDTO) {
  busy.value = s.tag;
  try {
    const result = await SuggestSplit({
      tag: s.tag,
      displayName: s.displayName,
      patterns: c.patterns,
      fromGroups: [c.groupKey],
    } as ipc.SuggestSplitParams);
    lastSplit.value = { name: s.displayName, moved: c.patterns.length, result };
    splitPreview.value = null;
    error.value = '';
    await load();
    emit('changed');
  } catch (e: any) {
    error.value = e?.message || String(e);
  } finally {
    busy.value = null;
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

const visible = computed(() => items.value.length > 0 || lastSplit.value !== null);
</script>

<template>
  <div v-if="visible" class="suggest-strip">
    <button class="strip-header" @click="toggleCollapsed"
            :title="collapsed ? 'Show suggestions' : 'Hide suggestions'">
      <span class="caret">{{ collapsed ? '▸' : '▾' }}</span>
      <strong>Suggested groups</strong>
      <span class="muted" style="font-size: 11px">
        {{ items.length ? `${items.length} · sites you visit that no rule covers` : '' }}
      </span>
    </button>

    <div v-if="lastSplit" class="undo-bar">
      <span style="flex: 1">
        Moved {{ plural(lastSplit.moved, 'pattern') }} into <strong>{{ lastSplit.name }}</strong>.
        Rules didn't change.
      </span>
      <button :disabled="undoing" @click="undoSplit">{{ undoing ? 'Undoing…' : 'Undo' }}</button>
      <button class="icon-btn" title="Dismiss" @click="lastSplit = null">✕</button>
    </div>

    <div v-if="error" class="error" style="margin: 6px 0">{{ error }}</div>

    <div v-show="!collapsed" class="cards">
      <div v-for="s in items" :key="s.tag" class="card">
        <div class="row" style="gap: 8px; align-items: center">
          <GroupIcon v-if="builtinFor(s)" :group-key="builtinFor(s)!.key" :size="22" />
          <span v-else class="badge">{{ s.displayName.slice(0, 1) }}</span>
          <div class="col" style="gap: 1px; flex: 1; min-width: 0">
            <strong class="ellipsis">{{ s.displayName }}</strong>
            <span class="muted" style="font-size: 10px">
              geosite:{{ s.tag.toLowerCase() }} · {{ plural(s.hits, 'query', 'queries') }} · {{ ago(s.lastSeen) }}
            </span>
          </div>
        </div>

        <div class="names" :title="(s.seenNames || []).join('\n')">
          <code v-for="n in (s.seenNames || []).slice(0, 3)" :key="n" class="pat">{{ n }}</code>
          <span v-if="(s.seenNames || []).length > 3" class="muted" style="font-size: 10px">
            +{{ s.seenNames.length - 3 }} more
          </span>
        </div>

        <div class="muted" style="font-size: 11px; line-height: 1.5">
          <span :title="(s.missing || []).join('\n')">
            {{ plural((s.missing || []).length, 'domain') }} to add<template
              v-if="(s.missing || []).length < (s.patterns || []).length">
              ({{ (s.patterns || []).length - (s.missing || []).length }} already have rules)</template>
          </span>
          <span v-if="s.skipped"> · {{ s.skipped }} keyword/regex {{ s.skipped === 1 ? 'entry' : 'entries' }} can't be used</span>
        </div>

        <label v-if="(s.cidrs || []).length" class="row ip-opt">
          <input type="checkbox" v-model="includeIPs[s.tag]" />
          <span>Include {{ plural(s.cidrs.length, 'IP range') }}</span>
        </label>

        <!-- Groups that already cover part of this tag -->
        <div v-for="c in s.collisions || []" :key="c.groupKey" class="collision">
          <div class="row" style="gap: 6px; align-items: center; flex-wrap: wrap">
            <span style="flex: 1; min-width: 0">
              <strong>{{ c.groupName }}</strong>
              <span class="muted"> has {{ plural(c.patterns.length, 'of these', 'of these') }}
                · {{ bindingLabel(c) }}</span>
            </span>
            <button v-if="sameBinding(c)" class="small" :disabled="busy === s.tag"
                    @click="create(s, sameBinding(c))"
                    :title="`Create the group and apply it ${bindingLabel(c)}, like ${c.groupName}`">
              {{ sameBinding(c)!.action === 'route' ? 'Use same set' : 'Block too' }}
            </button>
            <button v-if="c.custom" class="small" :disabled="busy === s.tag"
                    @click="openSplit(s, c)">
              Split overlap…
            </button>
          </div>
          <div v-if="splitPreview?.tag === s.tag && splitPreview?.groupKey === c.groupKey" class="split-preview">
            <div style="font-size: 11px; line-height: 1.5">
              Moves {{ plural(c.patterns.length, 'pattern') }} out of <strong>{{ c.groupName }}</strong>
              into a new <strong>{{ s.displayName }}</strong> group. Rules stay as they are,
              so routing doesn't change.
            </div>
            <div class="names">
              <code v-for="p in c.patterns" :key="p" class="pat">{{ p }}</code>
            </div>
            <div class="row" style="gap: 6px; justify-content: flex-end">
              <button class="small" @click="splitPreview = null">Cancel</button>
              <button class="small primary" :disabled="busy === s.tag" @click="split(s, c)">
                Move {{ plural(c.patterns.length, 'pattern') }}
              </button>
            </div>
          </div>
        </div>

        <div class="row actions">
          <button class="primary small" :disabled="busy === s.tag" @click="create(s, null)"
                  title="Create a custom group with these domains, then choose what to do with it">
            <span v-if="busy === s.tag" class="spinner"></span>Create group
          </button>
          <button v-if="builtinFor(s)" class="small" @click="emit('apply', builtinFor(s)!.key)"
                  :title="`Apply the built-in ${builtinFor(s)!.displayName} group instead`">
            Use built-in
          </button>
          <span style="flex: 1"></span>
          <button class="small" :disabled="busy === s.tag" @click="dismiss(s, false)"
                  title="Hide for 7 days">Not now</button>
          <button class="small" :disabled="busy === s.tag" @click="dismiss(s, true)"
                  title="Never suggest this again (undo in Settings → Suggestions)">Never</button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.suggest-strip {
  margin-bottom: 12px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--panel);
}
.strip-header {
  display: flex; align-items: center; gap: 8px; width: 100%;
  border: none; background: transparent; padding: 2px 0; cursor: pointer;
  color: var(--text); text-align: left;
}
.strip-header:hover { background: transparent; }
.caret { width: 10px; color: var(--text-dim); }
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 8px;
  margin-top: 8px;
}
.card {
  display: flex; flex-direction: column; gap: 6px;
  padding: 10px; border: 1px solid var(--border); border-radius: 8px;
  background: var(--panel-2);
}
.badge {
  width: 22px; height: 22px; border-radius: 5px; flex-shrink: 0;
  display: inline-flex; align-items: center; justify-content: center;
  background: var(--border); font-size: 12px; font-weight: 600;
}
.ellipsis { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.names { display: flex; flex-wrap: wrap; gap: 4px; align-items: center; }
.pat {
  font-size: 10px; background: var(--panel); border: 1px solid var(--border);
  border-radius: 4px; padding: 1px 5px;
}
.ip-opt { gap: 6px; align-items: center; font-size: 11px; cursor: pointer; }
.collision {
  font-size: 11px; padding: 6px 8px; border-radius: 6px;
  border: 1px dashed var(--border);
}
.split-preview {
  display: flex; flex-direction: column; gap: 6px; margin-top: 6px;
  padding-top: 6px; border-top: 1px solid var(--border);
}
.actions { gap: 6px; align-items: center; margin-top: 2px; }
button.small { padding: 3px 9px; font-size: 11px; }
.undo-bar {
  display: flex; align-items: center; gap: 8px; margin-top: 8px;
  padding: 6px 10px; border-radius: 6px; font-size: 12px;
  background: rgba(95, 208, 122, 0.08); border: 1px solid rgba(95, 208, 122, 0.4);
}
.icon-btn { border: none; background: transparent; padding: 2px 6px; cursor: pointer; color: var(--text-dim); }
</style>
