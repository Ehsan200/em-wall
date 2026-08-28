# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

```bash
make test                # core unit tests (no root, no port 53)
go test ./core/rules/... # run tests for a single package
go test -run TestX ./core/decision  # single test
make run-daemon          # local daemon: ./tmp/dev.db, ./tmp/em-wall.sock, :5353, system DNS untouched
make run-app             # Wails dev UI; stages embedded resources first (separate terminal)
make daemon              # builds build/em-walld
make cli                 # builds build/em-wall (command-line client)
make app-bundle          # primary user-facing build: stages resources + `wails build` → app/build/bin/em-wall.app
make tidy                # `go mod tidy` in both modules
```

Install/uninstall happens from inside the .app — see "In-app install / uninstall" below. The `em-wall` CLI ships as another embedded resource of that installer, so it has no install path of its own either.

The repo is a **Go workspace** (`go.work`) with two modules: the root module (daemon + core/) and `app/` (Wails UI). Use `go work` semantics — running `go test ./...` from root only sees the root module; the app module has its own `go.mod`. Frontend lives in `app/frontend` (Vite + Vue 3 + TS); `wails dev` handles its build.

## Architecture

This is a macOS firewall built around **DNS-layer interception**. There are two binaries that talk over a Unix socket:

- **`daemon/` → `em-walld`** — privileged process (LaunchDaemon, root). Owns the SQLite store, runs the DNS proxy on `127.0.0.1:53`, manages per-host routes via `/sbin/route`, and exposes the IPC server. Wires `core/*` packages together.
- **`app/` → Wails app** — unprivileged user-launched UI. Pure thin client; every `App` method on [app/app.go](app/app.go) just forwards an IPC call. **All real work lives in the daemon.**
- **`cli/` → `em-wall`** — unprivileged command-line client, installed to `/usr/local/bin` by the same in-app installer. Also a pure thin client (`status`, `rules`, `group` subcommands); it never opens the DB. The socket is mode 0660 group `staff`, so no `sudo`. Exit codes are part of its contract: `0` ok, `1` daemon-side error, `2` usage, `3` unreachable. Subcommand flags parse via `parseFlags` in [cli/main.go](cli/main.go), which permutes so flags may follow positionals — the stdlib `flag` package otherwise stops at the first non-flag word.

The IPC protocol is **newline-framed JSON-RPC** over `/var/run/em-wall.sock`. The single source of truth for method names and payload shapes is [core/ipc/protocol.go](core/ipc/protocol.go) — adding a feature means: define DTO + method constant there, register handler in `daemon/main.go` `registerHandlers`, expose method on `app/app.go`. The Wails frontend gets a typed binding for free via `wailsjs/`. Surfacing the same feature in the CLI is optional and independent — `cli/` reads the DTOs directly from `core/ipc`, so it needs no generated bindings.

### `core/` is intentionally OS-agnostic

The OS-coupled bits (`dnsproxy`, `routing`, `pfctl`) are isolated behind interfaces; `rules`, `decision`, `groups`, `ipc` are pure Go.

```
core/rules       — GORM+SQLite store, wildcard matcher (`*.x.com` matches apex + subs)
core/decision    — Engine: caches rule list (atomic.Pointer), Decide(name) → block/allow/route
core/dnsproxy    — UDP+TCP server on miekg/dns; takes Decider, Forwarder, Routes, Proxies via interfaces
core/routing     — Per-host route installer; sweeps expired entries on TTL
core/pfctl       — Manages an `em-wall` pf anchor for DoH/DoT blocking
core/groups      — Curated bundles of patterns ("Anthropic", "OpenAI") for one-click rules
core/ipc         — JSON-RPC over Unix socket; protocol.go is the wire contract
```

### Decision flow per DNS query

