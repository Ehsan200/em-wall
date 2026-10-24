# Geo data, group suggestions, world map

Status: in progress (branch `feat/geo-map-suggest`). Minor version bump.

Three features on one shared base:

1. **Geo data** — offline readers + a safe self-updater for geosite/geoip
   (`.dat`) and DB-IP Lite City/ASN (`.mmdb`).
2. **Group suggestions** — a geosite tag seen in DNS traffic that no group
   covers becomes a one-click custom group; dismissable.
3. **World map** — live proxied flows drawn hop by hop (you → pool node →
   server → exit → destination) at city precision, with group icons moving
   along the arcs.

## Hard invariant: existing groups and rules never break

- No migration rewrites rule rows or group definitions. Schema changes are
  additive only (new nullable columns, new settings keys).
- Nothing edits an existing group or rule without an explicit user action.
  The only such action added here is **Split overlap** (`suggest.split`),
  which edits custom group *definitions* only — rule rows are untouched, so
  routing does not change — and returns the old definitions for undo.
- A geo file update never touches rules. A geosite group's new patterns are
  only reported (`TagNew`) until the user syncs; removed ones (`TagGone`)
  are reported and never auto-removed.
- A geo update that would break xray (a `geosite:`/`geoip:` tag referenced
  by the current xray routing missing from the new file) is rejected.
- `decision.Engine` behaviour is unchanged: a regression test runs a
  fixture rule DB plus sample names through the engine and requires the
  same decisions before and after.

## Contract (written first; agents implement)

- `core/geo/geo.go` — `Location`, `Locator`, `Tag`, `SiteIndex`, `Broad`,
  `Paths`, `DB` (`Open`, `Reload`, `Locate`, `TagsFor`, `Tag`). Stub
  bodies; the geo agent fills them in.
- `core/ipc/protocol.go` — methods `geo.*`, `suggest.*`, `groups.sync-tag`,
  `map.paths` and their DTOs.
- `daemon/{geo,suggest,map}_handlers.go` — one register func per feature,
  already wired in `registerHandlers`; `handlerDeps.geoDB` is constructed
  in `main.go` from `-xray-data-dir`.
- `app/app.go` — forwarding methods for every new IPC method.

## 1. Geo data (`core/geo`, `daemon/geo_*.go`)

Files, all in `/usr/local/share/em-wall/` (the xray asset dir):

| file | source (default) | refresh | verify |
|---|---|---|---|
| `geosite.dat` | Loyalsoldier/v2ray-rules-dat `releases/latest/download/geosite.dat` | weekly | `.sha256sum` + parse |
| `geoip.dat` | same release, `geoip.dat` | weekly | `.sha256sum` + parse |
| `dbip-city-lite.mmdb` | `download.db-ip.com/free/dbip-city-lite-YYYY-MM.mmdb.gz` | monthly | gunzip + mmdb parse + sanity lookup |
| `dbip-asn-lite.mmdb` | `dbip-asn-lite-YYYY-MM.mmdb.gz` | monthly | same |

Alternative geosite sources (setting `geo_source`): `loyalsoldier`
(default), `chocolate4u` (Chocolate4U/Iran-v2ray-rules), `v2fly`, `custom`
(`geo_custom_url` base). DB-IP is CC-BY 4.0 — attribution line in About.

Readers:
- `.dat`: hand-written protowire decoder (`google.golang.org/protobuf/encoding/protowire`),
  no xray-core dependency. geosite domain types: Plain(keyword)=0,
  Regex=1, Domain=2, Full=3. Domain→`*.x`, Full→`x`, others counted in
  `Skipped`. Attributes are kept (for `@cn`-style filtering later).
- Reverse index: domain/full entries of non-`Broad` tags → tags, built
  lazily on first `TagsFor`/`Tag`. Target ≤ 40 MB resident.
- geoip: sorted ranges per country for `Locate` fallback (country
  centroid table embedded in the package), per-tag CIDRs for `Tag.CIDRs`.
- `.mmdb`: `github.com/oschwald/maxminddb-golang` (mmap). City first, then
  geoip.dat country, else not ok. ASN from the ASN mmdb; `Anycast` from a
  small embedded ASN list (13335 Cloudflare, 15169/396982 Google, 20940/16625
  Akamai, 54113 Fastly, 16509 Amazon CloudFront edges, 8075 Microsoft, …).

Updater (daemon, one goroutine, ticks hourly, checks due dates):
1. Download to `<file>.tmp` — direct first, then through `geo_fetch_via`
   (an Interface such as `xrayset:nj`) if set and direct failed.
2. Verify (checksum where published, then parse with the reader).
3. Reject if the new geosite/geoip lacks a tag referenced by the current
   xray routing rules or entries' outbound configs.
4. `<file>` → `<file>.bak`, `<file>.tmp` → `<file>` (rename, atomic).
5. `geo.meta` (JSON next to the files) records per file: source, version,
   sha256, updated-at. `geoDB.Reload()`.
