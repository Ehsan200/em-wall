<script lang="ts" setup>
import { ref, shallowRef, computed, watch, onMounted, onUnmounted, nextTick } from 'vue';
import { geoNaturalEarth1, geoPath } from 'd3-geo';
import type { GeoProjection, GeoPermissibleObjects } from 'd3-geo';
import { feature, mesh } from 'topojson-client';
import type { Topology, GeometryCollection } from 'topojson-specification';
import GroupIcon from '../GroupIcon.vue';
import HopCard from './HopCard.vue';
import { buildFlowGeom, buildClusters, fitBox, pointAt } from './geometry';
import type { Cluster, FlowGeom, Track } from './geometry';
import { fmtRate } from './types';
import type { ViewFlow } from './types';

// SVG world map of live flows. The SVG (land, arcs, markers) is re-rendered
// only when flows, size or the view change; the group icons travel in an
// HTML overlay that a requestAnimationFrame loop moves by writing
// transforms directly, so a frame never goes through Vue.

const props = defineProps<{
  flows: ViewFlow[];
  active: string[]; // highlighted flow keys (hover from either side, or selection)
  fit: boolean; // zoom to the hops instead of the whole world
  reducedMotion: boolean;
}>();

const emit = defineEmits<{
  (e: 'hover', keys: string[]): void;
  (e: 'select', key: string | null): void;
}>();

// ---- size ----
const root = ref<HTMLDivElement | null>(null);
const size = ref({ w: 0, h: 0 });
let ro: ResizeObserver | undefined;

// ---- base map ----
type CountriesTopo = Topology<{ countries: GeometryCollection }>;
const world = shallowRef<CountriesTopo | null>(null);

// ---- view box: only moves when the rounded hop extent changes ----
const box = ref<[number, number, number, number] | null>(null);
watch(
  () => (props.fit ? JSON.stringify(fitBox(props.flows.map((f) => f.flow))) : 'null'),
  (k) => { box.value = JSON.parse(k); },
  { immediate: true },
);

function boxShape(b: [number, number, number, number]): GeoPermissibleObjects {
  const [x0, y0, x1, y1] = b;
  const pts: [number, number][] = [];
  for (let x = x0; x <= x1; x += Math.max(1, (x1 - x0) / 8)) pts.push([x, y0], [x, y1]);
  for (let y = y0; y <= y1; y += Math.max(1, (y1 - y0) / 6)) pts.push([x0, y], [x1, y]);
  return { type: 'MultiPoint', coordinates: pts };
}

// ---- pan / zoom ----
// The user's view on top of the fitted projection: screen = k·base + (x, y),
// where base is where the fitted projection puts a point. Applied to the
// projection itself (not a CSS transform) so strokes, dots and icons stay
// their real size at every zoom.
const MIN_K = 1, MAX_K = 16;
const view = ref({ k: 1, x: 0, y: 0 });
function clampView(v: { k: number; x: number; y: number }) {
  const { w, h } = size.value;
  const k = Math.min(MAX_K, Math.max(MIN_K, v.k));
  // Keep at least half a screen of map in view.
  const x = Math.min(w / 2, Math.max((1 - k) * w - w / 2, v.x));
  const y = Math.min(h / 2, Math.max((1 - k) * h - h / 2, v.y));
  return { k, x, y };
}
function zoomAt(mx: number, my: number, f: number) {
  const v = view.value;
  const k = Math.min(MAX_K, Math.max(MIN_K, v.k * f));
  const r = k / v.k;
  view.value = clampView({ k, x: mx - r * (mx - v.x), y: my - r * (my - v.y) });
}
function zoomBy(f: number) {
  zoomAt(size.value.w / 2, size.value.h / 2, f);
}
function resetView() {
  view.value = { k: 1, x: 0, y: 0 };
}
// A new fit target (Fit/World toggle, hop extent moved) starts from its own
// framing again.
watch(box, resetView);

