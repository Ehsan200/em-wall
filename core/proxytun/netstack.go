package proxytun

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// ConnHandler is invoked once per accepted TCP connection. local is
// the destination the client wanted to reach (the IP/port written in
// the IP header — preserved end-to-end by netstack). remote is the
// originating client's IP/port on this machine.
//
// The handler is run in its own goroutine. Closing conn ends the
// connection; not closing it leaks the netstack endpoint.
type ConnHandler func(conn net.Conn, local, remote *net.TCPAddr)

// PacketHandler is invoked once per new UDP flow (a fresh src/dst
// tuple) arriving on the utun. conn is a connected netstack UDP socket:
// Read returns datagrams the client sent to local, Write sends
// datagrams back to the client. local is the destination the client
// targeted; remote is the client's source. Run in its own goroutine.
//
// UDP has no connection teardown, so the handler is responsible for
// closing conn on an idle timeout — otherwise the flow's endpoint
// leaks. nil PacketHandler disables UDP (datagrams are dropped).
type PacketHandler func(conn net.Conn, local, remote *net.UDPAddr)

// Tunnel binds a UTUN to a gvisor user-space TCP stack. Every TCP
// connection arriving on the utun is accepted by netstack and handed
// to ConnHandler. The handler's job is to look up which proxy to
// forward through and splice bytes both ways.
type Tunnel struct {
	tun           *UTUN
	stk           *stack.Stack
	ep            *channel.Endpoint
	handler       ConnHandler
	packetHandler PacketHandler
	logger        *log.Logger

	mtu      uint32
	stopOnce sync.Once
	stopped  chan struct{}
}

const (
	nicID = 1
	// channel.Endpoint capacity. 256 packets in flight before we
	// backpressure the utun read loop; chosen so a brief stall in
	// handler dispatch doesn't drop kernel packets.
	channelSize = 256
)

// NewTunnel attaches a gvisor stack to tun. The stack is configured
// in spoofing+promiscuous mode so it accepts packets addressed to any
// IP — which is exactly what arrives on a utun that the kernel routes
// arbitrary destination IPs through.
//
// tcpHandler is required. udpHandler may be nil, in which case UDP
// flows are dropped (the stack replies ICMP port-unreachable).
func NewTunnel(tun *UTUN, mtu int, tcpHandler ConnHandler, udpHandler PacketHandler, logger *log.Logger) (*Tunnel, error) {
	if tun == nil {
		return nil, fmt.Errorf("proxytun: nil tun")
	}
	if tcpHandler == nil {
		return nil, fmt.Errorf("proxytun: nil tcp handler")
	}
	if logger == nil {
		logger = log.Default()
	}
	if mtu <= 0 {
		mtu = 1500
	}

	stk := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        false,
	})

	linkAddr, _ := tcpip.ParseMACAddress("00:00:00:00:00:01")
	ep := channel.New(channelSize, uint32(mtu), linkAddr)

	if tcpErr := stk.CreateNIC(nicID, ep); tcpErr != nil {
		return nil, fmt.Errorf("proxytun: CreateNIC: %v", tcpErr)
	}
	// Spoofing: send packets with arbitrary source addresses (we may
	// be terminating connections for IPs we don't own).
	// Promiscuous: accept packets destined to any IP, not just ours.
	if tcpErr := stk.SetSpoofing(nicID, true); tcpErr != nil {
		return nil, fmt.Errorf("proxytun: SetSpoofing: %v", tcpErr)
	}
	if tcpErr := stk.SetPromiscuousMode(nicID, true); tcpErr != nil {
		return nil, fmt.Errorf("proxytun: SetPromiscuousMode: %v", tcpErr)
	}

	// Default route: send any packet for 0.0.0.0/0 (and ::/0) out our NIC.
	stk.SetRouteTable([]tcpip.Route{
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
		{Destination: header.IPv6EmptySubnet, NIC: nicID},
	})

	t := &Tunnel{
		tun:           tun,
		stk:           stk,
		ep:            ep,
		handler:       tcpHandler,
		packetHandler: udpHandler,
		logger:        logger,
		mtu:           uint32(mtu),
		stopped:       make(chan struct{}),
	}

	// Register the TCP forwarder. wq=16384 advertised window;
	// callback receives every incoming SYN.
	fwd := tcp.NewForwarder(stk, 0, channelSize, t.acceptTCP)
	stk.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)

	// Register the UDP forwarder when a handler is provided. It fires
	// once per new src/dst flow; we materialize a connected endpoint
	// and hand it off.
	if udpHandler != nil {
		ufwd := udp.NewForwarder(stk, t.acceptUDP)
		stk.SetTransportProtocolHandler(udp.ProtocolNumber, ufwd.HandlePacket)
	}

	return t, nil
}

