package xray

// PortStart..PortEnd is the loopback range the supervisor binds for
// per-entry SOCKS5 inbounds. 100 slots is generous for typical use;
// expand if anyone bumps into it.
const (
	PortStart = 11800
	PortEnd   = 11899
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
