<script lang="ts" setup>
import { ref, computed, watch, nextTick, onUnmounted } from 'vue';
import { setTourView, clearTourView, type TourStep, type TourRun } from '../tour';

// Guided tour. App.vue decides which run plays (full tour on first launch,
// What's new after an update, or whatever the Tour menu picked); this
// component owns the walk itself: switching tabs and sub-views,
// spotlighting the step's target, the card.
const props = defineProps<{ run: TourRun | null }>();
const emit = defineEmits<{
  (e: 'close'): void;
  // Section run finished and the user took the offered next section.
  (e: 'continue', sectionId: string): void;
  (e: 'navigate', tab: TourStep['tab']): void;
}>();

const TOUR = computed(() => props.run?.steps || []);
const index = ref(0);
const step = computed(() => TOUR.value[index.value]);
const isFirst = computed(() => index.value === 0);
const isLast = computed(() => index.value === TOUR.value.length - 1);
const chapters = computed(() => {
  const seen = new Map<string, number>();
  if (props.run?.kind !== 'full') return [];
  TOUR.value.forEach((s, i) => { if (!seen.has(s.chapter)) seen.set(s.chapter, i); });
  return [...seen].map(([title, start]) => ({ title, start }));
});

// Target box in viewport coordinates; null = centered card, full dim.
type Box = { top: number; left: number; width: number; height: number };
const box = ref<Box | null>(null);
const PAD = 6;
const GAP = 14;
const EDGE = 12;
const CARD_W = 420;

function candidates(s: TourStep): string[] {
  const list = s.target ? (Array.isArray(s.target) ? [...s.target] : [s.target]) : [];
  // Page content arrives over IPC and may be empty; the sidebar item is
  // always there.
  if (s.tab) list.push(`[data-tour="nav-${s.tab}"]`);
  return list;
}
function findTarget(): Element | null {
  for (const sel of candidates(step.value)) {
    const el = document.querySelector(sel);
    if (el && el.getClientRects().length) return el;
  }
  return null;
}

// The element scrolled into view for this step, so the periodic re-measure
// doesn't fight the user's (or the page's) scrolling.
let scrolledFor: Element | null = null;
function measure() {
  const el = findTarget();
  if (!el) { box.value = null; return; }
  if (el !== scrolledFor) {
    scrolledFor = el;
    el.scrollIntoView({ block: 'nearest' });
  }
  const r = el.getBoundingClientRect();
  // Clip to the window: a long list must not push the outline off-screen.
  const top = Math.max(EDGE / 2, r.top - PAD);
  const left = Math.max(EDGE / 2, r.left - PAD);
  const bottom = Math.min(window.innerHeight - EDGE / 2, r.bottom + PAD);
  const right = Math.min(window.innerWidth - EDGE / 2, r.right + PAD);
  const next = { top, left, width: Math.max(0, right - left), height: Math.max(0, bottom - top) };
  const b = box.value;
  if (!b || b.top !== next.top || b.left !== next.left || b.width !== next.width || b.height !== next.height) box.value = next;
}

const cardEl = ref<HTMLElement | null>(null);
const cardH = ref(320);
const cardStyle = computed(() => {
  const b = box.value;
  if (!b) return {};
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const h = Math.min(cardH.value, vh - EDGE * 2);
  const w = Math.min(CARD_W, vw - EDGE * 2);
  const clampTop = (y: number) => Math.min(Math.max(EDGE, y), vh - h - EDGE);
  const clampLeft = (x: number) => Math.min(Math.max(EDGE, x), vw - w - EDGE);
  // First side with room wins: right (sidebar items), below (bars and
  // headers), above, left. A target that fills the window gets the card
  // in the bottom-right corner over it.
  if (b.left + b.width + GAP + w <= vw - EDGE) return { top: clampTop(b.top) + 'px', left: b.left + b.width + GAP + 'px' };
  if (b.top + b.height + GAP + h <= vh - EDGE) return { top: b.top + b.height + GAP + 'px', left: clampLeft(b.left) + 'px' };
  if (b.top - GAP - h >= EDGE) return { top: b.top - GAP - h + 'px', left: clampLeft(b.left) + 'px' };
  if (b.left - GAP - w >= EDGE) return { top: clampTop(b.top) + 'px', left: b.left - GAP - w + 'px' };
  return { top: vh - h - EDGE + 'px', left: vw - w - EDGE + 'px' };
});

async function show(i: number) {
  index.value = Math.max(0, Math.min(TOUR.value.length - 1, i));
  scrolledFor = null;
  const s = step.value;
  if (s.tab) {
    emit('navigate', s.tab);
    if (s.view) setTourView(s.tab, s.view);
  }
  await nextTick();
  measure();
  await nextTick();
  cardH.value = cardEl.value?.offsetHeight || cardH.value;
}