function onWheel(ev: WheelEvent) {
  ev.preventDefault();
  const { x, y } = localXY(ev);
  if (ev.ctrlKey) {
    // Trackpad pinch (and ctrl + wheel) arrive as ctrlKey wheel events.
    zoomAt(x, y, Math.exp(-ev.deltaY * 0.01));
  } else {
    const v = view.value;
    view.value = clampView({ k: v.k, x: v.x - ev.deltaX, y: v.y - ev.deltaY });
  }
}

let drag: { id: number; sx: number; sy: number; vx: number; vy: number; moved: boolean } | null = null;
let suppressClick = false;
const dragging = ref(false);
function onPointerDown(ev: PointerEvent) {
  if (ev.button !== 0) return;
  drag = { id: ev.pointerId, sx: ev.clientX, sy: ev.clientY, vx: view.value.x, vy: view.value.y, moved: false };
}
function onPointerMove(ev: PointerEvent) {
  if (!drag || ev.pointerId !== drag.id) return;
  const dx = ev.clientX - drag.sx, dy = ev.clientY - drag.sy;
  if (!drag.moved && Math.hypot(dx, dy) < 4) return;
  if (!drag.moved) {
    drag.moved = true;
    dragging.value = true;
    root.value?.setPointerCapture(ev.pointerId);
  }
  view.value = clampView({ k: view.value.k, x: drag.vx + dx, y: drag.vy + dy });
}
function onPointerUp(ev: PointerEvent) {
  if (!drag || ev.pointerId !== drag.id) return;
  // A drag must not also count as a click on whatever it ended over.
  suppressClick = drag.moved;
  drag = null;
  dragging.value = false;
}
function onClickCapture(ev: MouseEvent) {
  if (suppressClick) {
    suppressClick = false;
    ev.stopPropagation();
    ev.preventDefault();
  }
}
function onDblClick(ev: MouseEvent) {
  const { x, y } = localXY(ev);
  zoomAt(x, y, ev.shiftKey ? 0.5 : 2);
}

const projection = computed<GeoProjection | null>(() => {
  const { w, h } = size.value;
  if (w < 50 || h < 50) return null;
  const p = geoNaturalEarth1();
  if (box.value) p.fitExtent([[28, 28], [w - 28, h - 28]], boxShape(box.value));
  else p.fitExtent([[8, 8], [w - 8, h - 8]], { type: 'Sphere' });
  const { k, x, y } = view.value;
  if (k !== 1 || x !== 0 || y !== 0) {
    const [tx, ty] = p.translate();
    p.scale(p.scale() * k).translate([tx * k + x, ty * k + y]);
  }
  return p.clipExtent([[-2, -2], [w + 2, h + 2]]);
});

const base = computed(() => {
  const p = projection.value;
  const topo = world.value;
  if (!p) return { sphere: '', land: '', borders: '' };
  const path = geoPath(p);
  const sphere = path({ type: 'Sphere' }) || '';
  if (!topo) return { sphere, land: '', borders: '' };
  const obj = topo.objects.countries;
  return {
    sphere,
    land: path(feature(topo, obj)) || '',
    borders: path(mesh(topo, obj, (a, b) => a !== b)) || '',
  };
});

// ---- flows ----
const geoms = computed(() => {
  const p = projection.value;
  const m = new Map<string, FlowGeom>();
  if (!p) return m;
  for (const vf of props.flows) m.set(vf.key, buildFlowGeom(vf.flow.hops ?? [], p, size.value.w));
  return m;
});

const clusters = computed<Cluster[]>(() => {
  const p = projection.value;
  return p ? buildClusters(props.flows, p) : [];
});

const activeSet = computed(() => new Set(props.active));

// Quietest first, so the busiest arcs draw on top.
const drawOrder = computed(() => [...props.flows].sort((a, b) => a.rate - b.rate));

function strokeWidth(rate: number): number {
  return Math.min(6, 1.2 + 0.8 * Math.log2(1 + rate / 16384));
}
function speed(rate: number): number {
  if (rate < 1) return 10;
  return Math.min(220, 26 + 22 * Math.log2(1 + rate / 8192));
}

