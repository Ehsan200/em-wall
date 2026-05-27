package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests exercise the real protocol code paths end-to-end in
// process: a stub upstream proxy (HTTP CONNECT or SOCKS5) sits between
// the Dialer and a TCP echo origin. They prove the dialer speaks the
// proxy protocol correctly (incl. auth) and that the byte stream is
// intact through to the origin. The utun + netstack half of the data
// path needs root and a real interface, so it's verified manually
// against the running daemon, not here.

// startEcho starts a TCP server that echoes everything it reads back to
// the writer, upper-cased so the test can tell the round-trip happened.
// Returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write([]byte(strings.ToUpper(string(buf[:n]))))
					}
					if err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// roundTrip writes "ping" through conn and asserts it gets "PING" back.
func roundTrip(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "PING" {
		t.Fatalf("got %q, want PING", buf)
	}
}

// ---- HTTP CONNECT stub proxy ----

// startHTTPConnectProxy starts a minimal HTTP CONNECT proxy. If
// wantUser is non-empty it requires matching Basic credentials. Returns
// the proxy's host and port.
func startHTTPConnectProxy(t *testing.T, wantUser, wantPass string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http proxy listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveHTTPConnect(c, wantUser, wantPass)
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func serveHTTPConnect(c net.Conn, wantUser, wantPass string) {
	defer c.Close()
	br := bufio.NewReader(c)
	reqLine, err := br.ReadString('\n')
	if err != nil {
		return
	}
	// "CONNECT host:port HTTP/1.1"
	parts := strings.Fields(reqLine)
	if len(parts) < 2 || parts[0] != "CONNECT" {
		_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	target := parts[1]
	authOK := wantUser == ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(strings.ToLower(line), "proxy-authorization:") {
			// crude check: just confirm the header is present and decodes
			// to user:pass — good enough to prove the client sent creds.
			if strings.Contains(line, basicCreds(wantUser, wantPass)) {
				authOK = true
			}
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !authOK {
		_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer upstream.Close()
	_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	// Splice any buffered client bytes first, then both directions.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		_, _ = upstream.Write(b)
	}
	go func() { _, _ = io.Copy(upstream, c) }()
	_, _ = io.Copy(c, upstream)
}

func basicCreds(user, pass string) string {
	// base64 of "user:pass" — mirrors what the dialer's Basic header sends.
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// ---- SOCKS5 stub proxy ----

// startSOCKS5Proxy starts a minimal SOCKS5 server supporting no-auth
// and username/password. If wantUser is non-empty, only user/pass is
// offered and the credentials must match. Returns host and port.
func startSOCKS5Proxy(t *testing.T, wantUser, wantPass string) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("socks5 listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				if err := serveSOCKS5(conn, wantUser, wantPass); err != nil {
					_ = conn.Close()
				}
			}(c)
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func serveSOCKS5(c net.Conn, wantUser, wantPass string) error {
	br := bufio.NewReader(c)
	// Greeting: VER NMETHODS METHODS...
	ver, _ := br.ReadByte()
	if ver != 0x05 {
		return fmt.Errorf("bad version %d", ver)
	}
	nm, _ := br.ReadByte()
	methods := make([]byte, nm)
	if _, err := io.ReadFull(br, methods); err != nil {
		return err
	}
	needAuth := wantUser != ""
	if needAuth {
		// Choose username/password (0x02).
		if _, err := c.Write([]byte{0x05, 0x02}); err != nil {
			return err
		}
		// Auth: VER ULEN UNAME PLEN PASSWD
		if v, _ := br.ReadByte(); v != 0x01 {
			return fmt.Errorf("bad auth version")
		}
		ulen, _ := br.ReadByte()
		user := make([]byte, ulen)
		_, _ = io.ReadFull(br, user)
		plen, _ := br.ReadByte()
		pass := make([]byte, plen)
		_, _ = io.ReadFull(br, pass)
		if string(user) != wantUser || string(pass) != wantPass {
			_, _ = c.Write([]byte{0x01, 0x01}) // failure
			return fmt.Errorf("bad creds")
		}
		if _, err := c.Write([]byte{0x01, 0x00}); err != nil { // success
			return err
		}
	} else {
		if _, err := c.Write([]byte{0x05, 0x00}); err != nil { // no-auth
			return err
		}
	}

	// Request: VER CMD RSV ATYP DST.ADDR DST.PORT
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return err
	}
	if hdr[0] != 0x05 || hdr[1] != 0x01 { // only CONNECT
		return fmt.Errorf("unsupported cmd")
	}
	var host string
	switch hdr[3] {
	case 0x01: // IPv4
		b := make([]byte, 4)
		_, _ = io.ReadFull(br, b)
		host = net.IP(b).String()
	case 0x03: // domain
		l, _ := br.ReadByte()
		b := make([]byte, l)
		_, _ = io.ReadFull(br, b)
		host = string(b)
	case 0x04: // IPv6
		b := make([]byte, 16)
		_, _ = io.ReadFull(br, b)
		host = net.IP(b).String()
	default:
		return fmt.Errorf("bad atyp")
	}
	portB := make([]byte, 2)
	if _, err := io.ReadFull(br, portB); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(portB)

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))), 2*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // general failure
		return err
	}
	defer upstream.Close()
	// Success reply with a zero BND.ADDR/PORT.
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		_, _ = upstream.Write(b)
	}
	go func() { _, _ = io.Copy(upstream, c) }()
	_, _ = io.Copy(c, upstream)
	return nil
}