function next() { if (isLast.value) emit('close'); else show(index.value + 1); }
function back() { if (!isFirst.value) show(index.value - 1); }

function onKey(e: KeyboardEvent) {
  if (e.key === 'Escape') { e.preventDefault(); emit('close'); }
  else if (e.key === 'ArrowRight' || e.key === 'Enter') { e.preventDefault(); next(); }
  else if (e.key === 'ArrowLeft') { e.preventDefault(); back(); }
}
function onResize() { measure(); }

// While open, re-measure on a short tick: the target may appear only once
// the page's data loads, and lists reflow as rows arrive.
let tick: number | undefined;
function start() {
  window.addEventListener('keydown', onKey);
  window.addEventListener('resize', onResize);
  tick = window.setInterval(() => {
    measure();
    if (cardEl.value) cardH.value = cardEl.value.offsetHeight;
  }, 250);
  show(0);
}
function stop() {
  window.removeEventListener('keydown', onKey);
  window.removeEventListener('resize', onResize);
  if (tick) window.clearInterval(tick);
  tick = undefined;
  clearTourView();
}
// A new run (say, the next section) restarts the walk from its first card.
watch(() => props.run, (r, old) => {
  if (r && !old) start();
  else if (r) show(0);
  else if (old) stop();
}, { immediate: true });
onUnmounted(stop);
</script>

<template>
  <div v-if="run && step" class="tour" role="dialog" aria-modal="true" :aria-label="'Tour: ' + step.title">
    <!-- Click-catcher: the app underneath stays visible but inert. -->
    <div class="tour-scrim" :class="{ dim: !box }"></div>
    <div v-if="box" class="tour-spot" :class="{ danger: step.danger }"
         :style="{ top: box.top + 'px', left: box.left + 'px', width: box.width + 'px', height: box.height + 'px' }"></div>

    <div ref="cardEl" class="tour-card" :class="{ centered: !box, danger: step.danger }" :style="cardStyle">
      <div class="tour-head">
        <span class="tour-chapter"><template v-if="step.danger">⚠ Important · </template>{{ step.chapter }}</span>
        <span class="tour-count">{{ index + 1 }} / {{ TOUR.length }}</span>
      </div>
      <h3 class="tour-title">{{ step.title }}</h3>

      <p v-for="(p, i) in step.body" :key="i" class="tour-p" v-html="p"></p>

      <div v-if="step.flow" class="tour-flow" aria-hidden="true">
        <template v-for="(n, i) in step.flow" :key="i">
          <span v-if="i > 0" class="tour-arrow">→</span>
          <span class="tour-node" :class="n.tone ? 'tone-' + n.tone : ''">
            <span>{{ n.label }}</span>
            <small v-if="n.sub">{{ n.sub }}</small>
          </span>
        </template>
      </div>

      <dl v-if="step.terms" class="tour-terms">
        <template v-for="t in step.terms" :key="t.term">
          <dt><code>{{ t.term }}</code></dt>
          <dd v-html="t.def"></dd>
        </template>
      </dl>

      <div v-if="step.tip" class="tour-tip" v-html="step.tip"></div>

      <!-- Chapter jump list on the first card, so a returning user can go
           straight to the part they came back for. -->
      <div v-if="isFirst && chapters.length > 1" class="tour-chapters">
        <button v-for="c in chapters.slice(1)" :key="c.title" class="tour-chip" @click="show(c.start)">{{ c.title }}</button>
      </div>

      <div class="tour-dots" aria-hidden="true">
        <button v-for="(s, i) in TOUR" :key="s.id" class="tour-dot"
                :class="{ on: i === index, done: i < index }" :title="s.title" @click="show(i)"></button>
      </div>

      <div class="tour-foot">
        <button class="tour-skip" @click="emit('close')">{{ isLast ? 'Close' : 'Skip tour' }}</button>
        <div class="row" style="gap: 6px">
          <button v-if="!isFirst" @click="back">Back</button>
          <button v-if="isLast && run.next" class="primary" @click="emit('continue', run.next.id)">Next: {{ run.next.title }} →</button>
          <button v-else class="primary" @click="next">{{ isLast ? 'Finish' : isFirst ? 'Start' : 'Next' }}</button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.tour { position: fixed; inset: 0; z-index: 1000; }