1. `decision.Engine` finds the most-specific matching rule (`rules.MostSpecific`): exact > wildcard at same depth; ties broken by lower ID.
2. **Block** → return NXDOMAIN with negative-cache TTL.
3. **Allow** with no interface → forward upstream as normal.
4. **Allow/Route** with `Interface = "utunN"` → forward, then for each A/AAAA in the answer, install `route -host <ip> -interface utunN`.
5. **Route** with `Interface = "proxy:NAME[,...]"` / `"xray:NAME[,...]"` → resolve to the daemon-owned proxy utun (`cfg.ProxyTun`), serve a fake IP (FakeIP), pin it there, and record the IP → (proxyNames, hostname) mapping so the netstack TCP layer redials through the chosen upstream proxy. (The old `app:KEY` binding — resolve a VPN app's utun via lsof — was removed; it broke whenever a VPN app hijacked DNS. proxy:/xray: supersede it. The daemon hard-deletes any leftover `app:` rules on startup.)

Rule changes via IPC always call `engine.Reload(ctx)` after a store mutation; updates and deletes also call `router.RemoveByRule(id)` so per-host routes don't outlive their binding.

### System DNS hijack lifecycle

The daemon may put `127.0.0.1` into every network service's DNS. This is risky — losing DNS bricks all networking — so `activateSystemDNS` in [daemon/main.go](daemon/main.go) is defensive:

- Captures the pre-hijack per-service DNS into `system_dns_backup` (sanitized: loopback entries become "DHCP-supplied" so restore returns to Empty, not 127.0.0.1).
- Picks an upstream from a priority list (per-service manual → backup → `AllDHCPDNS` across non-tunnel ports → `scutil --dns` → 1.1.1.1/8.8.8.8) and **validates each candidate with a live query**.
- Refuses to activate if no candidate answers, even falling back to deactivating if we were stuck in a 127.0.0.1-only state from a prior bad run.

`AllDHCPDNS` (in [daemon/system_dns.go](daemon/system_dns.go)) exists specifically because when a VPN owns the default route, `scutil --dns` only sees the VPN-pushed resolver, which may be loopback or unreachable from the daemon.

### In-app install / uninstall

The installer is the **only** install path — there is no shell script counterpart. The flow lives in [app/internal/installer/](app/internal/installer/):

- The daemon binary, plist, and pf anchor stub are embedded into the Wails binary via `//go:embed all:resources`. The Makefile target `app-resources` populates that directory before `wails build` (or `wails dev`) runs. A `wails dev` build without it will set `IsPackaged()` false and the install panel will refuse to act.
- `installer.Install` extracts the embedded files into a temp dir, writes a bash script with the install steps inlined, and runs it via `osascript ... do shell script "..." with administrator privileges`. macOS shows the standard auth prompt; cancellation surfaces as `installer.ErrCancelled`, which `App.Install`/`App.Uninstall` translate into a literal "cancelled" error so the frontend can ignore it silently.
- `App.Uninstall` first asks the still-running daemon (over IPC) to deactivate the system DNS hijack so the daemon's saved per-service backup restores the *original* DNS. The uninstall script then runs a safety sweep at the very end: any service whose first DNS entry is still `127.0.0.1` is reset to DHCP-supplied, then `dscacheutil -flushcache` and `killall -HUP mDNSResponder` are invoked. This is the last line of defence against leaving the host with broken DNS if the deactivate IPC failed (daemon already crashed, lost backup, etc.). The Settings → Uninstall section requires typed confirmation (`uninstall` or `delete everything`) and offers a purge toggle for the rules DB and log file.
- `App.InstallStatus` (filesystem inspection) is local to the UI process — no IPC. The install panel polls it; daemon-side `Status()` is the regular IPC call that fails until the daemon is running.

## Conventions

- `Rule.Action` is one of `block`, `allow`, `route`. `route` requires non-empty `Interface`; `allow` requires empty.
- `Interface` field accepts a literal interface name (`utun3`), `proxy:NAME` / `proxy:NAME1,NAME2`, `xray:NAME` / `xray:NAME1,NAME2` (multi-name fallback, first available wins), or `xrayset:NAME` (see below).
- **Outbound sets** (`core/xray/set.go`, `core/xray/set_store.go`) are named, ordered bundles of outbounds — typed members `xray:a,proxy:b` — that a rule binds to as a unit via `xrayset:NAME`. The rule stores only the reference, so editing a set's membership reaches every rule bound to it with no migration (the opposite of curated groups, whose patterns are copied into rules). `decision.Engine.Reload` resolves the indirection once per reload through the `InterfaceExpander` interface (satisfied by `*xray.Store`), so `Decision.Interface` is always literal and nothing downstream — dnsproxy, proxytun, routing — knows sets exist. An all-xray set expands to `xray:a,b,c`; a set with any plain proxy member expands to `proxy:_xray_a,b` instead. Daemon readers that inspect stored rule rows directly (`reconcileIPRoutes`, `exitip`, `proxylatency`) expand for themselves via `handlerDeps.expandIface` / `expandRuleIfaces` in [daemon/xray_sets.go](daemon/xray_sets.go). A disabled or deleted set is left out of the expansion map, so its rules keep the raw `xrayset:` string and fail closed (NXDOMAIN) rather than leaking to the default route. Renaming an xray entry or proxy cascades into set membership; deleting one is refused while a set still names it.
- Disabled rules are skipped during matching, not deleted.
- Adding patterns to a curated group in `core/groups` does **not** reach users who already applied that group — their rules are rows in the DB. `groups.list` reports the gap per group as `MissingPatterns`, and `groups.sync` ([daemon/groups_sync.go](daemon/groups_sync.go)) inserts only those, copying the action/interface of the group's existing rules. The UI surfaces it as a "+N new" pill and a Sync / Sync all button. So: extend a group's pattern list freely, no migration needed.
- Settings live in the same SQLite DB as rules (key/value table), accessed via `store.GetSetting/SetSetting`. Stateful daemon decisions (`block_encrypted_dns`, `system_dns_active`, `upstream_dns`, `system_dns_backup`) round-trip through here.
- `MethodSettingsSet` has a side-effect for `block_encrypted_dns`: it calls `pf.Sync` to install/remove the anchor. New side-effecting settings keys go in the same handler.
