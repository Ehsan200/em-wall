// Dev-only sample data for the Map tab, used while map.paths has no real
// backend (or to look at the panel without live proxied traffic).
//
// Enabled only in a dev build (import.meta.env.DEV), by any of:
//   - URL query      ?mapMock=1            (e.g. http://localhost:34115/?mapMock=1)
//   - localStorage   localStorage.setItem('em-wall.mapMock', '1'); location.reload()
//   - the constant   MOCK_DEFAULT below
// The value is a comma list of options:
//   1 / on      the hand-written sample flows
//   <N>         add synthetic flows up to N in total (perf check, e.g. 200)
//   nocity      report cityData=false (country-precision banner)
//   empty       no flows (empty state)
// A production build never imports this module (MapPanel loads it behind
// an import.meta.env.DEV check, which Vite folds to false).

import type { MapFlow, MapHop, MapResult } from './types';

export const MOCK_DEFAULT = '';

export interface MockOptions {
  count: number;
  noCity: boolean;
  empty: boolean;
}

export function mockOptions(): MockOptions | null {
  let raw = MOCK_DEFAULT;
  try {
    const q = new URLSearchParams(window.location.search).get('mapMock');
    if (q !== null) raw = q || '1';
    else raw = window.localStorage.getItem('em-wall.mapMock') || raw;
  } catch { /* storage blocked: fall back to the constant */ }
  raw = raw.trim().toLowerCase();
  if (!raw || raw === '0' || raw === 'off') return null;
  const opts: MockOptions = { count: 0, noCity: false, empty: false };
  for (const part of raw.split(',').map((s) => s.trim())) {
    if (part === 'nocity') opts.noCity = true;
    else if (part === 'empty') opts.empty = true;
    else if (/^\d+$/.test(part) && Number(part) > 1) opts.count = Number(part);
  }
  return opts;
}

type Place = Pick<MapHop, 'country' | 'countryName' | 'city' | 'lat' | 'lon' | 'precision'>;

const P: Record<string, Place> = {
  tehran:    { country: 'IR', countryName: 'Iran', city: 'Tehran', lat: 35.6892, lon: 51.389, precision: 'city' },
  frankfurt: { country: 'DE', countryName: 'Germany', city: 'Frankfurt am Main', lat: 50.1109, lon: 8.6821, precision: 'city' },
  falkenstein: { country: 'DE', countryName: 'Germany', city: 'Falkenstein', lat: 50.4779, lon: 12.3713, precision: 'city' },
  amsterdam: { country: 'NL', countryName: 'Netherlands', city: 'Amsterdam', lat: 52.3676, lon: 4.9041, precision: 'city' },
  stockholm: { country: 'SE', countryName: 'Sweden', city: 'Stockholm', lat: 59.3293, lon: 18.0686, precision: 'city' },
  sanfran:   { country: 'US', countryName: 'United States', city: 'San Francisco', lat: 37.7749, lon: -122.4194, precision: 'city' },
  secaucus:  { country: 'US', countryName: 'United States', city: 'Secaucus', lat: 40.7895, lon: -74.0565, precision: 'city' },
  ashburn:   { country: 'US', countryName: 'United States', city: 'Ashburn', lat: 39.0438, lon: -77.4874, precision: 'city' },
  // Country precision: no city, the country's centroid.
  turkey:    { country: 'TR', countryName: 'Türkiye', city: '', lat: 38.9637, lon: 35.2433, precision: 'country' },
  london:    { country: 'GB', countryName: 'United Kingdom', city: 'London', lat: 51.5072, lon: -0.1276, precision: 'city' },
  helsinki:  { country: 'FI', countryName: 'Finland', city: 'Helsinki', lat: 60.1699, lon: 24.9384, precision: 'city' },
  istanbul:  { country: 'TR', countryName: 'Türkiye', city: 'Istanbul', lat: 41.0082, lon: 28.9784, precision: 'city' },
  dubai:     { country: 'AE', countryName: 'United Arab Emirates', city: 'Dubai', lat: 25.2048, lon: 55.2708, precision: 'city' },
  singapore: { country: 'SG', countryName: 'Singapore', city: 'Singapore', lat: 1.3521, lon: 103.8198, precision: 'city' },
  tokyo:     { country: 'JP', countryName: 'Japan', city: 'Tokyo', lat: 35.6762, lon: 139.6503, precision: 'city' },
  paris:     { country: 'FR', countryName: 'France', city: 'Paris', lat: 48.8566, lon: 2.3522, precision: 'city' },
  warsaw:    { country: 'PL', countryName: 'Poland', city: 'Warsaw', lat: 52.2297, lon: 21.0122, precision: 'city' },
  toronto:   { country: 'CA', countryName: 'Canada', city: 'Toronto', lat: 43.6532, lon: -79.3832, precision: 'city' },
  saopaulo:  { country: 'BR', countryName: 'Brazil', city: 'São Paulo', lat: -23.5558, lon: -46.6396, precision: 'city' },
  sydney:    { country: 'AU', countryName: 'Australia', city: 'Sydney', lat: -33.8688, lon: 151.2093, precision: 'city' },
};