.tour-scrim { position: absolute; inset: 0; }
.tour-scrim.dim { background: rgba(6, 6, 10, 0.66); }
/* The spotlight dims everything around the target with one huge shadow. */
.tour-spot {
  position: absolute;
  border-radius: 8px;
  box-shadow: 0 0 0 9999px rgba(6, 6, 10, 0.66), 0 0 0 2px var(--accent);
  pointer-events: none;
  transition: top 0.2s ease, left 0.2s ease, width 0.2s ease, height 0.2s ease;
}
.tour-card {
  position: absolute;
  width: 420px;
  max-width: calc(100vw - 24px);
  max-height: calc(100vh - 24px);
  overflow-y: auto;
  padding: 16px 18px 14px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 10px;
  box-shadow: 0 18px 50px rgba(0, 0, 0, 0.5);
  transition: top 0.2s ease, left 0.2s ease;
}
.tour-spot.danger { box-shadow: 0 0 0 9999px rgba(6, 6, 10, 0.66), 0 0 0 2px var(--danger); }
.tour-card.danger { border-color: rgba(255, 111, 111, 0.6); box-shadow: 0 18px 50px rgba(0, 0, 0, 0.5), 0 0 0 1px rgba(255, 111, 111, 0.25); }
.tour-card.danger .tour-chapter { color: var(--danger); }
.tour-card.danger .tour-p:first-of-type :deep(b) { color: var(--danger); font-size: 14px; }
.tour-card.danger .tour-tip { background: rgba(255, 111, 111, 0.1); border-left-color: var(--danger); }
.tour-card.centered { top: 50%; left: 50%; transform: translate(-50%, -50%); width: 500px; }
.tour-head { display: flex; justify-content: space-between; align-items: center; }
.tour-chapter {
  font-size: 10px; font-weight: 700; letter-spacing: 0.1em; text-transform: uppercase; color: var(--accent);
}
.tour-count { font-size: 11px; color: var(--text-dim); font-variant-numeric: tabular-nums; }
.tour-title { margin: 6px 0 8px; font-size: 16px; font-weight: 600; }
.tour-p { margin: 0 0 8px; line-height: 1.55; color: #c9c9d4; }
.tour-p :deep(code), .tour-terms :deep(code), .tour-tip :deep(code) {
  font-size: 11.5px; padding: 1px 5px; border-radius: 4px; background: var(--panel-2); color: var(--text);
}
.tour-p :deep(b), .tour-terms :deep(b) { color: var(--text); }

.tour-flow {
  display: flex; flex-wrap: wrap; align-items: center; gap: 6px;
  margin: 10px 0; padding: 10px; border-radius: 8px; background: var(--bg); border: 1px solid var(--border);
}
.tour-arrow { color: var(--text-dim); }
.tour-node {
  display: flex; flex-direction: column; padding: 4px 8px; border-radius: 6px;
  background: var(--panel-2); border: 1px solid var(--border); font-size: 12px; font-weight: 600;
}
.tour-node small { font-size: 10px; font-weight: 400; color: var(--text-dim); }
.tour-node.tone-accent { border-color: rgba(110, 168, 255, 0.5); color: var(--accent); }
.tour-node.tone-danger { border-color: rgba(255, 111, 111, 0.5); color: var(--danger); }
.tour-node.tone-success { border-color: rgba(95, 208, 122, 0.5); color: var(--success); }
.tour-node.tone-warn { border-color: rgba(255, 200, 87, 0.5); color: var(--warn); }

.tour-terms { display: grid; grid-template-columns: auto 1fr; gap: 6px 10px; margin: 8px 0; font-size: 12px; }
.tour-terms dt { white-space: nowrap; }
.tour-terms dd { margin: 0; line-height: 1.45; color: #c9c9d4; }

.tour-tip {
  margin: 8px 0 4px; padding: 8px 10px; border-radius: 6px; font-size: 12px; line-height: 1.5;
  background: rgba(110, 168, 255, 0.08); border-left: 2px solid var(--accent); color: #c9c9d4;
}
.tour-chapters { display: flex; flex-wrap: wrap; gap: 6px; margin: 10px 0 2px; }
.tour-chip { padding: 3px 10px; border-radius: 12px; font-size: 11.5px; }

.tour-dots { display: flex; flex-wrap: wrap; gap: 4px; margin: 14px 0 10px; }
.tour-dot {
  width: 8px; height: 8px; padding: 0; border-radius: 50%; border: none; background: var(--border);
}
.tour-dot.done { background: rgba(110, 168, 255, 0.45); }
.tour-dot.on { background: var(--accent); width: 18px; border-radius: 4px; }
.tour-foot { display: flex; justify-content: space-between; align-items: center; }
.tour-skip { background: transparent; border: none; color: var(--text-dim); padding: 6px 0; }
.tour-skip:hover { background: transparent; color: var(--text); }
</style>
