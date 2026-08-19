<script lang="ts" setup>
import { ref, computed, watch, nextTick, onBeforeUnmount } from 'vue';

// Drop-in replacement for a native <select>. Renders its own popup so the
// option list can carry a filter box, dim hints and a "missing" marker for a
// stored value whose target no longer exists.
//
// Auto-mode: the filter input only appears once the list is longer than
// `searchThreshold`, so a three-option enum still behaves like a plain menu.
//
// The popup is teleported to <body> and positioned with fixed coords taken
// from the trigger's rect — several call sites live inside scrollable table
// containers where an absolutely-positioned popup would clip.

export type SelectOption = {
  value: string;
  label: string;
  hint?: string;      // dimmed suffix, e.g. "(mtu 1400)" or "2 members"
  disabled?: boolean;
};

const props = withDefaults(
  defineProps<{
    modelValue: string;
    options: (string | SelectOption)[];
    placeholder?: string;         // shown when the value is ''
    searchThreshold?: number;     // list length above which the filter shows
    searchPlaceholder?: string;
    disabled?: boolean;
    allowEmpty?: boolean;         // offer an explicit "clear" row for ''
  }>(),
  {
    placeholder: '— select —',
    searchThreshold: 6,
    searchPlaceholder: 'search…',
    disabled: false,
    allowEmpty: false,
  },
);
const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void;
  (e: 'change', v: string): void;
}>();

const norm = computed<SelectOption[]>(() =>
  props.options.map((o) => (typeof o === 'string' ? { value: o, label: o } : o)),
);

const open = ref(false);
const query = ref('');
const active = ref(-1);
const root = ref<HTMLElement | null>(null);
const popup = ref<HTMLElement | null>(null);
const searchEl = ref<HTMLInputElement | null>(null);
const pos = ref({ top: 0, bottom: 0, left: 0, width: 0, dropUp: false });

const showSearch = computed(() => norm.value.length > props.searchThreshold);

const selected = computed(() => norm.value.find((o) => o.value === props.modelValue));
// A stored ref whose target is gone still has to stay visible and selected.
const isMissing = computed(() => !!props.modelValue && !selected.value);

const triggerLabel = computed(() => {
  if (isMissing.value) return `${props.modelValue} (missing)`;
  return selected.value ? selected.value.label : props.placeholder;
});

const filtered = computed<SelectOption[]>(() => {
  const q = query.value.trim().toLowerCase();
  if (!q) return norm.value;
  return norm.value.filter(
    (o) => o.label.toLowerCase().includes(q) || o.value.toLowerCase().includes(q),
  );
});

function place() {
  const el = root.value;
  if (!el) return;
  const r = el.getBoundingClientRect();
  const wantHeight = 300;
  const below = window.innerHeight - r.bottom;
  const dropUp = below < wantHeight && r.top > below;
  pos.value = {
    top: r.bottom + 4,
    bottom: window.innerHeight - r.top + 4,
    left: r.left,
    width: r.width,
    dropUp,
  };
}

function openPopup(seed = '') {
  if (props.disabled) return;
  query.value = seed;
  open.value = true;
  active.value = filtered.value.findIndex((o) => o.value === props.modelValue);
  place();
  nextTick(() => {
    if (showSearch.value) searchEl.value?.focus();
    scrollActiveIntoView();
  });
  window.addEventListener('scroll', place, true);
  window.addEventListener('resize', place);
}

function closePopup() {
  open.value = false;
  query.value = '';
  active.value = -1;
  window.removeEventListener('scroll', place, true);
  window.removeEventListener('resize', place);
}

function pick(o: SelectOption) {
  if (o.disabled) return;
  closePopup();
  if (o.value !== props.modelValue) {
    emit('update:modelValue', o.value);
    emit('change', o.value);
  }
  root.value?.focus();
}

function move(delta: number) {
  const list = filtered.value;
  if (!list.length) return;
  let i = active.value;
  for (let n = 0; n < list.length; n++) {
    i = (i + delta + list.length) % list.length;
    if (!list[i].disabled) break;
  }
  active.value = i;
  nextTick(scrollActiveIntoView);
}

function scrollActiveIntoView() {
  const el = popup.value?.querySelector('.ss-opt.active') as HTMLElement | null;
  el?.scrollIntoView({ block: 'nearest' });
}

function onTriggerKey(e: KeyboardEvent) {
  if (props.disabled) return;
  if (open.value) return;
  if (e.key === 'Enter' || e.key === ' ' || e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    openPopup();
    return;
  }
  // type-to-search straight from the closed trigger
  if (e.key.length === 1 && !e.metaKey && !e.ctrlKey && !e.altKey) {
    e.preventDefault();
    openPopup(e.key);
  }
}

