<script lang="ts" setup>
import { computed } from 'vue';
import { SECTIONS, sectionRun, newRun, newSteps, sectionSeen, type TourRun } from '../tour';

// The tour's section picker. `dialog` is the first-launch welcome (centered,
// dimmed); otherwise it pops up from the sidebar's Tour button. `seenSeq`
// changes whenever App marks steps seen, so progress recomputes (the seen
// set lives in localStorage, outside Vue).
const props = defineProps<{ seenSeq: number; dialog?: boolean }>();
const emit = defineEmits<{ (e: 'pick', run: TourRun): void; (e: 'close'): void }>();

const fresh = computed(() => (props.seenSeq, newSteps().length));
const rows = computed(() => (props.seenSeq, SECTIONS.map((s) => ({
  id: s.id, title: s.title, blurb: s.blurb, icon: s.icon, steps: s.steps.length, seen: sectionSeen(s),
}))));
// The first section not finished yet: where "in order" continues.
const nextUp = computed(() => rows.value.find((r) => r.seen < r.steps)?.id);
const nextTitle = computed(() => rows.value.find((r) => r.id === nextUp.value)?.title || rows.value[0].title);

function pick(run: TourRun | null) { if (run) emit('pick', run); }
</script>

<template>
  <div class="tm-catch" :class="{ dim: dialog }" @click="emit('close')"></div>
  <div class="tm" :class="{ dialog }" role="dialog" aria-label="Tour sections">
    <div v-if="dialog" class="tm-head">
      <h3>Welcome to em-wall</h3>
      <p>A short guide in {{ rows.length }} sections. Each takes a minute. Go in order, or pick the part you need. They are always here under <b>Tour</b> at the bottom of the sidebar.</p>
    </div>
    <button v-if="fresh" class="tm-item tm-new" @click="pick(newRun())">
      <span class="tm-title">What’s new <span class="tm-badge">{{ fresh }}</span></span>
      <span class="tm-sub">Only the parts added since your last tour</span>
    </button>
    <button v-for="(r, i) in rows" :key="r.id" class="tm-item" :class="{ next: r.id === nextUp }"
            @click="pick(sectionRun(r.id))">
      <svg class="tm-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path :d="r.icon" /></svg>
      <span class="tm-text">
        <span class="tm-title">
          <span class="tm-num">{{ i + 1 }}</span>{{ r.title }}
          <span v-if="r.id === nextUp" class="tm-pill">{{ r.seen ? 'Continue' : i === 0 ? 'Start here' : 'Next' }}</span>
        </span>
        <span class="tm-sub">{{ r.blurb }}</span>
      </span>
      <span class="tm-state" :class="{ done: r.seen >= r.steps }">
        {{ r.seen >= r.steps ? '✓' : r.seen ? `${r.seen}/${r.steps}` : `${r.steps} steps` }}
      </span>
    </button>
    <div v-if="dialog" class="tm-foot">
      <button class="tm-later" @click="emit('close')">Maybe later</button>
      <button class="primary" @click="pick(sectionRun(nextUp || 'basics'))">Start with {{ nextTitle }}</button>
    </div>
  </div>
</template>

<style scoped>
.tm-catch { position: fixed; inset: 0; z-index: 900; }
.tm-catch.dim { background: rgba(6, 6, 10, 0.66); }
.tm {
  position: fixed;
  left: 176px;
  bottom: 12px;
  z-index: 901;
  width: 340px;
  max-height: calc(100vh - 24px);
  overflow-y: auto;
  padding: 6px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 10px;
  box-shadow: 0 14px 40px rgba(0, 0, 0, 0.5);
}
.tm.dialog { left: 50%; top: 50%; bottom: auto; transform: translate(-50%, -50%); width: 460px; max-width: calc(100vw - 24px); padding: 14px; }
.tm-head { padding: 2px 6px 10px; }
.tm-head h3 { margin: 0 0 6px; font-size: 16px; }
.tm-head p { margin: 0; line-height: 1.5; color: #c9c9d4; }
.tm-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 8px 10px;
  border: 1px solid transparent;
  border-radius: 8px;
  background: transparent;
  text-align: left;
}
.tm-item:hover { background: var(--panel-2); }
.tm-item.next { border-color: rgba(110, 168, 255, 0.35); background: rgba(110, 168, 255, 0.06); }
.tm-new { flex-direction: column; align-items: flex-start; gap: 2px; margin-bottom: 4px; border-color: var(--border); }
.tm-icon { width: 18px; height: 18px; flex-shrink: 0; color: var(--accent); }
.tm-text { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.tm-title { display: flex; align-items: center; gap: 6px; font-weight: 600; }
.tm-sub { font-size: 11px; color: var(--text-dim); }
.tm-num {
  display: inline-flex; align-items: center; justify-content: center;
  width: 16px; height: 16px; border-radius: 50%; background: var(--panel-2);
  font-size: 10px; color: var(--text-dim);
}
.tm-pill {
  padding: 0 6px; border-radius: 8px; background: rgba(110, 168, 255, 0.15); color: var(--accent);
  font-size: 10px; font-weight: 700; line-height: 15px;
}
.tm-badge {
  padding: 0 6px; border-radius: 8px; background: var(--accent); color: #fff;
  font-size: 10px; font-weight: 700; line-height: 15px;
}
.tm-state { font-size: 11px; color: var(--text-dim); white-space: nowrap; font-variant-numeric: tabular-nums; }
.tm-state.done { color: var(--success); font-size: 13px; }
.tm-foot { display: flex; justify-content: space-between; align-items: center; margin-top: 10px; padding: 0 4px; }
.tm-later { background: transparent; border: none; color: var(--text-dim); padding: 6px 0; }
.tm-later:hover { background: transparent; color: var(--text); }
</style>
