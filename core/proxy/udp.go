package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// UDP proxying uses SOCKS5 UDP ASSOCIATE (RFC 1928 §7). golang.org/x/net's
// SOCKS5 dialer only does TCP CONNECT, so the association + datagram
// framing here is hand-rolled. HTTP CONNECT proxies cannot carry UDP, so
// DialUDPAssociate rejects any non-SOCKS5 protocol.
//
// Lifecycle: a SOCKS5 UDP association lives exactly as long as its TCP
// control connection. We hold that conn open for the life of the
// UDPSession and tear the whole thing down when it closes — there's no
// per-datagram handshake, so the daemon handler relies on an idle
// timeout to reclaim a session.

// socks5 wire constants.
const (
	socks5Version   = 0x05
	socks5NoAuth    = 0x00
	socks5UserPass  = 0x02
	socks5NoAccept  = 0xFF
	socks5AuthVer   = 0x01
	socks5CmdAssoc  = 0x03
	socks5RepOK     = 0x00
	socks5ATYPv4    = 0x01
	socks5ATYPdomn  = 0x03
	socks5ATYPv6    = 0x04
	socks5UDPHdrMin = 4 + 4 + 2 // RSV(2)+FRAG(1)+ATYP(1) + min v4 addr(4) + port(2)
)

// UDPSession is an open SOCKS5 UDP association. WriteTo wraps a payload
// in the SOCKS5 UDP request header and sends it to the proxy's relay;
// Read strips the header off relayed datagrams and returns the payload.
// Not safe for concurrent WriteTo calls with itself, but one reader and
// one writer goroutine (the daemon's relay loops) is fine.
type UDPSession struct {
	ctrl  net.Conn     // TCP control conn — association lives as long as this is open
	relay *net.UDPConn // bound + connected to the proxy's relay address
}

// DialUDPAssociate opens a SOCKS5 UDP association via p and returns a
// session ready to relay datagrams. Only ProtocolSOCKS5 is supported.
func DialUDPAssociate(ctx context.Context, p Proxy) (*UDPSession, error) {
	if p.Protocol != ProtocolSOCKS5 {
		return nil, fmt.Errorf("proxy: UDP requires socks5, got %q", p.Protocol)
	}
	addr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))

	var d net.Dialer
	ctrl, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("socks5 udp: dial proxy: %w", err)
	}
	// Bound the whole handshake by the context deadline (or a default).
	if dl, ok := ctx.Deadline(); ok {
		_ = ctrl.SetDeadline(dl)
	} else {
		_ = ctrl.SetDeadline(time.Now().Add(DefaultDialTimeout))
	}

	if err := socks5Negotiate(ctrl, p); err != nil {
		ctrl.Close()
		return nil, err
	}

	// UDP ASSOCIATE request. DST.ADDR/PORT is the address we'll send
	// datagrams FROM; 0.0.0.0:0 means "any", which is what we want
	// since our relay socket's source port isn't known yet.
	req := []byte{socks5Version, socks5CmdAssoc, 0x00}
	req = appendSOCKSAddr(req, net.IPv4zero, "", 0)
	if _, err := ctrl.Write(req); err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("socks5 udp: associate request: %w", err)
	}

	relayAddr, err := readSOCKSReply(ctrl, p)
	if err != nil {
		ctrl.Close()
		return nil, err
	}

	relay, err := net.DialUDP("udp", nil, relayAddr)
	if err != nil {
		ctrl.Close()
		return nil, fmt.Errorf("socks5 udp: dial relay %s: %w", relayAddr, err)
	}

	// Clear the handshake deadline — the relay loops manage their own.
	_ = ctrl.SetDeadline(time.Time{})
	return &UDPSession{ctrl: ctrl, relay: relay}, nil
}

// WriteTo sends payload to dst through the relay, prefixed with the
// SOCKS5 UDP request header (RSV=0, FRAG=0).
func (s *UDPSession) WriteTo(payload []byte, dst *net.UDPAddr) error {
	buf := make([]byte, 0, 3+1+len(dst.IP)+2+len(payload))
	buf = append(buf, 0x00, 0x00, 0x00) // RSV(2) + FRAG(1)
	buf = appendSOCKSAddr(buf, dst.IP, "", dst.Port)
	buf = append(buf, payload...)
	_, err := s.relay.Write(buf)
	return err
}

// Read reads one relayed datagram, strips the SOCKS5 UDP header, and
// copies the payload into b. Returns the payload length. The datagram's
// embedded source address is discarded — in our connected-flow model
// every datagram in a session shares one (src, dst) pair.
func (s *UDPSession) Read(b []byte) (int, error) {
	buf := make([]byte, len(b)+512) // headroom for the header
	n, err := s.relay.Read(buf)
	if err != nil {
		return 0, err
	}
	payload, perr := stripSOCKSUDPHeader(buf[:n])
	if perr != nil {
		return 0, perr
	}
	return copy(b, payload), nil
}

func (s *UDPSession) SetReadDeadline(t time.Time) error { return s.relay.SetReadDeadline(t) }

