# Subscriptions as a rule exit (`xraysub:NAME`)

Status: built 2026-11-10 (branch `feat/xray-sub-exit`), not yet run against a live daemon.

A rule can route through a subscription's node pool directly: the node is
the **exit**, not a hop in front of a master. This reverses the 2026-07-11
decision "no `xraysub:` rule prefix" — a subscription is still also usable
inside a master's `Dialer`, unchanged.

The risk is the exit IP. A pool of N nodes can leave the internet from one
address (fine) or from many (a site sees the user hop between IPs — and
countries — between connections, which logins, banking and anti-fraud
checks treat as a hijacked session). em-wall measures every node's exit,
shows a verdict, and requires an explicit confirmation before a rule binds
a subscription whose nodes do not share one exit IP.

## Decisions

| # | topic | decision |
|---|---|---|
| 1 | target | `xraysub:NAME`, exactly one subscription per rule |
| 2 | selection | xray's slot balancer — shortlist, parking, path probes and the subscription's strategy (stable/agile/manual/auto) all apply as they do for a master's pool. Never re-implemented daemon-side. |
| 3 | spread | unchanged: the balancer spreads over `slotBalancerExpected` (2) nodes. The warning says one page load can leave from 2 IPs. |
| 4 | slot | each subscription exit has its own slot whose members are exactly that subscription's nodes. It shares a master's slot only when that master's Dialer is exactly `xraysub:NAME` (same pool key). |
| 5 | counted nodes | `Active` nodes (enabled, within the cap) — parked ones included, they return on trial. Under the manual strategy with pins: the pinned nodes only. |
| 6 | same exit | exact IPv4 address |
| 7 | rotation | a node seen with ≥2 exit IPs within 24 h is a rotating exit → the subscription is multi-exit even if it has one node. Seen IPs persist (DB) for 24 h. |
| 8 | unmeasured | verdict from measured nodes, plus "N not measured". No node measured → unverified. |
| 9 | multi-exit | allowed after warn + confirm. No exit pinning feature (the manual strategy already lets a user pin nodes). |
| 10 | confirm scope | per subscription: the set of exit IPs the user accepted |
| 11 | drift | a new exit IP outside the accepted set re-raises the warning once per subscription; rules keep routing |
| 12 | alert | in-app only: Rules banner + rule chip + subscription panel, `em-wall status`, daemon log |
| 13 | not now | `xraysub:a,b` fallback lists; `xraysub:` members in outbound sets |
| 14 | lifecycle | rename cascades into rules; delete refused while a rule uses it; disabled/missing subscription → rule fails closed (NXDOMAIN) |
| 15 | pickers | wherever `xray:` is offered (rule editor, bulk retarget, group apply) |

## Routing path

```
rule xraysub:NAME
  └─ decision.Engine.Reload expands → proxy:_xsub_<subID>
       └─ hidden proxy row _xsub_<id> = socks5 127.0.0.1:SlotPort(i)
            └─ xray slotI-in → slotI-bal (leastLoad) → slotI-out-<fp>  (node = exit)
```

- **Expansion** (`xray.Store.Expansions`, the existing `InterfaceExpander`):
  `xraysub:NAME` → `proxy:_xsub_<id>` for an *enabled* subscription.
  Disabled or missing → absent from the map → the raw `xraysub:` string is
  kept → fails closed, exactly like a disabled set. Daemon cold-path
  readers (`expandIface`, `expandRuleIfaces`) learn the new prefix too.
- **Hidden proxy row** `_xsub_<id>` (`xray.SubProxyName`), keyed by ID so a
  rename never touches it and the name stays inside the proxy name limit.
  `IsInternalProxyName` covers both `_xray_` and `_xsub_` so the Proxies
  UI hides it. The supervisor writes it only while the subscription has a
  slot; with no slot (no active node, xray not running) there is no row,
  the lookup fails, and the rule fails closed.
- **Slot**: `xray.DialerSlot` gains `Exits []SlotExit{Sub, SubID}`. Slots
  are resolved masters first (unchanged indices), then one exit-only slot
  per subscription bound by an enabled rule whose pool key no master
  already has. An exit-only slot has `Master = "sub:NAME"` as its owner
  label (shortlist, timeline and strategy state key by it; `:` can't occur
  in an entry name) and `ExitOnly = true`: `SlotMasters()` returns no
  masters for it, so Generate emits no `dialer-` outbound and no chain
  probes. `ProxyNames()` returns the hidden proxy names riding a slot
  (masters' `_xray_` + exits' `_xsub_`) for the stale-path sweeper and the
  agile pick's stale marking.
- **xray process** runs when there is an enabled entry **or** an enabled
  subscription with an active node (exit measurement needs it, see below).

## Exit measurement

- **Not done here**: the CLI has no `rules update` command, so the flag
  exists on `rules add`, `group apply` and `group sync` only.
