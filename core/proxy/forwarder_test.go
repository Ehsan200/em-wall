package proxy

import (
	"net"
	"testing"
	"time"
)

// tcpPair returns both ends of a loopback TCP connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c, <-ch
}

func TestSpliceObservedReportsClientFirst(t *testing.T) {
	// client ↔ a  (splice)  b ↔ server
	client, a := tcpPair(t)
	b, server := tcpPair(t)
	go func() {
		_, _ = client.Write([]byte("hello"))
		_ = client.Close() // the client gives up; the server never answered
	}()
	// The server lingers after the client's FIN before closing, as xray
	// does (downlinkOnly), so the splice outlives the client side.
	go func() {
		buf := make([]byte, 16)
		for {
			if _, err := server.Read(buf); err != nil {
				time.Sleep(300 * time.Millisecond)
				_ = server.Close()
				return
			}
		}
	}()
	res := SpliceObserved(a, b, 0, nil)
	if res.FirstEnd >= 300*time.Millisecond {
		t.Fatalf("FirstEnd = %s, want the client side's end, not the splice's", res.FirstEnd)
	}
	if !res.AFirst || res.AtoB != 5 || res.BtoA != 0 {
		t.Fatalf("result = %+v, want client side first, 5 bytes out, none back", res)
	}
}

func TestSpliceObservedReportsServerFirst(t *testing.T) {
	client, a := tcpPair(t)
	b, server := tcpPair(t)
	defer client.Close()
	go func() {
		_, _ = server.Write([]byte("bye"))
		_ = server.Close()
	}()
	// Drain what the server sent so the client side doesn't hold it up.
	go func() {
		buf := make([]byte, 16)
		for {
			if _, err := client.Read(buf); err != nil {
				_ = client.Close()
				return
			}
		}
	}()
	res := SpliceObserved(a, b, 0, nil)
	if res.AFirst || res.BtoA != 3 {
		t.Fatalf("result = %+v, want server side first with 3 bytes back", res)
	}
}