6. xray: if any routing rule references `geosite:`/`geoip:`, re-push routing
   through the live API (`adrules`); verify against the real binary that xray
   re-reads the file. If it does not, mark a pending restart and do it only
   via the supervisor's normal path — never a silent immediate restart.

Settings keys: `geo_source`, `geo_custom_url`, `geo_auto_update` (default
`true`), `geo_fetch_via` (default `""`). The DB-IP files are fetched on
first `map.paths` call when missing (and then monthly), not at install.

Installer: copy bundled `.dat` files only if `geo.meta` is absent or older
than the bundle — a reinstall must not downgrade a downloaded file.

CLI: `em-wall geo [status|update [FILE]|rollback]`.

UI: Settings → Geo data section (status per file, source, auto-update,
fetch via, Update now, Roll back).

## 2. Group suggestions (`daemon/suggest*.go`)

Input: every DNS query name the daemon logs (`storeLogSink.Log`) is also
offered, non-blocking, to the suggester. Never on the DNS hot path: a
bounded channel, dropped on full, processed in a background goroutine.

Per name: skip if a rule matches it or em-wall itself resolves exactly
that name (`core/selfuse` + store-derived server/subscription hosts); else
credit it to its one best suggestible tag (`MatchesFor`: `full:` match,
then longest suffix, then a curated-group tag, then the smallest) → bump
that tag's hits, first/last-seen and seen-names (cap 10). In memory,
window 7 days. A tag is listed after 5 hits spread over 10 minutes.

`suggest.list` returns tags with hits, minus:
- tags in `Broad`;
- tags dismissed "never" (`suggest_dismissed` setting, JSON list);
- tags in "not now" cooldown (`suggest_snoozed`, JSON tag → until; 7 days);
- tags whose patterns are fully covered by stored rules.

Each suggestion carries `Missing` (patterns no rule covers) and
`Collisions`: existing groups (curated + custom) whose patterns overlap the
tag, with the overlap's rule actions/interfaces.

`suggest.accept` → custom group with `source_tag` set (new nullable column
on `custom_groups`), patterns = `Missing` unless given, plus `CIDRs` when
`IncludeIPs` (cap 64; more requires the UI to pass them explicitly). No rules
are created — the UI opens the normal apply flow, preselecting the
collision's interface for "use same set".

`suggest.split` → see invariant above.

`groups.list`: for custom groups with `source_tag`, fill `SourceTag`,
`TagNew`, `TagGone`. `groups.sync-tag` adds `TagNew` to the definition;
the existing `MissingPatterns`/`groups.sync` flow then carries them to rules.

Setting `suggest_enabled` (default `true`) turns the suggester off.

Each suggestion also carries `Builtin` (a curated group to apply instead:
named like the tag, or covering most seen names, and not fully applied) and
`UsedBy` (em-wall's own hostnames its patterns cover). Collisions are only
groups whose overlap existing rules route.

UI: the Suggestions page, master–detail: a compact list (name, seen hosts,
"built-in: X" / "+N domains") and the selected suggestion's seen names,
domains (collapsed), em-wall-use note, collisions, and one primary action
(**Apply X** or **Create group**), **Not now**, quiet **Never**; collision
rows offer **Use same set** / **Split overlap** (with a preview of exactly
what moves, and Undo). Settings → Suggestions: on/off,
dismissed list with undo. Geosite groups show `+N from geosite` (Sync) and
`N no longer listed` (info).

## 3. World map (`daemon/map*.go`, `MapPanel.vue`)

Backend `map.paths`: from `liveConns` (extend `trackedConn` with destination
hostname, destination IP, rule ID/Interface, byte counters + a small rate
window). Group = the group (curated or custom) whose patterns contain the
matching rule's pattern. Hops per flow:
- `you`: the public IP (`net.public-ip` cache), located.
- `pool` (master entries only): the slot's node carrying the most bytes now
  (pool timeline counters), `Uncertain` + `Alternatives`.
- `server`: entry/proxy server host (resolved, cached), located.
- `exit`: `exitKeys` IP when it differs from `server`.
- `dest`: destination IP. FakeIP hides it: resolve the hostname locally
  for display only (cached, off the connection path, never blocking).
All lookups go through `geoDB.Locate`; nothing is sent anywhere new.
Flows aggregate by (group, iface, upstream, pool, dest country+city).

Frontend: new **Map** tab. `d3-geo` + `topojson-client` + world-atlas
`countries-110m.json` (bundled, no CDN). Arcs between hops, group icons
(`GroupIcon.vue`) travelling along them, speed/width ∝ rate, colour = group
colour. Filters: group, set. Hover: hop card (city, country, org/ASN, IP,
role, uncertain note, anycast note). City clusters. Polls `MapPaths` every
2 s while visible (pause when hidden, like other panels). Banner when
`cityData` is false ("country precision — city data downloading").

## Defaults

geosite source Loyalsoldier · weekly dat / monthly mmdb · "Not now" 7 days ·
ASN shown on map · suggestions on · auto-update on · fetch via direct only.
