package xray

// PortStart..PortEnd is the loopback range the supervisor binds for
// per-entry SOCKS5 inbounds. 100 slots is generous for typical use;
// expand if anyone bumps into it.
const (
	PortStart = 11800
	PortEnd   = 11899
)

// SlotPortStart..SlotPortStart+SlotCount-1 is a separate loopback range
// for the per-master dialer balancer slots (see dialer.go). Kept disjoint
// from the per-entry SOCKS range above. SlotCount bounds how many master
// entries can have a Dialer at once.
const (
	SlotPortStart = 11900
	SlotCount     = 32
)

// ApiPort is the loopback port the generated config binds for xray's gRPC
// API (dokodemo-door inbound tagged ApiTag). The supervisor drives live
// AddOutbound/RemoveOutbound + balancer-info calls against it via the
// `xray api` CLI. Kept clear of the entry (11800–11899) and slot
// (11900–11931) ranges. Always emitted.
const (
	ApiPort = 11932
	ApiTag  = "api"
)

// MetricsPort is the loopback port xray's metrics HTTP endpoint listens on.
// Its /debug/vars carries the burst observatory's per-outbound health
// (alive, delay, ping counts) — the only per-node health signal the CLI
// API doesn't expose — which the supervisor polls to park dead pool nodes.
const (
	MetricsPort = 11933
	MetricsTag  = "metrics"
)

// nextFreePort returns the lowest port in [PortStart, PortEnd] not in
// the used set, or ErrNoPortFree if the range is exhausted.
func nextFreePort(used map[int]bool) (int, error) {
	for p := PortStart; p <= PortEnd; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, ErrNoPortFree
}
