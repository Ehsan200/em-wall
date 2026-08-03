package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These exercise the SOCKS5 UDP ASSOCIATE client end-to-end in process:
// a stub SOCKS5 proxy (TCP control + UDP relay) sits between the
// UDPSession and a real UDP echo origin. The netstack half (gvisor UDP
// forwarder → handler) needs root + a utun and is verified manually.

// startUDPEcho starts a UDP server that echoes each datagram back
// upper-cased. Returns its address.
func startUDPEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp echo listen: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo([]byte(strings.ToUpper(string(buf[:n]))), addr)
		}
	}()
	return pc.LocalAddr().String()
}

// startSOCKS5UDPProxy starts a minimal SOCKS5 proxy that supports UDP
// ASSOCIATE. The TCP control side negotiates (optionally user/pass),
// replies to ASSOCIATE with the relay's address, and holds the conn
// open. The UDP relay unwraps each client datagram, forwards the
// payload to the requested destination, and wraps the reply back.
func startSOCKS5UDPProxy(t *testing.T, wantUser, wantPass string) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 udp listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("socks5 relay listen: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	relayAddr := relay.LocalAddr().(*net.UDPAddr)

	go runSOCKSUDPRelay(relay)

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if err := serveSOCKS5UDPControl(conn, relayAddr, wantUser, wantPass); err != nil {
					return
				}
				// Hold the control conn open — the association lives as
				// long as it does. Block until the client closes it.
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()

	h, pStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(pStr)
	return h, p
}

// serveSOCKS5UDPControl runs method negotiation + auth, then replies to
// an ASSOCIATE request with relayAddr. Returns once the reply is sent.
func serveSOCKS5UDPControl(c net.Conn, relayAddr *net.UDPAddr, wantUser, wantPass string) error {
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return err
	}
	if hdr[0] != socks5Version {
		return fmt.Errorf("bad version")
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}

	if wantUser != "" {
		if _, err := c.Write([]byte{socks5Version, socks5UserPass}); err != nil {
			return err
		}
		// VER ULEN UNAME PLEN PASSWD
		ah := make([]byte, 2)
		if _, err := io.ReadFull(c, ah); err != nil {
			return err
		}
		user := make([]byte, ah[1])
		_, _ = io.ReadFull(c, user)
		pl := make([]byte, 1)
		_, _ = io.ReadFull(c, pl)
		pass := make([]byte, pl[0])
		_, _ = io.ReadFull(c, pass)
		if string(user) != wantUser || string(pass) != wantPass {
			_, _ = c.Write([]byte{socks5AuthVer, 0x01})
			return fmt.Errorf("bad creds")
		}
		if _, err := c.Write([]byte{socks5AuthVer, 0x00}); err != nil {
			return err
		}
	} else {
		if _, err := c.Write([]byte{socks5Version, socks5NoAuth}); err != nil {
			return err
		}
	}

	// Request: VER CMD RSV ATYP ADDR PORT. Expect CMD=ASSOCIATE.
	rh := make([]byte, 4)
	if _, err := io.ReadFull(c, rh); err != nil {
		return err
	}
	if rh[1] != socks5CmdAssoc {
		return fmt.Errorf("expected ASSOCIATE, got cmd 0x%02x", rh[1])
	}
	if err := discardSOCKSAddr(c, rh[3]); err != nil {
		return err
	}

	reply := []byte{socks5Version, socks5RepOK, 0x00}
	reply = appendSOCKSAddr(reply, relayAddr.IP, "", relayAddr.Port)
	_ = c.SetReadDeadline(time.Time{})
	_, err := c.Write(reply)
	return err
}

// runSOCKSUDPRelay forwards each wrapped client datagram to its
// destination and wraps the reply back to the sender. Synchronous
// per-datagram — fine for the echo test.
func runSOCKSUDPRelay(relay *net.UDPConn) {
	buf := make([]byte, 64*1024)
	for {
		n, client, err := relay.ReadFromUDP(buf)
		if err != nil {
			return
		}
		dst, payload, perr := parseSOCKSUDPDatagram(buf[:n])
		if perr != nil {
			continue
		}
		out, err := net.DialUDP("udp", nil, dst)
		if err != nil {
			continue
		}
		_, _ = out.Write(payload)
		_ = out.SetReadDeadline(time.Now().Add(2 * time.Second))
		rbuf := make([]byte, 64*1024)
		rn, rerr := out.Read(rbuf)
		_ = out.Close()
		if rerr != nil {
			continue
		}
		wrapped := []byte{0x00, 0x00, 0x00}
		wrapped = appendSOCKSAddr(wrapped, dst.IP, "", dst.Port)
		wrapped = append(wrapped, rbuf[:rn]...)
		_, _ = relay.WriteToUDP(wrapped, client)
	}
}