function onPopupKey(e: KeyboardEvent) {
  if (e.key === 'Escape') { e.preventDefault(); closePopup(); root.value?.focus(); return; }
  if (e.key === 'ArrowDown') { e.preventDefault(); move(1); return; }
  if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); return; }
  if (e.key === 'Enter') {
    e.preventDefault();
    const o = filtered.value[active.value];
    if (o) pick(o);
    return;
  }
  if (e.key === 'Tab') closePopup();
}

function onDocPointer(e: PointerEvent) {
  if (!open.value) return;
  const t = e.target as Node;
  if (root.value?.contains(t) || popup.value?.contains(t)) return;
  closePopup();
}

watch(open, (v) => {
  if (v) document.addEventListener('pointerdown', onDocPointer, true);
  else document.removeEventListener('pointerdown', onDocPointer, true);
});
// Filtering invalidates the highlight index.
watch(query, () => {
  active.value = filtered.value.findIndex((o) => !o.disabled);
});

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointer, true);
  window.removeEventListener('scroll', place, true);
  window.removeEventListener('resize', place);
});
</script>

<template>
  <div ref="root"
       :class="['ss', { open, disabled }]"
       :tabindex="disabled ? -1 : 0"
       role="combobox"
       :aria-expanded="open"
       @click="open ? closePopup() : openPopup()"
       @keydown="onTriggerKey">
    <span :class="['ss-val', { ph: !modelValue, missing: isMissing }]">{{ triggerLabel }}</span>
    <span v-if="selected?.hint" class="ss-hint">{{ selected.hint }}</span>
    <span class="ss-caret">⌄</span>
  </div>

  <Teleport to="body">
    <div v-if="open"
         ref="popup"
         class="ss-pop"
         :style="{
           top: pos.dropUp ? 'auto' : pos.top + 'px',
           bottom: pos.dropUp ? pos.bottom + 'px' : 'auto',
           left: pos.left + 'px',
           minWidth: pos.width + 'px',
         }"
         @keydown="onPopupKey">
      <input v-if="showSearch"
             ref="searchEl"
             v-model="query"
             class="ss-search"
             :placeholder="searchPlaceholder"
             @keydown="onPopupKey" />
      <div class="ss-list">
        <div v-if="allowEmpty"
             :class="['ss-opt', { active: active === -1 && !modelValue }]"
             @click="pick({ value: '', label: placeholder })">
          <span class="ss-opt-label ph">{{ placeholder }}</span>
        </div>
        <div v-for="(o, i) in filtered"
             :key="o.value"
             :class="['ss-opt', { active: i === active, sel: o.value === modelValue, off: o.disabled }]"
             @mousemove="active = i"
             @click="pick(o)">
          <span class="ss-opt-label">{{ o.label }}</span>
          <span v-if="o.hint" class="ss-opt-hint">{{ o.hint }}</span>
          <span v-if="o.value === modelValue" class="ss-check">✓</span>
        </div>
        <div v-if="!filtered.length" class="ss-empty">no match</div>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.ss {
  display: flex;
  align-items: center;
  gap: 6px;
  background: var(--panel);
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 6px 10px;
  font-size: 13px;
  cursor: pointer;
  outline: none;
  min-width: 0;
  user-select: none;
}
.ss:focus, .ss.open { border-color: var(--accent); }
.ss.disabled { opacity: 0.5; cursor: not-allowed; }
.ss-val { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ss-val.ph { color: var(--text-dim); }
.ss-val.missing { color: var(--warn); }
.ss-hint { color: var(--text-dim); font-size: 11px; white-space: nowrap; }
.ss-caret { color: var(--text-dim); font-size: 12px; line-height: 1; margin-top: -4px; }

.ss-pop {
  position: fixed;
  z-index: 200;
  max-width: 520px;
  background: var(--panel-2);
  border: 1px solid var(--border);
  border-radius: 8px;
  box-shadow: 0 10px 30px rgba(0, 0, 0, 0.45);
  padding: 4px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.ss-search {
  background: var(--panel);
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 5px 8px;
  font-size: 12px;
  outline: none;
}
.ss-search:focus { border-color: var(--accent); }
.ss-list { max-height: 240px; overflow-y: auto; display: flex; flex-direction: column; }
.ss-opt {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 5px 8px;
  border-radius: 5px;
  font-size: 13px;
  cursor: pointer;
}
.ss-opt.active { background: var(--border); }
.ss-opt.sel { color: var(--accent); }
.ss-opt.off { opacity: 0.45; cursor: not-allowed; }
.ss-opt-label { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.ss-opt-label.ph { color: var(--text-dim); }
.ss-opt-hint { color: var(--text-dim); font-size: 11px; white-space: nowrap; }
.ss-check { color: var(--accent); font-size: 11px; }
.ss-empty { padding: 8px; color: var(--text-dim); font-size: 12px; text-align: center; }
</style>