function clusterRadius(c: Cluster): number {
  return 4 + Math.min(6, 2 * Math.log2(c.hops.length));
}

// ---- hover / card ----
const hoverCluster = ref<string | null>(null);
const pinnedCluster = ref<string | null>(null);
const cardCluster = computed(() => {
  const k = pinnedCluster.value ?? hoverCluster.value;
  return k ? clusters.value.find((c) => c.key === k) ?? null : null;
});
const cardPos = computed(() => {
  const c = cardCluster.value;
  if (!c) return { left: '0px', top: '0px' };
  const W = 290, H = 300;
  let left = c.x + 14;
  if (left + W > size.value.w - 8) left = c.x - 14 - W;
  let top = c.y - 20;
  if (top + H > size.value.h - 8) top = size.value.h - 8 - H;
  return { left: `${Math.max(8, left)}px`, top: `${Math.max(8, top)}px` };
});

function enterCluster(c: Cluster) {
  hoverCluster.value = c.key;
  emit('hover', c.flows);
}
function leaveCluster() {
  hoverCluster.value = null;
  emit('hover', []);
}
function clickCluster(c: Cluster) {
  pinnedCluster.value = pinnedCluster.value === c.key ? null : c.key;
}
// Drop a pin whose cluster vanished with its flows.
watch(clusters, (cs) => {
  if (pinnedCluster.value && !cs.some((c) => c.key === pinnedCluster.value)) pinnedCluster.value = null;
});

const tip = ref<{ key: string; x: number; y: number } | null>(null);
const tipFlow = computed(() => (tip.value ? props.flows.find((f) => f.key === tip.value!.key) ?? null : null));

function localXY(ev: MouseEvent): { x: number; y: number } {
  const r = root.value?.getBoundingClientRect();
  return { x: ev.clientX - (r?.left ?? 0), y: ev.clientY - (r?.top ?? 0) };
}
function enterFlow(vf: ViewFlow, ev: MouseEvent) {
  tip.value = { key: vf.key, ...localXY(ev) };
  emit('hover', [vf.key]);
}
function moveFlow(ev: MouseEvent) {
  if (tip.value) tip.value = { key: tip.value.key, ...localXY(ev) };
}
function leaveFlow() {
  tip.value = null;
  emit('hover', []);
}
function clickBackground() {
  pinnedCluster.value = null;
  emit('select', null);
}

const tipStyle = computed(() => {
  if (!tip.value) return {};
  const left = Math.min(tip.value.x + 14, size.value.w - 250);
  return { left: `${Math.max(4, left)}px`, top: `${tip.value.y + 14}px` };
});

// ---- animation ----
interface Anim { el: HTMLElement | null; track: Track | null; speed: number; t: number }
const anims = new Map<string, Anim>();
const pt = { x: 0, y: 0 };
let raf = 0;
let lastTs = 0;

function syncAnims() {
  const keep = new Set<string>();
  for (const vf of props.flows) {
    keep.add(vf.key);
    const g = geoms.value.get(vf.key);
    let a = anims.get(vf.key);
    if (!a) {
      a = { el: null, track: null, speed: 0, t: -1 };
      anims.set(vf.key, a);
    }
    a.track = g?.track ?? null;
    a.speed = speed(vf.rate);
    if (a.track && a.t < 0) a.t = Math.random() * a.track.len;
  }
  for (const k of [...anims.keys()]) if (!keep.has(k)) anims.delete(k);
}

function place(a: Anim) {
  if (!a.el || !a.track) return;
  pointAt(a.track, a.t, pt);
  a.el.style.transform = `translate3d(${pt.x.toFixed(1)}px, ${pt.y.toFixed(1)}px, 0)`;
}

function setTokenEl(key: string, el: unknown) {
  let a = anims.get(key);
  if (!a) {
    a = { el: null, track: null, speed: 0, t: -1 };
    anims.set(key, a);
  }
  a.el = (el as HTMLElement | null) ?? null;
  place(a);
}