// parseSOCKSUDPDatagram pulls the destination + payload out of a SOCKS5
// UDP request datagram (the test's relay-side counterpart to the
// client's stripSOCKSUDPHeader).
func parseSOCKSUDPDatagram(d []byte) (*net.UDPAddr, []byte, error) {
	if len(d) < socks5UDPHdrMin {
		return nil, nil, fmt.Errorf("short datagram")
	}
	if d[2] != 0x00 {
		return nil, nil, fmt.Errorf("fragmented")
	}
	atyp := d[3]
	off := 4
	var ip net.IP
	switch atyp {
	case socks5ATYPv4:
		ip = net.IP(d[off : off+4])
		off += 4
	case socks5ATYPv6:
		ip = net.IP(d[off : off+16])
		off += 16
	case socks5ATYPdomn:
		l := int(d[off])
		off++
		host := string(d[off : off+l])
		off += l
		resolved, err := net.ResolveIPAddr("ip", host)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		ip = resolved.IP
	default:
		return nil, nil, fmt.Errorf("bad atyp 0x%02x", atyp)
	}
	port := int(d[off])<<8 | int(d[off+1])
	off += 2
	return &net.UDPAddr{IP: ip, Port: port}, d[off:], nil
}

// discardSOCKSAddr reads and throws away an ATYP-prefixed addr+port from
// conn (the ASSOCIATE request's DST, which we ignore).
func discardSOCKSAddr(c net.Conn, atyp byte) error {
	switch atyp {
	case socks5ATYPv4:
		_, err := io.ReadFull(c, make([]byte, 4+2))
		return err
	case socks5ATYPv6:
		_, err := io.ReadFull(c, make([]byte, 16+2))
		return err
	case socks5ATYPdomn:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return err
		}
		_, err := io.ReadFull(c, make([]byte, int(l[0])+2))
		return err
	default:
		return fmt.Errorf("bad atyp 0x%02x", atyp)
	}
}

// udpRoundTrip sends "ping" to host:port through the session and asserts
// it gets "PING" back. host may be an IP literal or a domain name.
func udpRoundTrip(t *testing.T, s *UDPSession, host string, port int) {
	t.Helper()
	if err := s.WriteTo([]byte("ping"), host, port); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	_ = s.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "PING" {
		t.Fatalf("got %q, want PING", buf[:n])
	}
}

// ---- tests ----

func TestUDP_Associate_EndToEnd(t *testing.T) {
	echo := startUDPEcho(t)
	echoAddr, err := net.ResolveUDPAddr("udp", echo)
	if err != nil {
		t.Fatal(err)
	}
	host, port := startSOCKS5UDPProxy(t, "", "")

	sess, err := DialUDPAssociate(context.Background(), Proxy{
		Protocol: ProtocolSOCKS5, Host: host, Port: port,
	})
	if err != nil {
		t.Fatalf("DialUDPAssociate: %v", err)
	}
	defer sess.Close()
	udpRoundTrip(t, sess, echoAddr.IP.String(), echoAddr.Port)
}

// Fake-IP routed flows relay by hostname, not by the synthetic IP the
// client dialed — the proxy must receive ATYP=DOMAINNAME and resolve it
// on its own side.
func TestUDP_Associate_DomainDestination(t *testing.T) {
	echo := startUDPEcho(t)
	echoAddr, err := net.ResolveUDPAddr("udp", echo)
	if err != nil {
		t.Fatal(err)
	}
	host, port := startSOCKS5UDPProxy(t, "", "")

	sess, err := DialUDPAssociate(context.Background(), Proxy{
		Protocol: ProtocolSOCKS5, Host: host, Port: port,
	})
	if err != nil {
		t.Fatalf("DialUDPAssociate: %v", err)
	}
	defer sess.Close()
	udpRoundTrip(t, sess, "localhost", echoAddr.Port)
}

func TestUDP_Associate_Auth(t *testing.T) {
	echo := startUDPEcho(t)
	echoAddr, _ := net.ResolveUDPAddr("udp", echo)
	host, port := startSOCKS5UDPProxy(t, "carol", "p@ss")

	// Wrong creds → handshake fails.
	if _, err := DialUDPAssociate(context.Background(), Proxy{
		Protocol: ProtocolSOCKS5, Host: host, Port: port, Username: "carol", Password: "nope",
	}); err == nil {
		t.Fatalf("expected auth failure")
	}

	sess, err := DialUDPAssociate(context.Background(), Proxy{
		Protocol: ProtocolSOCKS5, Host: host, Port: port, Username: "carol", Password: "p@ss",
	})
	if err != nil {
		t.Fatalf("DialUDPAssociate with creds: %v", err)
	}
	defer sess.Close()
	udpRoundTrip(t, sess, echoAddr.IP.String(), echoAddr.Port)
}

func TestUDP_RejectsHTTPProxy(t *testing.T) {
	_, err := DialUDPAssociate(context.Background(), Proxy{
		Protocol: ProtocolHTTP, Host: "127.0.0.1", Port: 8080,
	})
	if err == nil {
		t.Fatalf("expected error for HTTP proxy, got nil")
	}
}
