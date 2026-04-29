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
make app-bundle          # primary user-facing build: stages resources + `wails build` → app/build/bin/em-wall.app
make tidy                # `go mod tidy` in both modules
```

There is no CLI install path. Install/uninstall happens from inside the .app — see "In-app install / uninstall" below.

The repo is a **Go workspace** (`go.work`) with two modules: the root module (daemon + core/) and `app/` (Wails UI). Use `go work` semantics — running `go test ./...` from root only sees the root module; the app module has its own `go.mod`. Frontend lives in `app/frontend` (Vite + Vue 3 + TS); `wails dev` handles its build.

## Architecture

This is a macOS firewall built around **DNS-layer interception**. There are two binaries that talk over a Unix socket:

- **`daemon/` → `em-walld`** — privileged process (LaunchDaemon, root). Owns the SQLite store, runs the DNS proxy on `127.0.0.1:53`, manages per-host routes via `/sbin/route`, and exposes the IPC server. Wires `core/*` packages together.
- **`app/` → Wails app** — unprivileged user-launched UI. Pure thin client; every `App` method on [app/app.go](app/app.go) just forwards an IPC call. **All real work lives in the daemon.**

The IPC protocol is **newline-framed JSON-RPC** over `/var/run/em-wall.sock`. The single source of truth for method names and payload shapes is [core/ipc/protocol.go](core/ipc/protocol.go) — adding a feature means: define DTO + method constant there, register handler in `daemon/main.go` `registerHandlers`, expose method on `app/app.go`. The Wails frontend gets a typed binding for free via `wailsjs/`.

### `core/` is intentionally OS-agnostic

The package layout under `core/` is structured so it can survive a phase 2 port to `NEPacketTunnelProvider` / `NEDNSProxyProvider` (requires Apple Developer Program). The OS-coupled bits (`dnsproxy`, `routing`, `pfctl`) are isolated behind interfaces; `rules`, `decision`, `groups`, `applocator`, `ipc` are pure Go.

```
core/rules       — GORM+SQLite store, wildcard matcher (`*.x.com` matches apex + subs)
core/decision    — Engine: caches rule list (atomic.Pointer), Decide(name) → block/allow/route
core/dnsproxy    — UDP+TCP server on miekg/dns; takes Decider, Forwarder, Routes, Apps via interfaces
core/routing     — Per-host route installer; sweeps expired entries on TTL
core/pfctl       — Manages an `em-wall` pf anchor for DoH/DoT blocking
core/applocator  — Maps app keys (e.g. "tailscale") → currently-owned utun via lsof
core/groups      — Curated bundles of patterns ("Anthropic", "OpenAI") for one-click rules
core/ipc         — JSON-RPC over Unix socket; protocol.go is the wire contract
```

### Decision flow per DNS query

1. `decision.Engine` finds the most-specific matching rule (`rules.MostSpecific`): exact > wildcard at same depth; ties broken by lower ID.
2. **Block** → return NXDOMAIN with negative-cache TTL.
3. **Allow** with no interface → forward upstream as normal.
4. **Allow/Route** with `Interface = "utunN"` → forward, then for each A/AAAA in the answer, install `route -host <ip> -interface utunN`.
5. **Route** with `Interface = "app:KEY[,KEY...]"` → resolve the app key to its current utun via `applocator` (with read-lock around the install), then install routes pinned to that utun. The app watcher (1s tick in `daemon/main.go`) takes a write-lock and flushes routes on utun changes so a restarted VPN app doesn't strand traffic on a stale interface.

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

### Phase 1 vs phase 2

Phase 1 = current. DNS-layer enforcement only — apps with hardcoded IPs bypass it. Phase 2 swaps `core/dnsproxy` for `NEDNSProxyProvider` and `core/routing` for `NEPacketTunnelProvider` (split-tunnel re-emission); `pfctl` becomes redundant. The `core/*` rule engine and SQLite layer are the same in both phases. Don't add OS-coupled logic outside `dnsproxy`/`routing`/`pfctl`.

## Conventions

- `Rule.Action` is one of `block`, `allow`, `route`. `route` requires non-empty `Interface`; `allow` requires empty.
- `Interface` field accepts either a literal interface name (`utun3`) or `app:KEY` / `app:KEY1,KEY2` (multi-app fallback, first running wins).
- Disabled rules are skipped during matching, not deleted.
- Settings live in the same SQLite DB as rules (key/value table), accessed via `store.GetSetting/SetSetting`. Stateful daemon decisions (`block_encrypted_dns`, `system_dns_active`, `upstream_dns`, `system_dns_backup`) round-trip through here.
- `MethodSettingsSet` has a side-effect for `block_encrypted_dns`: it calls `pf.Sync` to install/remove the anchor. New side-effecting settings keys go in the same handler.
