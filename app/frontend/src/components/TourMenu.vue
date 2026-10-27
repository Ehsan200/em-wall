<script lang="ts" setup>
import { computed } from 'vue';
import { SECTIONS, fullRun, sectionRun, newRun, sectionUnseen, unseenSteps, type TourRun } from '../tour';

// Opened from the sidebar's Tour button: the full tour, What's new, or one
// section on its own. `seenSeq` changes whenever App marks steps seen, so
// the "new" counts recompute (seen ids live in localStorage, not in Vue).
const props = defineProps<{ seenSeq: number }>();
const emit = defineEmits<{ (e: 'pick', run: TourRun): void; (e: 'close'): void }>();

const fresh = computed(() => (props.seenSeq, unseenSteps().length));
const rows = computed(() => (props.seenSeq, SECTIONS.map((s) => ({
  id: s.id, title: s.title, blurb: s.blurb, steps: s.steps.length, unseen: sectionUnseen(s),
}))));
const total = SECTIONS.reduce((n, s) => n + s.steps.length, 0);

function pick(run: TourRun | null) { if (run) emit('pick', run); }
</script>

<template>
  <div class="tm-catch" @click="emit('close')"></div>
  <div class="tm" role="menu" aria-label="Tours">
    <button class="tm-item tm-main" role="menuitem" @click="pick(fullRun())">
      <span class="tm-title">Full tour</span>
      <span class="tm-sub">Every part of the app, in order · {{ total }} steps</span>
    </button>
    <button v-if="fresh" class="tm-item" role="menuitem" @click="pick(newRun())">
      <span class="tm-title">What’s new <span class="tm-badge">{{ fresh }}</span></span>
      <span class="tm-sub">Only the parts added since your last tour</span>
    </button>
    <div class="tm-sep">Sections</div>
    <button v-for="(r, i) in rows" :key="r.id" class="tm-item" role="menuitem" @click="pick(sectionRun(r.id))">
      <span class="tm-title">
        <span class="tm-num">{{ i + 1 }}</span>{{ r.title }}
        <span v-if="r.unseen && r.unseen < r.steps" class="tm-badge">{{ r.unseen }} new</span>
        <span v-else-if="!r.unseen" class="tm-done" title="Seen">✓</span>
      </span>
      <span class="tm-sub">{{ r.blurb }} · {{ r.steps }} steps</span>
    </button>
  </div>
</template>

<style scoped>
.tm-catch { position: fixed; inset: 0; z-index: 900; }
.tm {
  position: fixed;
  left: 176px;
  bottom: 12px;
  z-index: 901;
  width: 300px;
  padding: 6px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 10px;
  box-shadow: 0 14px 40px rgba(0, 0, 0, 0.5);
}
.tm-item {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 2px;
  width: 100%;
  padding: 7px 10px;
  border: none;
  border-radius: 6px;
  background: transparent;
  text-align: left;
}
.tm-item:hover { background: var(--panel-2); }
.tm-main .tm-title { color: var(--accent); }
.tm-title { display: flex; align-items: center; gap: 6px; font-weight: 600; }
.tm-sub { font-size: 11px; color: var(--text-dim); }
.tm-num {
  display: inline-flex; align-items: center; justify-content: center;
  width: 16px; height: 16px; border-radius: 50%; background: var(--panel-2);
  font-size: 10px; color: var(--text-dim);
}
.tm-badge {
  padding: 0 6px; border-radius: 8px; background: var(--accent); color: #fff;
  font-size: 10px; font-weight: 700; line-height: 15px;
}
.tm-done { color: var(--success); font-size: 11px; }
.tm-sep {
  margin: 6px 10px 2px; padding-top: 6px; border-top: 1px solid var(--border);
  font-size: 9.5px; font-weight: 700; letter-spacing: 0.1em; text-transform: uppercase; color: #5e5e72;
}
</style>
