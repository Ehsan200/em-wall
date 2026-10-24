// Projection-dependent geometry for the world map: arcs per flow, the
// sampled track a group icon travels along, and city clusters of hops.
// Pure functions of (flows, projection) so the panel can recompute them on
// a poll or a resize without touching the animation loop.

import { geoDistance, geoInterpolate, geoPath } from 'd3-geo';
import type { GeoProjection } from 'd3-geo';
import { placed, ROLE_ORDER } from './types';
import type { MapFlow, MapHop } from './types';

export interface Segment {
  d: string;
  dashed: boolean; // an uncertain endpoint, or a gap over an unplaced hop
}

// A polyline in screen space with cumulative lengths, so a position along
// it is a binary search + lerp (no getPointAtLength per frame).
export interface Track {
  xy: Float32Array; // x0,y0,x1,y1,...
  cum: Float32Array; // cumulative length at each point
  len: number;
}

export interface Arrow { x: number; y: number; a: number }

export interface FlowGeom {
  segments: Segment[];
  hitD: string; // all segments, for one wide transparent hover target
  track: Track | null;
  arrows: Arrow[]; // one per segment midpoint (reduced-motion direction cue)
}

const MIN_DIST = 1e-4; // radians; hops closer than ~600 m draw no arc

export function buildFlowGeom(hops: MapHop[], proj: GeoProjection, width: number): FlowGeom {
  const path = geoPath(proj);
  const segments: Segment[] = [];
  const arrows: Arrow[] = [];
  const xs: number[] = [];
  const cum: number[] = [];
  let total = 0;

  let prev: { h: MapHop; i: number } | null = null;
  hops.forEach((h, i) => {
    if (!placed(h)) return;
    if (prev) {
      const a: [number, number] = [prev.h.lon, prev.h.lat];
      const b: [number, number] = [h.lon, h.lat];
      const dist = geoDistance(a, b);
      if (dist > MIN_DIST) {
        const d = path({ type: 'LineString', coordinates: [a, b] }) || '';
        const dashed = prev.h.uncertain || h.uncertain || i - prev.i > 1;
        segments.push({ d, dashed });

        const interp = geoInterpolate(a, b);
        const n = Math.max(2, Math.min(64, Math.ceil(dist * 40)));
        const segPts: [number, number][] = [];
        for (let k = 0; k <= n; k++) {
          const p = proj(interp(k / n));
          if (p) segPts.push([p[0], p[1]]);
        }
        for (let k = 0; k < segPts.length; k++) {
          const [x, y] = segPts[k];
          if (xs.length) {
            const px = xs[xs.length - 2], py = xs[xs.length - 1];
            const step = Math.hypot(x - px, y - py);
            // A jump over the antimeridian: teleport, don't streak across.
            total += step > width / 2 ? 0 : step;
          }
          xs.push(x, y);
          cum.push(total);
        }
        if (segPts.length >= 3) {
          const m = Math.floor(segPts.length / 2);
          const [x0, y0] = segPts[m - 1];
          const [x1, y1] = segPts[m + 1];
          arrows.push({ x: segPts[m][0], y: segPts[m][1], a: (Math.atan2(y1 - y0, x1 - x0) * 180) / Math.PI });
        }
      }
    }
    prev = { h, i };
  });

  const track = total > 0 ? { xy: Float32Array.from(xs), cum: Float32Array.from(cum), len: total } : null;
  return { segments, hitD: segments.map((s) => s.d).join(' '), track, arrows };
}

export function pointAt(track: Track, dist: number, out: { x: number; y: number }): void {
  const cum = track.cum;
  let lo = 0, hi = cum.length - 1;
  if (dist <= 0) { out.x = track.xy[0]; out.y = track.xy[1]; return; }
  if (dist >= track.len) { out.x = track.xy[hi * 2]; out.y = track.xy[hi * 2 + 1]; return; }
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (cum[mid] <= dist) lo = mid; else hi = mid;
  }
  const span = cum[hi] - cum[lo];
  const f = span > 0 ? (dist - cum[lo]) / span : 0;
  out.x = track.xy[lo * 2] + (track.xy[hi * 2] - track.xy[lo * 2]) * f;
  out.y = track.xy[lo * 2 + 1] + (track.xy[hi * 2 + 1] - track.xy[lo * 2 + 1]) * f;
}

// ---- City clusters ----