// Close tears down both the relay socket and the control conn (which
// ends the association on the proxy side).
func (s *UDPSession) Close() error {
	err1 := s.relay.Close()
	err2 := s.ctrl.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// CtrlConn exposes the TCP control connection so the caller can watch
// for its closure (which invalidates the association) and tear the
// session down promptly.
func (s *UDPSession) CtrlConn() net.Conn { return s.ctrl }

// ---- handshake helpers ----

// socks5Negotiate performs SOCKS5 method negotiation and, if the proxy
// selects it, username/password authentication. Leaves conn positioned
// to send a request.
func socks5Negotiate(conn net.Conn, p Proxy) error {
	hasAuth := p.Username != "" || p.Password != ""
	var greeting []byte
	if hasAuth {
		greeting = []byte{socks5Version, 0x02, socks5NoAuth, socks5UserPass}
	} else {
		greeting = []byte{socks5Version, 0x01, socks5NoAuth}
	}
	if _, err := conn.Write(greeting); err != nil {
		return fmt.Errorf("socks5: greeting: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5: method reply: %w", err)
	}
	if resp[0] != socks5Version {
		return fmt.Errorf("socks5: bad version 0x%02x", resp[0])
	}
	switch resp[1] {
	case socks5NoAuth:
		return nil
	case socks5UserPass:
		return socks5UserPassAuth(conn, p)
	case socks5NoAccept:
		return fmt.Errorf("socks5: proxy rejected all auth methods")
	default:
		return fmt.Errorf("socks5: unexpected method 0x%02x", resp[1])
	}
}

func socks5UserPassAuth(conn net.Conn, p Proxy) error {
	if len(p.Username) > 255 || len(p.Password) > 255 {
		return fmt.Errorf("socks5: username/password too long")
	}
	buf := []byte{socks5AuthVer, byte(len(p.Username))}
	buf = append(buf, p.Username...)
	buf = append(buf, byte(len(p.Password)))
	buf = append(buf, p.Password...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5: auth send: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("socks5: auth reply: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("socks5: authentication failed")
	}
	return nil
}

// readSOCKSReply reads a SOCKS5 reply and returns the relay UDP address
// for an ASSOCIATE. If the proxy returns an unspecified BND.ADDR
// (0.0.0.0 / ::), the relay shares the proxy host's IP — a common
// convention — so we substitute the control conn's remote IP.
func readSOCKSReply(conn net.Conn, p Proxy) (*net.UDPAddr, error) {
	head := make([]byte, 4) // VER REP RSV ATYP
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, fmt.Errorf("socks5: reply header: %w", err)
	}
	if head[0] != socks5Version {
		return nil, fmt.Errorf("socks5: bad reply version 0x%02x", head[0])
	}
	if head[1] != socks5RepOK {
		return nil, fmt.Errorf("socks5: associate rejected (REP=0x%02x)", head[1])
	}
	var ip net.IP
	switch head[3] {
	case socks5ATYPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		ip = net.IP(b)
	case socks5ATYPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		ip = net.IP(b)
	case socks5ATYPdomn:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return nil, err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		resolved, err := net.ResolveIPAddr("ip", string(b))
		if err != nil {
			return nil, fmt.Errorf("socks5: resolve relay host: %w", err)
		}
		ip = resolved.IP
	default:
		return nil, fmt.Errorf("socks5: bad reply ATYP 0x%02x", head[3])
	}
	portB := make([]byte, 2)
	if _, err := io.ReadFull(conn, portB); err != nil {
		return nil, err
	}
	port := int(binary.BigEndian.Uint16(portB))

	if ip.IsUnspecified() {
		// Relay is on the same host as the control conn.
		if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
			ip = ta.IP
		}
	}
	return &net.UDPAddr{IP: ip, Port: port}, nil
}

// appendSOCKSAddr appends an ATYP + ADDR + PORT triple to buf. If host
// is non-empty it's encoded as a domain name; otherwise ip is used
// (v4 or v6). port is encoded big-endian.
func appendSOCKSAddr(buf []byte, ip net.IP, host string, port int) []byte {
	if host != "" {
		buf = append(buf, socks5ATYPdomn, byte(len(host)))
		buf = append(buf, host...)
	} else if v4 := ip.To4(); v4 != nil {
		buf = append(buf, socks5ATYPv4)
		buf = append(buf, v4...)
	} else {
		buf = append(buf, socks5ATYPv6)
		buf = append(buf, ip.To16()...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	return append(buf, p[:]...)
}

// stripSOCKSUDPHeader parses a relayed datagram and returns just the
// payload (the bytes after the SOCKS5 UDP request header). FRAG != 0 is
// rejected — we don't reassemble fragments.
func stripSOCKSUDPHeader(d []byte) ([]byte, error) {
	if len(d) < socks5UDPHdrMin {
		return nil, fmt.Errorf("socks5 udp: short datagram (%d bytes)", len(d))
	}
	if d[2] != 0x00 {
		return nil, fmt.Errorf("socks5 udp: fragmented datagram unsupported (FRAG=0x%02x)", d[2])
	}
	atyp := d[3]
	off := 4
	switch atyp {
	case socks5ATYPv4:
		off += 4
	case socks5ATYPv6:
		off += 16
	case socks5ATYPdomn:
		if len(d) < off+1 {
			return nil, fmt.Errorf("socks5 udp: truncated domain length")
		}
		off += 1 + int(d[off])
	default:
		return nil, fmt.Errorf("socks5 udp: bad ATYP 0x%02x", atyp)
	}
	off += 2 // port
	if off > len(d) {
		return nil, fmt.Errorf("socks5 udp: header overruns datagram")
	}
	return d[off:], nil
}