function hop(role: string, label: string, place: Place, extra: Partial<MapHop> = {}): MapHop {
  return { role, label, asn: 0, org: '', anycast: false, uncertain: false, ...place, ...extra };
}

const YOU = hop('you', 'This Mac', P.tehran, {
  ip: '5.160.218.47', asn: 58224, org: 'Iran Telecommunication Company PJS',
});

function poolHop(label: string, place: Place, ip: string, alternatives: string[]): MapHop {
  return hop('pool', label, place, {
    ip, asn: 24940, org: 'Hetzner Online GmbH', uncertain: true, alternatives,
  });
}

interface Seed { flow: Omit<MapFlow, 'rateUp' | 'rateDown' | 'bytesUp' | 'bytesDown'>; base: number }

const SEEDS: Seed[] = [
  {
    // The full chain: master entry riding a subscription pool node, with a
    // measured exit IP that differs from the server.
    base: 2.6e6,
    flow: {
      group: 'spotify', groupName: 'Spotify', groupColor: '#1db954',
      iface: 'xrayset:eu', upstream: 'ams-master', conns: 7,
      hosts: ['spclient.wg.spotify.com', 'audio4-fa.scdn.co', 'ap-gew4.spotify.com'],
      hops: [
        YOU,
        poolHop('de-fra-03', P.frankfurt, '88.198.41.17', ['de-fsn-11', 'de-fra-07']),
        hop('server', 'ams-master', P.amsterdam, { host: 'ams1.example.net', ip: '185.107.56.12', asn: 60781, org: 'LeaseWeb Netherlands B.V.' }),
        hop('exit', '89.38.98.144', P.amsterdam, { ip: '89.38.98.144', asn: 9009, org: 'M247 Europe SRL' }),
        hop('dest', 'spclient.wg.spotify.com', P.stockholm, { host: 'spclient.wg.spotify.com', ip: '78.31.8.101', asn: 8403, org: 'Spotify AB' }),
      ],
    },
  },
  {
    // Anycast destination: GeoIP places Cloudflare at its registered HQ.
    base: 420e3,
    flow: {
      group: 'anthropic', groupName: 'Claude / Anthropic', groupColor: '#d97757',
      iface: 'xray:nyc', upstream: 'nyc', conns: 4,
      hosts: ['claude.ai', 'challenges.cloudflare.com'],
      hops: [
        YOU,
        hop('server', 'nyc', P.secaucus, { host: 'nyc.example.org', ip: '64.176.12.9', asn: 20473, org: 'The Constant Company, LLC' }),
        hop('dest', 'claude.ai', P.sanfran, { host: 'claude.ai', ip: '104.18.32.47', asn: 13335, org: 'Cloudflare, Inc.', anycast: true }),
      ],
    },
  },
  {
    // Country-precision server (no city in the DB for that range).
    base: 95e3,
    flow: {
      group: 'telegram', groupName: 'Telegram', groupColor: '#2aabee',
      iface: 'xray:tr1', upstream: 'tr1', conns: 3,
      hosts: ['149.154.167.51'],
      hops: [
        YOU,
        hop('server', 'tr1', P.turkey, { host: 'tr1.example.com', ip: '185.255.94.30', asn: 197328, org: 'Inetltd' }),
        hop('dest', 'Telegram DC4', P.amsterdam, { ip: '149.154.167.51', asn: 62041, org: 'Telegram Messenger Inc' }),
      ],
    },
  },
  {
    // Same pool, different master and destination; uncertain pool hop.
    base: 5.4e6,
    flow: {
      group: 'youtube', groupName: 'YouTube', groupColor: '#ff3045',
      iface: 'xrayset:nj', upstream: 'us-nj-nap', conns: 12,
      hosts: ['rr3---sn-4g5e6nzz.googlevideo.com', 'www.youtube.com', 'i.ytimg.com', 'yt3.ggpht.com'],
      hops: [
        YOU,
        poolHop('de-fsn-11', P.falkenstein, '78.46.102.200', ['de-fra-03']),
        hop('server', 'us-nj-nap', P.secaucus, { host: 'nj-nap.example.net', ip: '89.117.120.221', asn: 46562, org: 'Performive LLC' }),
        hop('dest', 'rr3---sn-4g5e6nzz.googlevideo.com', P.ashburn, { host: 'rr3---sn-4g5e6nzz.googlevideo.com', ip: '173.194.140.8', asn: 15169, org: 'Google LLC' }),
      ],
    },
  },
  {
    // A rule that belongs to no group.
    base: 18e3,
    flow: {
      group: '', groupName: '', groupColor: '',
      iface: 'proxy:lon', upstream: 'lon', conns: 1,
      hosts: ['news.ycombinator.com'],
      hops: [
        YOU,
        hop('server', 'lon', P.london, { host: 'lon.example.net', ip: '45.77.88.10', asn: 20473, org: 'The Constant Company, LLC' }),
        hop('dest', 'news.ycombinator.com', P.sanfran, { host: 'news.ycombinator.com', ip: '209.216.230.207', asn: 21581, org: 'M5 Computer Security' }),
      ],
    },
  },
  {
    // Destination with no location at all (precision ""): the path ends at
    // the server on the map, the list still names the host.
    base: 6e3,
    flow: {
      group: 'openai', groupName: 'OpenAI / ChatGPT / Codex', groupColor: '#10a37f',
      iface: 'xrayset:eu', upstream: 'ams-master', conns: 2,
      hosts: ['chatgpt.com'],
      hops: [
        YOU,
        poolHop('de-fra-07', P.frankfurt, '88.198.41.33', ['de-fra-03', 'de-fsn-11']),
        hop('server', 'ams-master', P.amsterdam, { host: 'ams1.example.net', ip: '185.107.56.12', asn: 60781, org: 'LeaseWeb Netherlands B.V.' }),
        hop('dest', 'chatgpt.com', { country: '', countryName: '', city: '', lat: 0, lon: 0, precision: '' }, { host: 'chatgpt.com' }),
      ],
    },
  },
];

