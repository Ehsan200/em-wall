# em-wall

A native macOS firewall focused on **per-domain rules with per-interface routing**.

- Block specific domains (and wildcards: `*.y.com` matches `y.com` and any subdomain).
- Allow specific domains and route their traffic out a chosen interface (e.g. `utun3`).
- Optional toggle to block encrypted DNS (DoH/DoT), which would otherwise bypass the firewall.

## Architecture (current phase)

This is **phase 1**: the firewall is implemented as a privileged Go daemon that runs as a `LaunchDaemon` and intercepts DNS at `127.0.0.1:53`. There is no `NEPacketTunnelProvider` yet — that requires an Apple Developer Program membership and is the planned **phase 2** for app-store / public distribution.

```
                  ┌─────────────────────────────────┐
                  │ em-wall.app (Wails + Vue)       │
                  │ user-launched UI                │
                  └─────────────────┬───────────────┘
                                    │ unix socket /var/run/em-wall.sock
                                    │ newline-framed JSON-RPC
                  ┌─────────────────▼───────────────┐
                  │ em-walld (LaunchDaemon, root)   │
                  │  ┌──────────────────────────┐   │
                  │  │ core/rules    GORM+SQLite│   │
                  │  │ core/decision  matcher   │   │
                  │  │ core/dnsproxy  :53 udp+tcp│  ◄─── system DNS
                  │  │ core/routing  per-host    │   │
                  │  │ core/pfctl    DoH/DoT     │   │
                  │  └──────────────────────────┘   │
                  └─────────────────────────────────┘
```

### What the daemon does on each DNS query

1. Match the queried name against the rule set (using most-specific-wins; exact > wildcard at same depth).
2. **Block** rule → return `NXDOMAIN` with a 60s negative-cache TTL.
3. **Allow** rule with no interface → forward to upstream as normal.
4. **Allow** rule with an interface → forward upstream, then for each A/AAAA in the answer, install `route add -host <ip> -interface <iface>`. Routes auto-expire on TTL and on rule deletion.

Plain allows (no rule matched) are forwarded but **not logged**.

### Limitations of phase 1

- Apps with hardcoded IPs bypass DNS-layer filtering (closed in phase 2 with SNI/QUIC inspection).
- Encrypted DNS bypasses the daemon unless the **Block encrypted DNS** toggle is on (covered in `core/pfctl`).
- Per-host routes are best-effort — multiple domains pointing at the same IP with conflicting rules will collide.

## Repo layout

```
core/                    Go library, fully testable without root
├── rules/               GORM + SQLite store, wildcard matcher
├── decision/            Rule evaluation engine, in-memory cache
├── dnsproxy/            DNS server + multi-upstream forwarder
├── routing/             Per-host route installer (route shell wrapper)
├── pfctl/               pf anchor manager for DoH/DoT blocking
└── ipc/                 Unix-socket JSON-RPC, server + client

daemon/                  em-walld main, wires core/* together
app/                     Wails + Vue UI (separate Go module via go.work)
launchd/                 LaunchDaemon plist
scripts/                 install.sh / uninstall.sh
```

## Develop

Toolchain expected: Go 1.21+, Node 18+, [Wails v2](https://wails.io) installed at `~/go/bin/wails`.

```bash
make test            # run core unit tests (no root needed)
make run-daemon      # run em-walld locally on :5353 (no root)
make run-app         # in another terminal: wails dev
```

## Install (real firewall)

```bash
make install          # sudo, builds + installs daemon, plist, pf anchor
sudo networksetup -setdnsservers Wi-Fi 127.0.0.1
make run-app          # launch the UI to manage rules
```

## Uninstall

```bash
make uninstall                # leaves DB and logs in place
sudo ./scripts/uninstall.sh --purge   # also removes DB and logs
sudo networksetup -setdnsservers Wi-Fi empty
```

## Phase 2 (future): NEPacketTunnelProvider

The Go core (`core/*`) is OS-agnostic by design — same rule engine, same wildcard matcher, same SQLite layer. Phase 2 swaps the **OS integration** layer:

- `core/dnsproxy` is replaced by a Swift `NEDNSProxyProvider` that calls into Go via `c-archive`.
- `core/routing` is replaced by `NEPacketTunnelProvider` doing real split-tunnel re-emission.
- `core/pfctl` becomes redundant — the content filter sees all flows including DoH endpoints by SNI.
- The Wails UI mostly stays as-is, talking XPC instead of Unix socket.

Phase 2 requires a paid Apple Developer Program membership for the `com.apple.developer.networking.networkextension` entitlement.
