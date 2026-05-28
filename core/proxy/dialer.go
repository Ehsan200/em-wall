package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// ProbeFallbackSNI is the ServerName ProbeTLS uses when the caller
// can't supply a hostname (e.g. the test target is an IP literal).
// Go's TLS stack refuses IP literals as ServerName and an empty value
// causes some upstreams (notably Cloudflare on 1.1.1.1) to EOF without
// responding — leaving the probe unable to distinguish a healthy
// SOCKS5 hop from a black-holed outbound. Picking a real hostname
// guarantees a TLS-layer reply (ServerHello, or at worst an
// unrecognized-name alert), which is what we want: any TLS-shaped
// answer proves bytes traversed the upstream end-to-end.
const ProbeFallbackSNI = "www.cloudflare.com"

// ProbeTLS performs a TLS handshake over conn to verify the upstream
// proxy chain actually carries bytes — not just that SOCKS5 CONNECT
// (or HTTP CONNECT) returned success. Chained outbounds like
// VLESS+XHTTP accept the inbound SOCKS5 CONNECT immediately and only
// dial their real upstream when the first payload arrives, so a
// pure-Dial test reports OK even when the chain is broken. A TLS
// handshake forces a round-trip through the entire chain.
//
// sni must be a hostname; pass ProbeFallbackSNI for IP-literal
// targets. InsecureSkipVerify is on because we don't care that the
// cert matches the SNI — we only need the upstream to speak TLS.
//
// Closes conn (the handshake mutates its read/write state past a
// reasonable reuse point) and returns the handshake error, if any.
func ProbeTLS(conn net.Conn, sni string, deadline time.Time) error {
	if sni == "" || net.ParseIP(sni) != nil {
		sni = ProbeFallbackSNI
	}
	_ = conn.SetDeadline(deadline)
	tc := tls.Client(conn, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true,
	})
	defer tc.Close()
	return tc.Handshake()
}

// DefaultDialTimeout is how long we wait for the upstream proxy to
// connect and complete its handshake before giving up. Long enough to
// survive a slow proxy with TLS termination; short enough that a dead
// proxy doesn't hang the connection forever.
const DefaultDialTimeout = 15 * time.Second

// Dialer opens a TCP connection to host:port via an upstream proxy.
// hostname (if non-empty) is what the proxy sees in its CONNECT or
// SOCKS5 request — passing the original DNS name preserves SNI and
// keeps proxy-side ACLs working with hostnames instead of IPs. If
// hostname is empty we fall back to the IP literal.
type Dialer interface {
	Dial(ctx context.Context, hostname string, ip net.IP, port int) (net.Conn, error)
}

// NewDialer builds a Dialer for the given Proxy record. Returns an
// error if the protocol isn't supported.
func NewDialer(p Proxy) (Dialer, error) {
	switch p.Protocol {
	case ProtocolSOCKS5:
		return &socks5Dialer{p: p}, nil
	case ProtocolHTTP:
		return &httpConnectDialer{p: p}, nil
	default:
		return nil, fmt.Errorf("proxy: unsupported protocol %q", p.Protocol)
	}
}

// ---- SOCKS5 ----

type socks5Dialer struct{ p Proxy }

func (d *socks5Dialer) Dial(ctx context.Context, hostname string, ip net.IP, port int) (net.Conn, error) {
	addr := net.JoinHostPort(d.p.Host, strconv.Itoa(d.p.Port))
	var auth *xproxy.Auth
	if d.p.Username != "" || d.p.Password != "" {
		auth = &xproxy.Auth{User: d.p.Username, Password: d.p.Password}
	}
	upstream, err := xproxy.SOCKS5("tcp", addr, auth, contextDialer(ctx))
	if err != nil {
		return nil, fmt.Errorf("socks5: build dialer: %w", err)
	}
	// Pass hostname when available — SOCKS5h-style host-based forwarding.
	target := pickTarget(hostname, ip, port)
	if cd, ok := upstream.(xproxy.ContextDialer); ok {
		return cd.DialContext(ctx, "tcp", target)
	}
	return upstream.Dial("tcp", target)
}

// ---- HTTP CONNECT ----

type httpConnectDialer struct{ p Proxy }

func (d *httpConnectDialer) Dial(ctx context.Context, hostname string, ip net.IP, port int) (net.Conn, error) {
	addr := net.JoinHostPort(d.p.Host, strconv.Itoa(d.p.Port))
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("http-connect: dial proxy: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(DefaultDialTimeout))
	}

	target := pickTarget(hostname, ip, port)
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if d.p.Username != "" || d.p.Password != "" {
		creds := d.p.Username + ":" + d.p.Password
		req += "Proxy-Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte(creds)) + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("http-connect: send: %w", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("http-connect: read status: %w", err)
	}
	// Crude parse: "HTTP/1.1 200 ..." is the only success.
	if len(statusLine) < 12 || statusLine[9:12] != "200" {
		// Drain remaining headers for the error message context.
		conn.Close()
		return nil, fmt.Errorf("http-connect: upstream said: %s", trimCRLF(statusLine))
	}
	// Skip remaining response headers up to the blank line.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("http-connect: header drain: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// Reset deadline so the tunnel is open-ended.
	_ = conn.SetDeadline(time.Time{})

	// If the proxy sent extra bytes before the blank line was consumed,
	// they're sitting in the bufio.Reader. We wrap the conn so reads
	// drain that buffer first. Without this, any unread upstream bytes
	// would be lost on the first conn.Read by the caller.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, br: br}, nil
	}
	return conn, nil
}

// ---- helpers ----

// pickTarget returns "hostname:port" if hostname is non-empty,
// otherwise "ip:port". Hostname-preferred so SNI and proxy ACLs stay
// hostname-aware end-to-end.
func pickTarget(hostname string, ip net.IP, port int) string {
	if hostname != "" {
		return net.JoinHostPort(hostname, strconv.Itoa(port))
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
}

func trimCRLF(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\r' || s[len(s)-1] == '\n') {
		s = s[:len(s)-1]
	}
	return s
}

// contextDialer wraps a context.Context in an xproxy.Dialer that
// respects its deadline/cancellation. Required because xproxy.SOCKS5
// takes a static Dialer, not a per-call context.
func contextDialer(ctx context.Context) xproxy.Dialer {
	return &ctxDialer{ctx: ctx}
}

type ctxDialer struct{ ctx context.Context }

func (d *ctxDialer) Dial(network, addr string) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(d.ctx, network, addr)
}

// bufferedConn lets readers consume bytes still in a bufio.Reader
// before falling through to the underlying conn.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.br.Read(p) }

// ParseProxyURL is a convenience for tests and the proxies.test
// handler: build a Proxy record from a "scheme://[user[:pass]@]host:port"
// URL. Returns ErrInvalidProtocol for schemes we don't support.
func ParseProxyURL(raw string) (Proxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Proxy{}, err
	}
	var proto Protocol
	switch u.Scheme {
	case "socks5":
		proto = ProtocolSOCKS5
	case "http":
		proto = ProtocolHTTP
	default:
		return Proxy{}, ErrInvalidProtocol
	}
	host, portStr := u.Hostname(), u.Port()
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return Proxy{}, ErrInvalidPort
	}
	var user, pass string
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return Proxy{
		Protocol: proto, Host: host, Port: port,
		Username: user, Password: pass,
	}, nil
}
