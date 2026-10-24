// Plain mirrors of the map.paths DTOs in core/ipc/protocol.go
// (MapPathsResult / MapFlowDTO / MapHopDTO). The generated wailsjs classes
// are structurally assignable to these, and the dev mock can build them as
// object literals.

export type HopRole = 'you' | 'pool' | 'server' | 'exit' | 'dest';

export interface MapHop {
  role: string;
  label: string;
  host?: string;
  ip?: string;
  country: string;
  countryName: string;
  city: string;
  lat: number;
  lon: number;
  precision: string; // "city" | "country" | "" (unplaced)
  asn: number;
  org: string;
  anycast: boolean;
  uncertain: boolean;
  alternatives?: string[];
}

export interface MapFlow {
  group: string;
  groupName: string;
  groupColor: string;
  iface: string;
  upstream: string;
  hosts: string[] | null;
  hops: MapHop[] | null;
  conns: number;
  rateUp: number;
  rateDown: number;
  bytesUp: number;
  bytesDown: number;
}

// A flow as the panel shows it: the DTO plus its identity and display bits.
export interface ViewFlow {
  key: string;
  flow: MapFlow;
  color: string;
  rate: number;
  label: string;
}

export interface MapResult {
  flows: MapFlow[] | null;
  cityData: boolean;
  generatedAt: string;
}

export const ROLE_LABEL: Record<string, string> = {
  you: 'You',
  pool: 'Pool node',
  server: 'Server',
  exit: 'Exit',
  dest: 'Destination',
};

// Draw order of roles inside a city cluster (first wins the marker colour).
export const ROLE_ORDER = ['you', 'dest', 'exit', 'server', 'pool'];

export function placed(h: MapHop): boolean {
  return h.precision === 'city' || h.precision === 'country';
}

// Stable identity of a flow across polls: the daemon aggregates by
// (group, iface, upstream, pool, dest country+city), so the same tuple
// built from the hops is the same flow.
export function flowKey(f: MapFlow): string {
  const hops = (f.hops ?? []).map((h) => `${h.role}:${h.ip || h.label}:${h.country}:${h.city}`);
  return [f.group, f.iface, f.upstream, ...hops].join('|');
}

export function flowRate(f: MapFlow): number {
  return (f.rateUp || 0) + (f.rateDown || 0);
}

export function fmtRate(bps: number): string {
  if (!bps || bps < 1) return '0 B/s';
  if (bps < 1024) return `${Math.round(bps)} B/s`;
  if (bps < 1 << 20) return `${(bps / 1024).toFixed(bps < 10240 ? 1 : 0)} KB/s`;
  if (bps < 1 << 30) return `${(bps / (1 << 20)).toFixed(1)} MB/s`;
  return `${(bps / (1 << 30)).toFixed(2)} GB/s`;
}

export function fmtBytes(n: number): string {
  if (!n || n <= 0) return '0 B';
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1 << 30) return `${(n / (1 << 20)).toFixed(1)} MB`;
  return `${(n / (1 << 30)).toFixed(2)} GB`;
}

export function hopPlace(h: MapHop): string {
  if (!placed(h)) return 'location unknown';
  const country = h.countryName || h.country;
  return h.city ? `${h.city}, ${country}` : country;
}

// Short place name for path summaries ("Tehran → Frankfurt → …").
export function hopShortPlace(h: MapHop): string {
  if (!placed(h)) return '?';
  return h.city || h.countryName || h.country || '?';
}

export function groupColor(f: MapFlow): string {
  const c = (f.groupColor || '').trim();
  return c || 'var(--accent)';
}