- **Probe outbounds**: Generate emits one outbound per active node of every
  enabled subscription, tag `xexit-<subID>-<fp>` (never starting with
  `slot`, so the observatory ignores it), reachable only through the probe
  inbound (`xray.ProbePort`, routed by SOCKS username). This covers parked
  nodes and subscriptions no rule uses yet, so a verdict exists *before*
  the user binds a rule. Outbounds are inert until dialed; live apply
  handles them by tag like any other.
- **Prober** (`daemon/xray_subexit.go`): every `subExitProbeInterval`
  (15 min), ~60 s after a subscription refresh, and on demand
  (`xraysub.checkExits`). Sequential per subscription, `subExitParallel`
  at a time, `subExitTimeout` per node. Primary prober
  `https://1.1.1.1/cdn-cgi/trace` (IP literal → the node's server dials
  IPv4, so the exit is IPv4; `ip=` and `loc=`; no rate limit), fallbacks
  `https://1.0.0.1/cdn-cgi/trace` then `https://www.cloudflare.com/cdn-cgi/trace`
  (an IPv6 answer is rejected). At most `subExitNodeBudget` (14 s) per node. ip-api is not used here — a 60-node
  pool would eat its free tier.
- **Store**: table `xray_sub_exits` (sub_id, fingerprint, ip, country,
  first_seen, last_seen), unique (sub_id, fingerprint, ip). A probe upserts
  `last_seen`; rows older than `subExitMemory` (24 h) are pruned. Rows for
  a deleted subscription are deleted with it.
- **Verdict** (`subExitVerdict`, pure):
  - counted nodes = decision 5
  - `single`: every measured counted node has exactly one IP and they are
    all equal
  - `multi`: ≥2 distinct IPs across counted nodes (a rotating node counts)
  - `unverified`: no counted node measured
  - plus: distinct exits with country and node count, `rotating` nodes,
    measured / unmeasured counts, last check time.
- **Acknowledgement**: column `xray_subscriptions.exit_ack` (sorted
  comma-joined IPs) + `exit_ack_at`. `NeedsAck` = verdict `multi` and some
  current exit IP is not in `exit_ack`. Accepting stores the current set.

## Confirm (enforced by the daemon)

`rules.add`, `rules.update`, `rules.bulkUpdate` and `groups.apply` take
`ackExits bool`. When the target is `xraysub:NAME` and its verdict
`NeedsAck`, the call fails with an error starting `multi-exit:` unless
`ackExits` is set; with it, the current exit set is accepted, then the
write proceeds. `unverified` does not block (nothing to confirm yet; the
banner appears once measurement finds multiple exits). The CLI gains
`--accept-multi-exit`.

## Drift

After each measurement the daemon compares `NeedsAck` per subscription.
On a false → true edge for a subscription bound by at least one enabled
rule it logs `xray sub NAME: new exit IP(s) … — confirm in the app`. The
app shows a Rules-page banner listing such subscriptions with an
**Accept** button (calls `xraysub.ackExits`). `em-wall status` prints a
warning line. Rules keep routing throughout.

## IPC

- `xraysub.exits` → `[]XraySubExitDTO` (all subscriptions)
- `xraysub.ackExits {id}` → accepts the current exit set
- `xraysub.checkExits {id}` (0 = all) → runs a measurement now, returns
  the fresh verdicts
- `StatusResult.ExitWarnings []string` — subscriptions needing a confirm
  that rules use
- `XraySubDTO.RuleCount` — rules bound to it

## UI

- Rule editor / bulk retarget / group apply: binding kind **Subscription**
  with a `SearchSelect` of subscriptions, each option showing its verdict.
  Picking a `multi` one shows a red card: exit count, countries, the
  2-IPs-per-page-load note; saving asks a confirm dialog and then sends
  `ackExits`. `unverified` shows a grey note.
- Rules list: `xraysub:` rules show the subscription name with a verdict
  chip; a red chip while `NeedsAck`.
- Rules page banner while any used subscription `NeedsAck`.
- Subscriptions panel: verdict badge + exits (IP, country, nodes) +
  "Check now".
- Tour: a new step (new id) so it plays in "What's new".

## Phases

- [x] P1 core — `xraysub:` interface helpers, `SubProxyName`,
      `IsInternalProxyName`, `DialerSlot.Exits/ExitOnly/ProxyNames`,
      Generate (exit-only slots, probe outbounds), Expansions; tests.
- [x] P2 daemon routing — slot resolution with exits, hidden rows, xray
      run condition, cold-path expansion, rule validation, rename / delete
      / enable cascades, stale-path + agile marking; tests.
- [x] P3 measurement — `xray_sub_exits` + ack columns, prober, verdict,
      drift log, IPC methods, `ackExits` enforcement, status; tests.
- [x] P4 CLI + frontend — `--accept-multi-exit`, status line, pickers,
      chips, banner, subscription panel, tour step.
- [x] P5 docs — CLAUDE.md convention bullet.