export interface ClusterHop {
  hop: MapHop;
  flows: string[]; // flow keys that pass through this hop
}

export interface Cluster {
  key: string;
  x: number;
  y: number;
  lat: number;
  lon: number;
  place: string;
  hops: ClusterHop[];
  role: string; // the most prominent role, for the marker colour
  roles: string[];
  countryOnly: boolean; // every hop here is country precision
  anycast: boolean;
  uncertain: boolean;
  flows: string[];
}

function clusterKey(h: MapHop): string {
  return h.precision === 'city' ? `c|${h.country}|${h.city}` : `k|${h.country}|${h.lat.toFixed(1)},${h.lon.toFixed(1)}`;
}

function hopId(h: MapHop): string {
  return `${h.role}|${h.ip || h.label}|${h.country}|${h.city}`;
}

export function buildClusters(flows: { key: string; flow: MapFlow }[], proj: GeoProjection): Cluster[] {
  const by = new Map<string, { c: Cluster; ids: Map<string, ClusterHop>; flowSet: Set<string> }>();
  for (const { key, flow } of flows) {
    for (const h of flow.hops ?? []) {
      if (!placed(h)) continue;
      const ck = clusterKey(h);
      let entry = by.get(ck);
      if (!entry) {
        const p = proj([h.lon, h.lat]);
        if (!p) continue;
        entry = {
          c: {
            key: ck, x: p[0], y: p[1], lat: h.lat, lon: h.lon,
            place: h.city ? `${h.city}, ${h.countryName || h.country}` : (h.countryName || h.country),
            hops: [], role: h.role, roles: [], countryOnly: true, anycast: false, uncertain: false, flows: [],
          },
          ids: new Map(),
          flowSet: new Set(),
        };
        by.set(ck, entry);
      }
      const id = hopId(h);
      let ch = entry.ids.get(id);
      if (!ch) {
        ch = { hop: h, flows: [] };
        entry.ids.set(id, ch);
        entry.c.hops.push(ch);
      }
      if (!ch.flows.includes(key)) ch.flows.push(key);
      entry.flowSet.add(key);
    }
  }
  const out: Cluster[] = [];
  for (const { c, flowSet } of by.values()) {
    const roles = new Set(c.hops.map((x) => x.hop.role));
    c.roles = ROLE_ORDER.filter((r) => roles.has(r));
    c.role = c.roles[0] || c.hops[0].hop.role;
    c.countryOnly = c.hops.every((x) => x.hop.precision === 'country');
    c.anycast = c.hops.some((x) => x.hop.anycast);
    c.uncertain = c.hops.every((x) => x.hop.uncertain);
    c.flows = [...flowSet];
    c.hops.sort((a, b) => ROLE_ORDER.indexOf(a.hop.role) - ROLE_ORDER.indexOf(b.hop.role));
    out.push(c);
  }
  // Busier clusters underneath, so a lone hop next to a hub stays hoverable.
  out.sort((a, b) => b.hops.length - a.hops.length);
  return out;
}

// Geographic box of every placed hop, padded to a minimum span and rounded
// so a small change in the flow set doesn't move the view on every poll.
// null = show the whole world.
export function fitBox(flows: MapFlow[]): [number, number, number, number] | null {
  let minLon = Infinity, maxLon = -Infinity, minLat = Infinity, maxLat = -Infinity;
  for (const f of flows) {
    for (const h of f.hops ?? []) {
      if (!placed(h)) continue;
      minLon = Math.min(minLon, h.lon); maxLon = Math.max(maxLon, h.lon);
      minLat = Math.min(minLat, h.lat); maxLat = Math.max(maxLat, h.lat);
    }
  }
  if (!isFinite(minLon)) return null;
  const grow = (lo: number, hi: number, min: number): [number, number] => {
    const span = hi - lo;
    const pad = Math.max(span * 0.12, (min - span) / 2, 0);
    return [lo - pad, hi + pad];
  };
  [minLon, maxLon] = grow(minLon, maxLon, 40);
  [minLat, maxLat] = grow(minLat, maxLat, 22);
  if (maxLon - minLon > 220) return null;
  const r = (v: number, up: boolean) => (up ? Math.ceil(v / 5) * 5 : Math.floor(v / 5) * 5);
  return [
    Math.max(-180, r(minLon, false)), Math.max(-85, r(minLat, false)),
    Math.min(180, r(maxLon, true)), Math.min(85, r(maxLat, true)),
  ];
}
