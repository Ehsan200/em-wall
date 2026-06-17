// Package netprobe is the single place that answers "is this endpoint
// reachable through this connector, and how fast?". It's connector-agnostic:
// anything implementing Connector (a proxy dialer today, a direct dialer or
// VPN tunnel tomorrow) can be probed the same way, and the result feeds both
// one-shot reachability tests and the LatencyTracker that ranks
// multi-outbound route bindings.
package netprobe

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"time"
)

// defaultTimeout bounds a probe when the caller's context carries no
// deadline. Long enough for a slow TLS-terminating chain, short enough that
// a black-holed connector doesn't hang a probe round.
const defaultTimeout = 15 * time.Second

// FallbackSNI is the ServerName used when the caller can't supply a hostname
// (an IP-literal target). Go's TLS stack rejects IP literals as ServerName,
// and an empty value makes some endpoints (notably Cloudflare on 1.1.1.1)
// EOF without a reply — which would leave the probe unable to tell a healthy
// hop from a black-holed one. A real hostname guarantees a TLS-layer reply
// (ServerHello, or at worst an unrecognized-name alert): any TLS-shaped
// answer proves bytes traversed the connector end-to-end.
const FallbackSNI = "www.cloudflare.com"

// Connector opens a connection to an endpoint. hostname (when non-empty) is
// what the connector sees — passing the original DNS name preserves SNI and
// keeps hostname-based ACLs working; an empty hostname falls back to the IP.
// proxy.Dialer satisfies this structurally.
type Connector interface {
	Dial(ctx context.Context, hostname string, ip net.IP, port int) (net.Conn, error)
}

// Target is an endpoint to probe. Host may be a hostname or an IP literal:
// an IP literal is dialed by IP (no connector-side DNS — a pure reachability
// check), a hostname is passed through so the connector resolves it.
type Target struct {
	Host string
	Port int
}

func (t Target) split() (hostname string, ip net.IP) {
	if ip = net.ParseIP(t.Host); ip == nil {
		hostname = t.Host
	}
	return
}

func (t Target) String() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// Stage marks how far a probe got — used by callers to phrase a failure
// (couldn't reach the connector vs. reached it but the chain is broken).
type Stage int

const (
	StageOK   Stage = iota // probe succeeded
	StageDial              // failed dialing through the connector
	StageTLS               // dialed, but the TLS handshake didn't complete
)

// Result is one probe outcome. Latency is meaningful only when OK.
type Result struct {
	OK      bool
	Latency time.Duration
	Stage   Stage
	Err     error
}

// Measure dials target through c and runs a TLS handshake to prove the path
// carries bytes end-to-end — not just that a SOCKS5/HTTP CONNECT was
// accepted, which chained outbounds (VLESS+XHTTP etc.) do before they dial
// their real upstream. Returns the round-trip latency. The connection is
// always closed before returning.
func Measure(ctx context.Context, c Connector, target Target) Result {
	hostname, ip := target.split()
	start := time.Now()
	conn, err := c.Dial(ctx, hostname, ip, target.Port)
	if err != nil {
		return Result{Stage: StageDial, Err: err}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	if err := ProbeTLS(conn, hostname, deadline); err != nil {
		return Result{Stage: StageTLS, Err: err}
	}
	return Result{OK: true, Latency: time.Since(start)}
}

// ProbeTLS performs a TLS handshake over conn to verify the path carries
// bytes. sni must be a hostname; an empty or IP-literal sni falls back to
// FallbackSNI. InsecureSkipVerify is on because we don't care that the cert
// matches — only that the far end speaks TLS. Closes conn (the handshake
// mutates its state past reasonable reuse) and returns the handshake error.
func ProbeTLS(conn net.Conn, sni string, deadline time.Time) error {
	if sni == "" || net.ParseIP(sni) != nil {
		sni = FallbackSNI
	}
	_ = conn.SetDeadline(deadline)
	tc := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true,
	})
	defer tc.Close()
	return tc.Handshake()
}