const SYN_GROUPS = [
  { group: 'github', groupName: 'GitHub', groupColor: '#8b949e' },
  { group: 'google', groupName: 'Google', groupColor: '#4285f4' },
  { group: 'x', groupName: 'X', groupColor: '#d0d0d8' },
  { group: 'meta', groupName: 'Meta', groupColor: '#0866ff' },
  { group: 'notion', groupName: 'Notion', groupColor: '#c9c9c9' },
  { group: 'docker', groupName: 'Docker', groupColor: '#1d63ed' },
  { group: 'npm', groupName: 'npm', groupColor: '#cb3837' },
  { group: 'figma', groupName: 'Figma', groupColor: '#a259ff' },
];
const SYN_SERVERS = ['frankfurt', 'amsterdam', 'helsinki', 'istanbul', 'dubai', 'london', 'paris', 'warsaw', 'secaucus'];
const SYN_DESTS = ['ashburn', 'sanfran', 'stockholm', 'singapore', 'tokyo', 'paris', 'toronto', 'saopaulo', 'sydney', 'frankfurt', 'london'];

function synthetic(n: number): Seed[] {
  const out: Seed[] = [];
  for (let i = 0; out.length < n; i++) {
    const g = SYN_GROUPS[i % SYN_GROUPS.length];
    const s = SYN_SERVERS[(i * 7 + 3) % SYN_SERVERS.length];
    const d = SYN_DESTS[(i * 5 + 1) % SYN_DESTS.length];
    const host = `h${i}.${g.group}.example`;
    const hops: MapHop[] = [YOU];
    if (i % 3 === 0) hops.push(poolHop(`pool-${i % 9}`, P.falkenstein, `78.46.10.${i % 250}`, ['pool-x']));
    hops.push(hop('server', `srv-${s}`, P[s], { ip: `10.${i % 250}.0.1`, asn: 64500 + (i % 20), org: 'Example Hosting' }));
    hops.push(hop('dest', host, P[d], { host, ip: `203.0.${i % 250}.9`, asn: 64600 + (i % 30), org: 'Example CDN', anycast: i % 11 === 0 }));
    out.push({
      base: 2e3 + ((i * 7919) % 1500) * 1e3,
      flow: { ...g, iface: `xray:srv-${s}`, upstream: `srv-${s}`, conns: 1 + (i % 6), hosts: [host], hops },
    });
  }
  return out;
}