function frame(ts: number) {
  const dt = lastTs ? Math.min(0.1, (ts - lastTs) / 1000) : 0;
  lastTs = ts;
  for (const a of anims.values()) {
    if (!a.track || !a.el) continue;
    a.t = (a.t + a.speed * dt) % a.track.len;
    place(a);
  }
  raf = requestAnimationFrame(frame);
}

function startAnim() {
  if (raf || props.reducedMotion || document.hidden) return;
  lastTs = 0;
  raf = requestAnimationFrame(frame);
}
function stopAnim() {
  if (raf) cancelAnimationFrame(raf);
  raf = 0;
}
function onVisibility() {
  if (document.hidden) stopAnim(); else startAnim();
}

watch([() => props.flows, geoms], () => {
  syncAnims();
  for (const a of anims.values()) place(a);
}, { immediate: true });
watch(() => props.reducedMotion, (r) => { if (r) stopAnim(); else nextTick(startAnim); });

onMounted(async () => {
  if (root.value) {
    ro = new ResizeObserver((entries) => {
      const r = entries[0].contentRect;
      const w = Math.round(r.width), h = Math.round(r.height);
      if (w !== size.value.w || h !== size.value.h) size.value = { w, h };
    });
    ro.observe(root.value);
  }
  document.addEventListener('visibilitychange', onVisibility);
  startAnim();
  // Bundled with the app (no CDN: the app must work offline); a separate
  // chunk so it loads only when the Map tab opens.
  const mod = await import('world-atlas/countries-110m.json');
  world.value = ((mod as any).default ?? mod) as CountriesTopo;
});
onUnmounted(() => {
  ro?.disconnect();
  stopAnim();
  document.removeEventListener('visibilitychange', onVisibility);
});
</script>

