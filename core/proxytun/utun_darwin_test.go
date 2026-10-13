package proxytun

import (
	"bytes"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// socketpairUTUN stands a datagram socketpair in for the utun fd: same
// one-read-one-packet framing, no root needed.
func socketpairUTUN(t *testing.T) (*UTUN, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	u := &UTUN{f: os.NewFile(uintptr(fds[0]), "tun"), name: "utun-test"}
	peer := os.NewFile(uintptr(fds[1]), "peer")
	t.Cleanup(func() { u.Close(); peer.Close() })
	return u, peer
}

func TestFramedIOMatchesPlain(t *testing.T) {
	u, peer := socketpairUTUN(t)
	v4 := []byte{0x45, 0, 0, 20, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	v6 := append([]byte{0x60}, bytes.Repeat([]byte{7}, 39)...)

	for _, pkt := range [][]byte{v4, v6} {
		// WriteFramed must put the same bytes on the wire as Write.
		if _, err := u.Write(pkt); err != nil {
			t.Fatal(err)
		}
		want := make([]byte, 2048)
		n, _ := peer.Read(want)
		framed := append(make([]byte, 4), pkt...)
		if err := u.WriteFramed(framed); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 2048)
		m, _ := peer.Read(got)
		if !bytes.Equal(got[:m], want[:n]) {
			t.Fatalf("WriteFramed wire = %x, Write wire = %x", got[:m], want[:n])
		}

		// ReadFramed strips the prefix in place.
		if _, err := peer.Write(want[:n]); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 2048)
		ip, err := u.ReadFramed(buf)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ip, pkt) {
			t.Fatalf("ReadFramed = %x, want %x", ip, pkt)
		}
	}
	if err := u.WriteFramed([]byte{0, 0, 0, 0, 0x10}); err == nil {
		t.Fatal("unknown IP version accepted")
	}
}