// Country centroids for the nocity option.
const CENTROID: Record<string, [number, number]> = {
  IR: [32.4, 53.7], DE: [51.2, 10.4], NL: [52.1, 5.3], SE: [60.1, 18.6], US: [39.8, -98.6],
  TR: [38.96, 35.24], GB: [55.4, -3.4], FI: [61.9, 25.7], AE: [23.4, 53.8], SG: [1.35, 103.8],
  JP: [36.2, 138.3], FR: [46.2, 2.2], PL: [51.9, 19.1], CA: [56.1, -106.3], BR: [-14.2, -51.9],
  AU: [-25.3, 133.8],
};

const startedAt = Date.now();
let synCache: { n: number; seeds: Seed[] } | null = null;

export function mockPaths(opts: MockOptions): MapResult {
  const now = Date.now();
  if (opts.empty) return { flows: [], cityData: !opts.noCity, generatedAt: new Date(now).toISOString() };
  let seeds = SEEDS;
  const extra = Math.max(0, opts.count - SEEDS.length);
  if (extra > 0) {
    if (!synCache || synCache.n !== extra) synCache = { n: extra, seeds: synthetic(extra) };
    seeds = [...SEEDS, ...synCache.seeds];
  }
  const t = (now - startedAt) / 1000;
  const flows: MapFlow[] = seeds.map((s, i) => {
    // Rates wander so speeds and widths visibly change between polls.
    const wobble = 0.55 + 0.45 * Math.sin(t / 7 + i * 1.7) + 0.15 * Math.sin(t / 2.3 + i);
    const down = Math.max(0, s.base * wobble);
    const up = down * (0.04 + 0.02 * ((i % 5) / 5));
    let hops = s.flow.hops;
    if (opts.noCity && hops) {
      // Without city data every hop drops to its country's centroid.
      hops = hops.map((h) => {
        const c = CENTROID[h.country];
        return h.precision === 'city' && c ? { ...h, city: '', precision: 'country', lat: c[0], lon: c[1] } : h;
      });
    }
    return {
      ...s.flow,
      hops,
      rateDown: down,
      rateUp: up,
      bytesDown: Math.round(s.base * (40 + t)),
      bytesUp: Math.round(s.base * (40 + t) * 0.05),
    };
  });
  return { flows, cityData: !opts.noCity, generatedAt: new Date(now).toISOString() };
}