<template>
  <div ref="root" class="map" :class="{ 'has-active': active.length > 0, dragging }"
       @wheel="onWheel" @pointerdown="onPointerDown" @pointermove="onPointerMove"
       @pointerup="onPointerUp" @pointercancel="onPointerUp" @click.capture="onClickCapture"
       @dblclick.self="onDblClick">
    <svg v-if="projection" :width="size.w" :height="size.h" :viewBox="`0 0 ${size.w} ${size.h}`"
         @click.self="clickBackground" @dblclick.self="onDblClick">
      <path class="sphere" :class="{ outline: !box }" :d="base.sphere" @click="clickBackground" @dblclick="onDblClick" />
      <path class="land" :d="base.land" @click="clickBackground" @dblclick="onDblClick" />
      <path class="borders" :d="base.borders" />

      <g class="arcs">
        <g v-for="vf in drawOrder" :key="vf.key" class="flow"
           :class="{ active: activeSet.has(vf.key) }"
           :style="{ '--c': vf.color }">
          <path v-for="(s, i) in geoms.get(vf.key)?.segments ?? []" :key="i" :d="s.d"
                class="arc" :class="{ dashed: s.dashed }"
                :stroke-width="strokeWidth(vf.rate)" />
          <template v-if="reducedMotion">
            <path v-for="(a, i) in geoms.get(vf.key)?.arrows ?? []" :key="'a' + i" class="arrow"
                  d="M -5 -4 L 5 0 L -5 4 Z"
                  :transform="`translate(${a.x.toFixed(1)} ${a.y.toFixed(1)}) rotate(${a.a.toFixed(0)}) scale(${Math.max(1, strokeWidth(vf.rate) / 2).toFixed(2)})`" />
          </template>
          <path class="hit" :d="geoms.get(vf.key)?.hitD || ''"
                @mouseenter="enterFlow(vf, $event)" @mousemove="moveFlow" @mouseleave="leaveFlow"
                @click.stop="emit('select', vf.key)" />
        </g>
      </g>

      <g class="clusters">
        <g v-for="c in clusters" :key="c.key" class="cluster"
           :class="['r-' + c.role, { country: c.countryOnly, uncertain: c.uncertain, pinned: pinnedCluster === c.key,
                    active: c.flows.some((k) => activeSet.has(k)) }]"
           :transform="`translate(${c.x.toFixed(1)} ${c.y.toFixed(1)})`"
           tabindex="0" role="button" :aria-label="`${c.place}: ${c.hops.length} hops`"
           @mouseenter="enterCluster(c)" @mouseleave="leaveCluster" @click.stop="clickCluster(c)"
           @focus="enterCluster(c)" @blur="leaveCluster" @keydown.enter.prevent="clickCluster(c)">
          <circle v-if="c.anycast" class="anycast-ring" :r="clusterRadius(c) + 4" />
          <circle class="dot" :r="clusterRadius(c)" />
          <text v-if="c.hops.length > 1" class="count" dy="0.35em">{{ c.hops.length }}</text>
          <text v-if="c.role === 'you'" class="you-label" :x="clusterRadius(c) + 5" dy="0.35em">You</text>
        </g>
      </g>
    </svg>

    <div v-if="!reducedMotion" class="tokens" aria-hidden="true">
      <template v-for="vf in flows" :key="vf.key">
        <div v-if="geoms.get(vf.key)?.track" :ref="(el) => setTokenEl(vf.key, el)"
             class="token" :class="{ active: activeSet.has(vf.key), idle: vf.rate < 1 }"
             :style="{ '--c': vf.color }"
             @mouseenter="enterFlow(vf, $event)" @mouseleave="leaveFlow" @click.stop="emit('select', vf.key)">
          <GroupIcon v-if="vf.flow.group" :groupKey="vf.flow.group" :size="16" />
          <span v-else class="nogroup"></span>
        </div>
      </template>
    </div>

    <div class="zoom" @pointerdown.stop @dblclick.stop>
      <button title="Zoom in" aria-label="Zoom in" @click="zoomBy(1.6)">+</button>
      <button title="Zoom out" aria-label="Zoom out" :disabled="view.k <= MIN_K" @click="zoomBy(1 / 1.6)">−</button>
      <button title="Reset view" aria-label="Reset view" :disabled="view.k === 1 && view.x === 0 && view.y === 0" @click="resetView">⟲</button>
    </div>

    <div v-if="tipFlow && !cardCluster" class="tip" :style="tipStyle">
      <strong>{{ tipFlow.label }}</strong>
      <span class="muted">{{ tipFlow.flow.iface }}<template v-if="tipFlow.flow.upstream"> · {{ tipFlow.flow.upstream }}</template></span>
      <span>{{ (tipFlow.flow.hosts ?? []).slice(0, 2).join(', ') }}<template v-if="(tipFlow.flow.hosts ?? []).length > 2"> +{{ (tipFlow.flow.hosts ?? []).length - 2 }}</template></span>
      <span class="muted">↓ {{ fmtRate(tipFlow.flow.rateDown) }} · ↑ {{ fmtRate(tipFlow.flow.rateUp) }} · {{ tipFlow.flow.conns }} conn</span>
    </div>

    <div v-if="cardCluster" class="card-wrap" :style="cardPos"
         @mouseenter="hoverCluster = cardCluster.key" @mouseleave="hoverCluster = null">
      <HopCard :cluster="cardCluster" :pinned="pinnedCluster === cardCluster.key"
               :flowLabel="(k: string) => flows.find((f) => f.key === k)?.label ?? k" />
    </div>
  </div>
</template>

