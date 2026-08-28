# em-wall

![em-wall](assets/screenshots/main.png)

A macOS firewall that works at the DNS layer. Every domain lookup on your machine passes through em-wall before any connection is made — rules decide whether it is blocked, allowed, or routed through a specific network interface, SOCKS/HTTP proxy, or embedded Xray outbound.

- Block domains and wildcards (`*.example.com` matches the apex and all subdomains).
- Route specific domains out a chosen target: a raw interface (`utun3`), a configured upstream proxy (`proxy:work`), or an embedded Xray outbound (`xray:home`). Multi-target bindings (`xray:home,backup`) fall back to the first reachable member, ranked lowest-latency first.
- Curated domain groups (OpenAI, Anthropic, Google, Meta, X, Telegram, JetBrains, …) for one-click bulk rules, plus bulk enable/disable/delete on whole groups.
- Optional toggle to block encrypted DNS (DoH/DoT), which would otherwise bypass the firewall entirely.
- Live log of every DNS query with the decision that was applied.
- Embedded [xray-core](https://github.com/XTLS/Xray-core): manage outbounds, import VLESS/VMess/Trojan links, edit raw JSON in a Monaco editor, per-entry latency test, capped rolling log.
- **Subscriptions**: add base64 share-link subscription URLs; em-wall fetches them on a schedule into a node pool (direct, falling back through a working node when the URL is censored), with per-node disable that survives refresh.
- **Master-config dialer**: turn any Xray entry into a "master" whose own transport is tunneled through the *fastest* of a chosen set of nodes/subscriptions — a live `leastPing` balancer that re-picks the winner and absorbs node-list changes **without restarting Xray**.

## How it works

Two binaries talk over a Unix socket:

```
em-wall.app  (Wails + Vue, user-space)
     │
     │  /var/run/em-wall.sock  (newline-framed JSON-RPC)
     │
em-walld  (LaunchDaemon, root)
  ├─ core/rules      GORM + SQLite rule store
  ├─ core/decision   rule engine, in-memory cache
  ├─ core/dnsproxy   UDP + TCP server on 127.0.0.1:53
  ├─ core/routing    per-host route installer via /sbin/route; utun owner labels (lsof)
  ├─ core/proxy      SOCKS/HTTP upstream proxies (proxy:NAME target)
  ├─ core/proxytun   userspace TUN that funnels matched traffic into a proxy
  ├─ core/xray       embedded xray-core lifecycle + config builder
  └─ core/pfctl      pf anchor for DoH/DoT blocking
```

The daemon owns everything — the UI is a thin client that forwards calls over IPC. On each DNS query the engine finds the most-specific matching rule (exact beats wildcard at the same depth) and dispatches:

- **block** → NXDOMAIN with a negative-cache TTL.
- **allow** with no interface → forward upstream as normal.
- **allow/route** with `Interface = "utunN"` → forward, then install `route -host <ip> -interface utunN` for every A/AAAA in the answer.
- **route** with `Interface = "proxy:NAME"` or `"xray:NAME"` → install routes that point at em-wall's userspace TUN, which forwards matched flows into the configured SOCKS/HTTP proxy or Xray outbound. Comma-separated lists (`proxy:work,backup`) fall back to the first reachable member, ranked lowest-latency first.

### Subscriptions & master-config dialer

A **subscription** is a remote URL that returns a base64 list of share links. The daemon fetches it on a schedule (default 12h, plus manual + on-start), parses the links into a pool of nodes, and keeps them fingerprinted so a manually-disabled node stays disabled across refreshes. Subscriptions are never a rule target on their own.

A **master** is an ordinary Xray entry with a `Dialer` field — a list of `xraysub:NAME` / `xray:NAME` / `proxy:NAME` refs. Every referenced node (all active nodes of each subscription, plus any individual entries/proxies) is thrown into one Xray `leastPing` balancer, and the master's outbound is wired to it via `streamSettings.sockopt.dialerProxy`. So the master's own server connection is tunneled through whichever node is currently fastest — useful when the master's server is itself reachable only through a working relay.

The "fastest" choice lives entirely inside Xray's balancer + observatory, so it re-picks the winner and survives node deaths mid-connection. Node churn (refresh, enable/disable, cap changes) is applied to the running Xray process **live over its gRPC API** (`ado`/`rmo`), never by a restart — so long-lived connections aren't dropped. Rules reach a master the normal way, with `xray:MASTER`.

## Repo layout

```
core/          Go library — fully testable without root
  rules/       SQLite store + wildcard matcher
  decision/    rule evaluation engine
  dnsproxy/    DNS server + multi-upstream forwarder
  routing/     per-host route installer + utun owner labels
  proxy/       SOCKS/HTTP upstream proxy registry
  proxytun/    userspace TUN that funnels matched flows into a proxy
  xray/        embedded xray-core lifecycle + JSON config builder
  pfctl/       pf anchor manager
  ipc/         Unix-socket JSON-RPC (protocol.go is the wire contract)
  groups/      curated domain group definitions
  version/     build-time version stamp

daemon/        em-walld — wires core/* together, runs as LaunchDaemon
cli/           em-wall — command-line IPC client (same wire contract as the app)
app/           Wails + Vue 3 UI (separate Go module via go.work)
  app.go       thin IPC client; every method forwards one RPC call
  internal/installer/  in-app install / uninstall logic
  frontend/    Vite + Vue 3 + TypeScript (Rules / Logs / Network / Proxies / Xray / Settings)

assets/        source assets (app icon, screenshots)
launchd/       LaunchDaemon plist template
```

## Develop

**Required:** Go 1.21+, Node 18+, [Wails v2](https://wails.io) at `~/go/bin/wails`.

```bash
make test            # core + cli unit tests — no root, no port 53 needed
make run-daemon      # local daemon on :5353 with ./tmp/dev.db (no root)
make run-app         # wails dev UI — run in a second terminal
make cli             # build/em-wall — point it at a dev daemon with --socket
```

`make run-daemon` starts em-walld against a local DB and socket so the UI can connect without touching system DNS. The install panel will show `not packaged` — that is expected in dev; the rule/log/network tabs work normally.

### Adding a feature

The IPC protocol is the single source of truth. Adding a method:

1. Define the DTO and method constant in [core/ipc/protocol.go](core/ipc/protocol.go).
2. Register the handler in `daemon/main.go` → `registerHandlers`.
3. Expose a method on `app/app.go` that calls `a.call(ipc.MethodXxx, ...)`.
4. Run `wails generate module` inside `app/` to regenerate the TypeScript bindings.

### Build a distributable app

```bash
make app-bundle      # builds app/build/bin/em-wall.app (fully self-contained)
```

The Makefile always rebuilds the daemon from source before bundling so the embedded binary is never stale.

## Install

Open `em-wall.app`. On first launch an **Install** screen appears — clicking it triggers the standard macOS admin prompt and writes the daemon, LaunchDaemon plist, pf anchor, and the `em-wall` CLI to their system paths. After install, activate the DNS hijack from the **Settings** tab.

## CLI

`em-wall` is installed to `/usr/local/bin` alongside the daemon. It is a thin client over the same Unix socket the app uses — no separate install step, and no `sudo`: the socket is group-`staff` readable.

```bash
em-wall status                            # daemon health, upstream, rule count
em-wall rules list --action route         # filter by action, or --match SUBSTR
em-wall rules add '*.example.com' --action route --iface xray:tokyo
em-wall rules disable 12 13               # also: enable, rm

em-wall group add "Work AI" -p '*.openai.com,api.anthropic.com'
em-wall group add "Work AI" --from-file patterns.txt   # one per line, # comments ok
em-wall group apply work-ai --action route --iface xrayset:fast
em-wall group edit work-ai --add-pattern gemini.google.com
em-wall group sync work-ai                # create rules for patterns added since
em-wall group list --custom
```

Custom-group keys carry a `custom:` prefix; the bare name works everywhere a key is accepted. Pass `--json` to any command for the raw DTO, and `--socket PATH` to target a dev daemon. Exit codes: `0` ok, `1` the daemon rejected the request, `2` bad arguments, `3` daemon unreachable.

Note that `group sync --all` targets every group the daemon considers applied, and "applied" means a stored rule matches one of the group's patterns — a hand-written rule can therefore pull an overlapping curated group into scope. The command prints its target list before syncing.

## Uninstall

**Settings → Uninstall em-wall** inside the running app. The flow deactivates the DNS hijack first (restoring your original per-service DNS from its saved backup), removes the daemon and all system files, and runs a safety sweep to ensure no network service is left pointing at `127.0.0.1`.
