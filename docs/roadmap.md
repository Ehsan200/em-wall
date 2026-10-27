# Roadmap

Planned work that has been agreed but not built yet. When an item ships,
remove it from here and, if it changes what users can do, add a tour step
for it in `app/frontend/src/tour.ts` with a new step `id` — that is what
surfaces it in the app's "What's new" tour.

## Planned

### Block by IP / CIDR

Today an IP/CIDR rule only works with **Route**. A Block rule on an IP
pattern is accepted by validation and stored, but nothing enforces it:
traffic to that IP never passes through em-wall. It follows the normal
default route, and the only pf anchor we install (`core/pfctl`) blocks
encrypted DNS. `decision.Engine.DecideIP` does return `OutcomeBlock`, but
it is only consulted for connections that already reached the proxy utun.

Direction: put enabled IP/CIDR Block rules into a pf table in the
`em-wall` anchor and drop traffic to it, synced on every rule change the
same way `reconcileIPRoutes` syncs routes. Open questions:

- Allow-inside-Block exceptions (`10.0.0.0/8` block, `10.1.2.3` allow):
  pf needs a pass rule ordered before the block, or the table has to be
  built from the difference.
- The anchor's rules are loaded only while `block_encrypted_dns` is on
  (`pfctl.Manager.Sync`); IP blocking needs them independent of that
  setting, so the anchor body has to be built from both.
- Until this ships, the Rules page should warn when an IP Block rule is
  saved (the tour's "Limits today" card already says it isn't enforced).

## Discussed, not decided

- **Coexisting with VPN apps that take over DNS.** A VPN app that rewrites
  the system DNS displaces em-wall's `127.0.0.1` and every rule stops
  applying.
- **DNS server per domain** (e.g. `*.corp.internal` → `10.0.0.53`). Names
  routed to a `utunN` interface resolve through the global upstream list
  today, so a private name only works if the VPN's resolver is in it, and
  the query also reaches the local resolver.