<style scoped>
.map {
  position: relative;
  width: 100%;
  height: 100%;
  min-height: 0;
  overflow: hidden;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 8px;
  user-select: none;
  cursor: grab;
  touch-action: none;
}
.map.dragging, .map.dragging * { cursor: grabbing !important; }
.zoom {
  position: absolute;
  left: 10px;
  bottom: 10px;
  z-index: 3;
  display: flex;
  flex-direction: column;
  border: 1px solid var(--border);
  border-radius: 6px;
  overflow: hidden;
  background: var(--panel);
}
.zoom button {
  width: 28px;
  height: 26px;
  padding: 0;
  border: none;
  border-radius: 0;
  background: transparent;
  font-size: 15px;
  line-height: 1;
  cursor: pointer;
}
.zoom button + button { border-top: 1px solid var(--border); }
.zoom button:disabled { opacity: 0.35; cursor: default; }
svg { display: block; position: absolute; inset: 0; }
.sphere { fill: var(--map-ocean); stroke: none; }
.sphere.outline { stroke: var(--border); stroke-width: 1; }
.land { fill: var(--map-land); }
.borders { fill: none; stroke: var(--map-border); stroke-width: 0.5; pointer-events: none; }

.flow { transition: opacity 0.15s ease; }
.arc {
  fill: none;
  stroke: var(--c);
  stroke-linecap: round;
  stroke-opacity: 0.75;
  pointer-events: none;
}
.arc.dashed { stroke-dasharray: 5 4; stroke-opacity: 0.6; }
.arrow { fill: var(--c); stroke: var(--bg); stroke-width: 0.6; pointer-events: none; }
.hit { fill: none; stroke: transparent; stroke-width: 12; pointer-events: stroke; cursor: pointer; }
.has-active .flow:not(.active) { opacity: 0.12; }
.flow.active .arc { stroke-opacity: 1; }

.cluster { cursor: pointer; transition: opacity 0.15s ease; outline: none; }
.cluster:focus-visible .dot { stroke: var(--accent); stroke-width: 2.5; }
.cluster .dot {
  fill: var(--role-c, var(--text-dim));
  stroke: var(--bg);
  stroke-width: 1.5;
}
.cluster.country .dot { fill: var(--bg); stroke: var(--role-c); stroke-width: 1.5; stroke-dasharray: 2 2; }
.cluster.uncertain .dot { fill-opacity: 0.55; stroke: var(--role-c); stroke-dasharray: 2 2; }
.cluster.pinned .dot { stroke: var(--text); stroke-width: 2; }
.cluster .anycast-ring { fill: none; stroke: var(--map-dest); stroke-width: 1; stroke-dasharray: 2 3; opacity: 0.8; }
.has-active .cluster:not(.active) { opacity: 0.3; }
.count {
  font-size: 9px;
  font-weight: 700;
  text-anchor: middle;
  fill: var(--bg);
  pointer-events: none;
}
.cluster.country .count { fill: var(--text); }
.you-label { font-size: 11px; font-weight: 600; fill: var(--text); pointer-events: none; paint-order: stroke; stroke: var(--bg); stroke-width: 3px; }
.r-you { --role-c: var(--map-you); }
.r-pool { --role-c: var(--map-pool); }
.r-server { --role-c: var(--map-server); }
.r-exit { --role-c: var(--map-exit); }
.r-dest { --role-c: var(--map-dest); }

.tokens { position: absolute; inset: 0; pointer-events: none; }
.token {
  position: absolute;
  left: -11px;
  top: -11px;
  width: 22px;
  height: 22px;
  padding: 1px;
  border-radius: 7px;
  background: var(--panel);
  border: 2px solid var(--c);
  display: flex;
  align-items: center;
  justify-content: center;
  pointer-events: auto;
  cursor: pointer;
  will-change: transform;
  transition: opacity 0.15s ease;
}
.token :deep(img), .token :deep(.placeholder) { border-radius: 4px; }
.token.idle { opacity: 0.6; }
.has-active .token:not(.active) { opacity: 0.12; }
.nogroup { width: 8px; height: 8px; border-radius: 50%; background: var(--c); }

.tip {
  position: absolute;
  max-width: 240px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 7px 9px;
  background: var(--panel);
  border: 1px solid var(--border);
  border-radius: 6px;
  font-size: 11px;
  pointer-events: none;
  box-shadow: 0 6px 18px rgba(0, 0, 0, 0.3);
}
.tip span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.card-wrap { position: absolute; z-index: 2; }
</style>