// ---- tests ----

func TestDialer_HTTPConnect_EndToEnd(t *testing.T) {
	echo := startEcho(t)
	echoHost, echoPortStr, _ := net.SplitHostPort(echo)
	echoPort, _ := strconv.Atoi(echoPortStr)

	host, port := startHTTPConnectProxy(t, "", "")
	d, err := NewDialer(Proxy{Protocol: ProtocolHTTP, Host: host, Port: port})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	conn, err := d.Dial(context.Background(), "", net.ParseIP(echoHost), echoPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
}

func TestDialer_HTTPConnect_Auth(t *testing.T) {
	echo := startEcho(t)
	echoHost, echoPortStr, _ := net.SplitHostPort(echo)
	echoPort, _ := strconv.Atoi(echoPortStr)

	host, port := startHTTPConnectProxy(t, "alice", "s3cret")

	// Wrong creds → 407 → dial fails.
	bad, _ := NewDialer(Proxy{Protocol: ProtocolHTTP, Host: host, Port: port, Username: "alice", Password: "wrong"})
	if _, err := bad.Dial(context.Background(), "", net.ParseIP(echoHost), echoPort); err == nil {
		t.Fatalf("expected auth failure, got success")
	}

	// Right creds → success.
	good, _ := NewDialer(Proxy{Protocol: ProtocolHTTP, Host: host, Port: port, Username: "alice", Password: "s3cret"})
	conn, err := good.Dial(context.Background(), "", net.ParseIP(echoHost), echoPort)
	if err != nil {
		t.Fatalf("Dial with creds: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
}

func TestDialer_SOCKS5_EndToEnd(t *testing.T) {
	echo := startEcho(t)
	echoHost, echoPortStr, _ := net.SplitHostPort(echo)
	echoPort, _ := strconv.Atoi(echoPortStr)

	host, port := startSOCKS5Proxy(t, "", "")
	d, err := NewDialer(Proxy{Protocol: ProtocolSOCKS5, Host: host, Port: port})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	conn, err := d.Dial(context.Background(), "", net.ParseIP(echoHost), echoPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
}

func TestDialer_SOCKS5_Auth(t *testing.T) {
	echo := startEcho(t)
	echoHost, echoPortStr, _ := net.SplitHostPort(echo)
	echoPort, _ := strconv.Atoi(echoPortStr)

	host, port := startSOCKS5Proxy(t, "bob", "hunter2")
	good, _ := NewDialer(Proxy{Protocol: ProtocolSOCKS5, Host: host, Port: port, Username: "bob", Password: "hunter2"})
	conn, err := good.Dial(context.Background(), "", net.ParseIP(echoHost), echoPort)
	if err != nil {
		t.Fatalf("Dial with creds: %v", err)
	}
	defer conn.Close()
	roundTrip(t, conn)
}

// TestForwarder_Splice verifies Splice ties two conns together and that
// data flows both ways — this is the core of the netstack handler's
// job once a connection is accepted and an upstream is dialed.
func TestForwarder_Splice(t *testing.T) {
	echo := startEcho(t)
	host, port := startSOCKS5Proxy(t, "", "")
	d, _ := NewDialer(Proxy{Protocol: ProtocolSOCKS5, Host: host, Port: port})
	echoHost, echoPortStr, _ := net.SplitHostPort(echo)
	echoPort, _ := strconv.Atoi(echoPortStr)

	upstream, err := d.Dial(context.Background(), "", net.ParseIP(echoHost), echoPort)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	// client <-> server is the stand-in for the netstack-accepted conn.
	client, server := net.Pipe()
	go func() {
		_, _, _ = Splice(server, upstream)
		_ = server.Close()
		_ = upstream.Close()
	}()
	defer client.Close()

	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "HELLO" {
		t.Fatalf("got %q, want HELLO", buf)
	}
}
