import { ref, computed } from 'vue';
import { GetSetting, SetSetting } from '../wailsjs/go/main/App';

// Display timezone for every timestamp the UI renders. '' means "follow the
// system" (the default); any IANA name means the user picked a custom zone.
// Stored daemon-side under SETTING_KEY like every other setting, and mirrored
// to localStorage so the first paint (and a stopped daemon) already uses it.
export const SETTING_KEY = 'display_timezone';
const CACHE_KEY = 'em-wall.displayTimezone';

function readCache(): string {
  try { return localStorage.getItem(CACHE_KEY) || ''; } catch { return ''; }
}
function writeCache(v: string) {
  try {
    if (v) localStorage.setItem(CACHE_KEY, v); else localStorage.removeItem(CACHE_KEY);
  } catch { /* storage unavailable — the daemon copy still holds it */ }
}

export const displayTimezone = ref<string>(readCache());

export const systemTimezone = (() => {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
})();

export const effectiveTimezone = computed(() => displayTimezone.value || systemTimezone);

export function isValidTimezone(tz: string): boolean {
  try { new Intl.DateTimeFormat(undefined, { timeZone: tz }); return true; } catch { return false; }
}

// All IANA zones the webview knows, falling back to a short list on an engine
// without Intl.supportedValuesOf.
export function allTimezones(): string[] {
  const anyIntl = Intl as any;
  if (typeof anyIntl.supportedValuesOf === 'function') {
    try { return anyIntl.supportedValuesOf('timeZone') as string[]; } catch { /* fall through */ }
  }
  return ['UTC', 'Europe/London', 'Europe/Berlin', 'Europe/Istanbul', 'Asia/Tehran', 'Asia/Dubai',
    'Asia/Kolkata', 'Asia/Shanghai', 'Asia/Tokyo', 'America/New_York', 'America/Chicago',
    'America/Los_Angeles', 'Australia/Sydney'];
}

// Short "UTC+3:30" label for a zone at the current instant.
export function utcOffsetLabel(tz: string): string {
  try {
    const part = new Intl.DateTimeFormat('en-US', { timeZone: tz, timeZoneName: 'shortOffset' })
      .formatToParts(new Date()).find((p) => p.type === 'timeZoneName');
    return part ? part.value.replace('GMT', 'UTC') : '';
  } catch { return ''; }
}

function apply(v: string) {
  const tz = v && isValidTimezone(v) ? v : '';
  displayTimezone.value = tz;
  writeCache(tz);
}

// Pull the stored choice from the daemon. Silent on failure: the cached
// value (or the system zone) stays in effect.
export async function loadDisplayTimezone() {
  try { apply(await GetSetting(SETTING_KEY, '')); } catch { /* daemon down */ }
}

// Persist a choice; '' resets to the system zone.
export async function saveDisplayTimezone(tz: string) {
  await SetSetting(SETTING_KEY, tz);
  apply(tz);
}

type DateInput = Date | string | number;

function toDate(v: DateInput): Date {
  return v instanceof Date ? v : new Date(v);
}

// Formatters mirroring Date.toLocale*String, pinned to the display zone.
// Reading displayTimezone inside them makes any template that calls them
// re-render when the zone changes.
export function fmtDateTime(v: DateInput, opts: Intl.DateTimeFormatOptions = {}): string {
  return toDate(v).toLocaleString([], { ...opts, timeZone: effectiveTimezone.value });
}
export function fmtTime(v: DateInput, opts: Intl.DateTimeFormatOptions = {}): string {
  return toDate(v).toLocaleTimeString([], { ...opts, timeZone: effectiveTimezone.value });
}
export function fmtDate(v: DateInput, opts: Intl.DateTimeFormatOptions = {}): string {
  return toDate(v).toLocaleDateString([], { ...opts, timeZone: effectiveTimezone.value });
}

// Wall-clock parts of an instant in the display zone.
export interface ZonedParts { y: number; mo: number; d: number; h: number; mi: number; dow: number }

export function zonedParts(v: DateInput): ZonedParts {
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: effectiveTimezone.value, hourCycle: 'h23',
    year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', weekday: 'short',
  }).formatToParts(toDate(v));
  const get = (t: string) => parts.find((p) => p.type === t)?.value || '0';
  const dow = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'].indexOf(get('weekday'));
  return { y: +get('year'), mo: +get('month'), d: +get('day'), h: +get('hour') % 24, mi: +get('minute'), dow };
}

// Epoch ms of a wall-clock time in the display zone. Out-of-range fields
// roll over like Date.UTC (day 0 = last day of the previous month). Two
// passes settle the zone offset, DST edges included.
export function zonedEpoch(y: number, mo: number, d = 1, h = 0, mi = 0): number {
  const want = Date.UTC(y, mo - 1, d, h, mi);
  let t = want;
  for (let i = 0; i < 2; i++) {
    const p = zonedParts(t);
    t += want - Date.UTC(p.y, p.mo - 1, p.d, p.h, p.mi);
  }
  return t;
}