// Start begins shuttling packets between the utun fd and the stack.
// Runs until ctx is cancelled or the utun fd is closed. Returns when
// both shuttle goroutines have exited.
func (t *Tunnel) Start(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.tunReadLoop(ctx) }()
	go func() { defer wg.Done(); t.tunWriteLoop(ctx) }()
	wg.Wait()
	close(t.stopped)
}

// Stop closes the underlying utun and waits for Start to return.
// Safe to call multiple times.
func (t *Tunnel) Stop() {
	t.stopOnce.Do(func() {
		_ = t.tun.Close()
		t.ep.Close()
	})
}

// IfaceName is the kernel-assigned name of the underlying utun.
func (t *Tunnel) IfaceName() string { return t.tun.Name() }

// tunReadLoop reads IP packets from the utun and injects them into
// the stack. One read = one packet (utun is packet-mode). On read
// error we exit — the most common reason is Stop closing the fd.
func (t *Tunnel) tunReadLoop(ctx context.Context) {
	buf := make([]byte, t.mtu+128)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := t.tun.Read(buf)
		if err != nil {
			t.logger.Printf("proxytun: utun read: %v", err)
			return
		}
		if n == 0 {
			continue
		}
		// Determine network protocol from the IP version nibble.
		var netProto tcpip.NetworkProtocolNumber
		switch buf[0] >> 4 {
		case 4:
			netProto = header.IPv4ProtocolNumber
		case 6:
			netProto = header.IPv6ProtocolNumber
		default:
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...)),
		})
		t.ep.InjectInbound(netProto, pkt)
		pkt.DecRef()
	}
}

// tunWriteLoop drains outbound packets from the stack and writes them
// to the utun. Blocks on ReadContext so we exit cleanly when ctx is
// cancelled.
func (t *Tunnel) tunWriteLoop(ctx context.Context) {
	for {
		pkt := t.ep.ReadContext(ctx)
		if pkt == nil {
			// ReadContext returns nil on ctx cancellation or after
			// ep.Close() — same exit semantics either way.
			return
		}
		// A packet is split across multiple buffer slices; flatten.
		view := pkt.ToView()
		data := view.AsSlice()
		if _, err := t.tun.Write(data); err != nil {
			t.logger.Printf("proxytun: utun write: %v", err)
			view.Release()
			pkt.DecRef()
			return
		}
		view.Release()
		pkt.DecRef()
	}
}

// acceptTCP is called by netstack for every inbound SYN. We build a
// net.Conn out of the request and hand it to the user-supplied
// handler. Note: failure to Complete the request means the SYN is
// dropped silently — RFC-compliant for cases where we can't service
// the connection (e.g. unknown destination, no proxy configured).
func (t *Tunnel) acceptTCP(req *tcp.ForwarderRequest) {
	id := req.ID()
	local := &net.TCPAddr{
		IP:   net.IP(id.LocalAddress.AsSlice()),
		Port: int(id.LocalPort),
	}
	remote := &net.TCPAddr{
		IP:   net.IP(id.RemoteAddress.AsSlice()),
		Port: int(id.RemotePort),
	}

	var wq waiter.Queue
	endpoint, tcpErr := req.CreateEndpoint(&wq)
	if tcpErr != nil {
		t.logger.Printf("proxytun: CreateEndpoint %s -> %s: %v", remote, local, tcpErr)
		req.Complete(true) // send RST
		return
	}
	req.Complete(false) // accepted, no reset

	conn := gonet.NewTCPConn(&wq, endpoint)
	go t.handler(conn, local, remote)
}

// acceptUDP is called by netstack's UDP forwarder for each new flow.
// We create a connected endpoint (bound to the destination, connected
// to the client source) and hand a net.Conn to the packet handler.
// Returning true marks the flow handled so the stack doesn't emit ICMP
// port-unreachable. CreateEndpoint already queues the first datagram,
// so no payload is lost.
func (t *Tunnel) acceptUDP(req *udp.ForwarderRequest) bool {
	id := req.ID()
	local := &net.UDPAddr{
		IP:   net.IP(id.LocalAddress.AsSlice()),
		Port: int(id.LocalPort),
	}
	remote := &net.UDPAddr{
		IP:   net.IP(id.RemoteAddress.AsSlice()),
		Port: int(id.RemotePort),
	}

	var wq waiter.Queue
	endpoint, tcpErr := req.CreateEndpoint(&wq)
	if tcpErr != nil {
		t.logger.Printf("proxytun: udp CreateEndpoint %s -> %s: %v", remote, local, tcpErr)
		return true
	}
	conn := gonet.NewUDPConn(&wq, endpoint)
	go t.packetHandler(conn, local, remote)
	return true
}
