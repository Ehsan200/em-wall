import { fmtDateTime, zonedEpoch, zonedParts } from './timezone';

// A usage window. 'last' rolls with the clock and 'since' runs from a
// fixed start to now (both live); 'between' is a fixed past window.
export type RangeSel =
  | { kind: 'last'; secs: number }
  | { kind: 'since'; from: number; label: string }
  | { kind: 'between'; from: number; to: number; label?: string };

export interface ResolvedRange { from: number; to: number; live: boolean }

// Must match core/rules TrafficRetention / TrafficRollupAge / TrafficRollup.
export const RETENTION_SECS = 90 * 86400;
export const ROLLUP_AGE_SECS = 48 * 3600;
const ROLLUP_SECS = 3600;

const MIN = 60, HOUR = 3600, DAY = 86400;

export const RELATIVE_PRESETS: { secs: number; short: string }[] = [
  { secs: 15 * MIN, short: '15m' }, { secs: HOUR, short: '1h' }, { secs: 6 * HOUR, short: '6h' },
  { secs: 12 * HOUR, short: '12h' }, { secs: DAY, short: '24h' }, { secs: 3 * DAY, short: '3d' },
  { secs: 7 * DAY, short: '7d' }, { secs: 14 * DAY, short: '14d' }, { secs: 30 * DAY, short: '30d' },
  { secs: 90 * DAY, short: '90d' },
].filter((p) => p.secs <= RETENTION_SECS); // never offer more history than is kept

export function spanLabel(secs: number): string {
  if (secs % DAY === 0) return secs === DAY ? '24 hours' : `${secs / DAY} days`;
  if (secs % HOUR === 0) return secs === HOUR ? 'hour' : `${secs / HOUR} hours`;
  return `${Math.round(secs / MIN)} minutes`;
}

const nowSec = () => Math.floor(Date.now() / 1000);
const startOfDay = (p: { y: number; mo: number; d: number }, addDays = 0) =>
  Math.floor(zonedEpoch(p.y, p.mo, p.d + addDays) / 1000);

// Calendar windows in the display zone, built at click time. Weeks start
// on Monday.
export function calendarPresets(): { label: string; sel: RangeSel }[] {
  const p = zonedParts(Date.now());
  const today = startOfDay(p);
  const weekStart = startOfDay(p, -((p.dow + 6) % 7));
  const monthStart = Math.floor(zonedEpoch(p.y, p.mo, 1) / 1000);
  const prevMonthStart = Math.floor(zonedEpoch(p.y, p.mo - 1, 1) / 1000);
  return [
    { label: 'Today', sel: { kind: 'since', from: today, label: 'Today' } },
    { label: 'Yesterday', sel: { kind: 'between', from: startOfDay(p, -1), to: today, label: 'Yesterday' } },
    { label: 'This week', sel: { kind: 'since', from: weekStart, label: 'This week' } },
    { label: 'Last week', sel: { kind: 'between', from: startOfDay(p, -((p.dow + 6) % 7) - 7), to: weekStart, label: 'Last week' } },
    { label: 'This month', sel: { kind: 'since', from: monthStart, label: 'This month' } },
    { label: 'Last month', sel: { kind: 'between', from: prevMonthStart, to: monthStart, label: 'Last month' } },
  ];
}

export function resolveRange(sel: RangeSel, now = nowSec()): ResolvedRange {
  switch (sel.kind) {
    case 'last': return { from: now - sel.secs, to: now, live: true };
    case 'since': return { from: sel.from, to: now, live: true };
    default: return { from: sel.from, to: sel.to, live: false };
  }
}

export const fmtPoint = (sec: number) =>
  fmtDateTime(sec * 1000, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });

export function rangeLabel(sel: RangeSel): string {
  if (sel.kind === 'last') return `Last ${spanLabel(sel.secs)}`;
  if (sel.kind === 'since' || sel.label) return sel.label!;
  return `${fmtPoint(sel.from)} – ${fmtPoint(sel.to)}`;
}

// Oldest instant the daemon still holds usage for.
export const retentionStart = (now = nowSec()) => now - RETENTION_SECS;

// Move the window back (dir -1) or forward (+1) by its own length. Stepping
// forward into the present turns it back into a live "last N" window.
// Stepping back never reaches past retention: the window is cut at the
// oldest stored data rather than showing an empty stretch before it.
export function shiftRange(sel: RangeSel, dir: -1 | 1, now = nowSec()): RangeSel {
  const r = resolveRange(sel, now);
  const span = r.to - r.from;
  const to = r.to + dir * span;
  if (dir > 0 && to >= now) return { kind: 'last', secs: span };
  return { kind: 'between', from: Math.max(r.from + dir * span, retentionStart(now)), to };
}

// Why a custom window can't be shown, or null when it can. Ranges are
// checked here rather than trusting the inputs' min/max, which a typed
// date bypasses.
export function customRangeError(from: number | null, to: number | null, now = nowSec()): string | null {
  if (from === null || to === null) return 'Pick both a start and an end.';
  if (to <= from) return 'End must be after start.';
  if (from > now) return 'Start is in the future.';
  if (to <= retentionStart(now)) return `No data that old — history is kept ${RETENTION_SECS / DAY} days.`;
  return null;
}

const BUCKETS = [MIN, 5 * MIN, 10 * MIN, 15 * MIN, 30 * MIN, HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR, DAY];
const TARGET_POINTS = 200;

// Display bucket for a window: the finest that keeps the chart under
// TARGET_POINTS, never finer than the hourly rollup once the window reaches
// past ROLLUP_AGE_SECS (minute buckets there would show hourly spikes).
export function bucketFor(r: ResolvedRange, now = nowSec()): number {
  const span = Math.max(r.to - r.from, MIN);
  let b = BUCKETS.find((w) => span / w <= TARGET_POINTS) ?? DAY;
  if (r.from < now - ROLLUP_AGE_SECS && b < ROLLUP_SECS) b = ROLLUP_SECS;
  return b;
}

// Epoch seconds ⇄ <input type="datetime-local"> value, in the display zone.
export function toLocalInput(sec: number): string {
  const p = zonedParts(sec * 1000);
  const z = (n: number) => String(n).padStart(2, '0');
  return `${p.y}-${z(p.mo)}-${z(p.d)}T${z(p.h)}:${z(p.mi)}`;
}
export function fromLocalInput(v: string): number | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(v);
  if (!m) return null;
  return Math.floor(zonedEpoch(+m[1], +m[2], +m[3], +m[4], +m[5]) / 1000);
}
