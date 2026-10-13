package proxytun

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/ehsan/em-wall/core/proxy"
)

// benchPair is a Tunnel on one end of a datagram socketpair and a second
// gvisor stack — standing in for the kernel's TCP — on the other. Each
// Tunnel connection is spliced to a loopback server, the way proxytun
// splices into xray's SOCKS inbound. No root, no utun.
type benchPair struct {
	client *stack.Stack
	stop   func()
}

func newBenchPair(tb testing.TB, mtu int, upstream string) *benchPair {
	tb.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		tb.Fatal(err)
	}
	// macOS caps a local datagram at its send buffer (2 KB by default).
	for _, fd := range fds {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<20)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	}
	u := &UTUN{f: os.NewFile(uintptr(fds[0]), "tun"), name: "utun-bench"}
	peer := os.NewFile(uintptr(fds[1]), "peer")

	handler := func(c net.Conn, _, _ *net.TCPAddr) {
		up, err := net.Dial("tcp", upstream)
		if err != nil {
			c.Close()
			return
		}
		proxy.Splice(c, up)
		c.Close()
		up.Close()
	}
	tun, err := NewTunnel(u, mtu, handler, nil, log.New(io.Discard, "", 0))
	if err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go tun.Start(ctx)

	// Client stack: the "kernel" side of the utun.
	cs := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	cep := channel.New(1024, uint32(mtu), "")
	if e := cs.CreateNIC(1, cep); e != nil {
		tb.Fatal(e)
	}
	cs.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4([4]byte{10, 0, 0, 2}).WithPrefix(),
	}, stack.AddressProperties{})
	cs.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	go func() { // client stack → "utun"
		buf := make([]byte, 4+mtu+128)
		for {
			pkt := cep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			n := 4
			for _, sl := range pkt.AsSlices() {
				n += copy(buf[n:], sl)
			}
			pkt.DecRef()
			buf[0], buf[1], buf[2], buf[3] = 0, 0, 0, unix.AF_INET
			if _, err := peer.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	go func() { // "utun" → client stack
		buf := make([]byte, 4+mtu+128)
		for {
			n, err := peer.Read(buf)
			if err != nil {
				return
			}
			if n <= 4 {
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(buf[4:n])})
			cep.InjectInbound(ipv4.ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()

	return &benchPair{client: cs, stop: func() {
		cancel()
		tun.Stop()
		peer.Close()
		cep.Close()
		cs.Close()
	}}
}

// sinkSource serves loopback TCP: every accepted conn is sent size bytes
// then closed (download direction, the one bulk traffic uses).
func sinkSource(tb testing.TB, size int) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { ln.Close() })
	chunk := make([]byte, 64<<10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				for left := size; left > 0; {
					n := min(left, len(chunk))
					if _, err := c.Write(chunk[:n]); err != nil {
						return
					}
					left -= n
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// BenchmarkTunnelDownload measures bulk download throughput through the
// netstack + splice path at two MTUs. ns/op is per 8 MB transfer.
func BenchmarkTunnelDownload(b *testing.B) {
	const size = 8 << 20
	for _, mtu := range []int{1500, 9000} {
		b.Run(fmt.Sprintf("mtu%d", mtu), func(b *testing.B) {
			p := newBenchPair(b, mtu, sinkSource(b, size))
			defer p.stop()
			dst := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{198, 18, 0, 9}), Port: 443}
			buf := make([]byte, 64<<10)
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c, err := gonet.DialTCP(p.client, dst, ipv4.ProtocolNumber)
				if err != nil {
					b.Fatal(err)
				}
				got := 0
				for {
					n, err := c.Read(buf)
					got += n
					if err != nil {
						break
					}
				}
				c.Close()
				if got != size {
					b.Fatalf("read %d bytes, want %d", got, size)
				}
			}
		})
	}
}
