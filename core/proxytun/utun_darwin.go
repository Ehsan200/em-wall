// Package proxytun owns a daemon-managed macOS utun interface plus a
// user-space TCP/IP stack (gvisor netstack) that terminates connections
// arriving at the utun. The DNS layer can route proxy-bound destination
// IPs to this interface (via `route -host <ip> -interface utunN`), and
// every TCP connection on those IPs ends up here for the daemon to
// forward through an upstream HTTP/SOCKS5 proxy.
//
// Why a utun + netstack instead of pf rdr: a kernel-level redirect
// (pf rdr) is fragile when a VPN owns the default route — locally
// generated outbound packets sometimes never hit pf's hooks, depending
// on how the VPN claims the route. A daemon-owned utun bypasses that
// entirely: per-host routes are more specific than the VPN's default,
// so the kernel hands the packet to us. We read raw IP packets from
// the utun fd, terminate the TCP locally via netstack, and forward.
package proxytun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS kernel-control constants. SYSPROTO_CONTROL isn't exported by
// golang.org/x/sys/unix but is documented as 2 in
// <sys/kern_control.h>.
const (
	sysprotoControl = 2
	utunControlName = "com.apple.net.utun_control"
)

// UTUN owns an open utun device. Read/Write deal in raw IP packets:
// the BSD 4-byte AF prefix is stripped on Read and added on Write, so
// callers see pure IPv4/IPv6 datagrams.
type UTUN struct {
	f    *os.File
	name string // "utunN"
}

type ctlInfo struct {
	ID   uint32
	Name [96]byte
}

// sockaddrCtl matches struct sockaddr_ctl from <sys/kern_control.h>:
//
//	struct sockaddr_ctl {
//	    u_char  sc_len;
//	    u_char  sc_family;
//	    u_int16_t ss_sysaddr;
//	    u_int32_t sc_id;
//	    u_int32_t sc_unit;
//	    u_int32_t sc_reserved[5];
//	};
type sockaddrCtl struct {
	Len      uint8
	Family   uint8
	SysAddr  uint16
	ID       uint32
	Unit     uint32
	Reserved [5]uint32
}

// Open creates a new utun (kernel picks the unit) and brings it up
// with the given /32 point-to-point address and MTU. Caller owns the
// returned *UTUN and must Close it on shutdown.
//
// addr must be a valid IPv4 dotted-quad. We give the utun a unique
// address from a private range so it doesn't collide with anything
// the user's other VPNs might use. The address itself isn't routed —
// only the per-host routes the daemon installs later are.
func Open(addr string, mtu int) (*UTUN, error) {
	if runtime.GOOS != "darwin" {
		return nil, errors.New("proxytun: only darwin is supported")
	}
	if mtu <= 0 {
		mtu = 1500
	}

	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, sysprotoControl)
	if err != nil {
		return nil, fmt.Errorf("utun: socket(AF_SYSTEM): %w", err)
	}
	closeFD := func() { _ = unix.Close(fd) }

	var ci ctlInfo
	copy(ci.Name[:], utunControlName)
	if _, _, errno := unix.Syscall(
		unix.SYS_IOCTL, uintptr(fd), uintptr(unix.CTLIOCGINFO),
		uintptr(unsafe.Pointer(&ci)),
	); errno != 0 {
		closeFD()
		return nil, fmt.Errorf("utun: CTLIOCGINFO: %w", errno)
	}

	sa := sockaddrCtl{
		Len:     32, // sizeof(struct sockaddr_ctl)
		Family:  unix.AF_SYSTEM,
		SysAddr: unix.AF_SYS_CONTROL,
		ID:      ci.ID,
		Unit:    0, // 0 = kernel picks
	}
	if _, _, errno := unix.Syscall(
		unix.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&sa)), uintptr(sa.Len),
	); errno != 0 {
		closeFD()
		return nil, fmt.Errorf("utun: connect: %w", errno)
	}

	// Recover the assigned utun name (e.g. "utun17") via getsockopt with
	// UTUN_OPT_IFNAME=2 (level=sysprotoControl).
	const utunOptIfName = 2
	var nameBuf [16]byte
	nameLen := uint32(len(nameBuf))
	if _, _, errno := unix.Syscall6(
		unix.SYS_GETSOCKOPT, uintptr(fd),
		uintptr(sysprotoControl), uintptr(utunOptIfName),
		uintptr(unsafe.Pointer(&nameBuf[0])),
		uintptr(unsafe.Pointer(&nameLen)),
		0,
	); errno != 0 {
		closeFD()
		return nil, fmt.Errorf("utun: getsockopt(IFNAME): %w", errno)
	}
	name := string(nameBuf[:nameLen])
	// Strip trailing NUL.
	for i, c := range name {
		if c == 0 {
			name = name[:i]
			break
		}
	}

	if err := configureUTUN(name, addr, mtu); err != nil {
		closeFD()
		return nil, fmt.Errorf("utun: configure %s: %w", name, err)
	}

	return &UTUN{
		f:    os.NewFile(uintptr(fd), name),
		name: name,
	}, nil
}

// configureUTUN brings the interface up via ifconfig. Avoiding raw
// SIOCSIFADDR / SIOCSIFFLAGS keeps this file free of struct ifreq +
// in_aliasreq layout code, which is annoying to maintain correctly.
// ifconfig is in /sbin on macOS and present in every install.
func configureUTUN(name, addr string, mtu int) error {
	// `ifconfig utunN inet <addr> <addr> mtu <m> up` — point-to-point
	// with self as both source and dest. We don't actually use this IP
	// for anything; the daemon receives traffic via per-host routes
	// pointed at the interface, not via the address.
	cmd := exec.Command("/sbin/ifconfig", name, "inet", addr, addr,
		"mtu", fmt.Sprintf("%d", mtu), "up")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ifconfig: %w (%s)", err, string(out))
	}
	return nil
}

// Name returns the kernel-assigned interface name (utunN).
func (u *UTUN) Name() string { return u.name }

// Close shuts down the interface and releases the fd.
func (u *UTUN) Close() error {
	if u == nil || u.f == nil {
		return nil
	}
	return u.f.Close()
}

// Read pulls one IP packet from the utun. The 4-byte BSD AF prefix
// is stripped: the returned slice starts with the IPv4/IPv6 header.
// The caller-supplied buf must have room for the prefix; on a short
// buf the prefix bytes are still consumed.
func (u *UTUN) Read(buf []byte) (int, error) {
	tmp := make([]byte, len(buf)+4)
	n, err := u.f.Read(tmp)
	if err != nil {
		return 0, err
	}
	if n < 4 {
		return 0, fmt.Errorf("utun: short read (%d bytes)", n)
	}
	payload := tmp[4:n]
	copy(buf, payload)
	return n - 4, nil
}

// Write sends one IP packet to the utun. The first byte of pkt
// determines AF (4 = IPv4, 6 = IPv6) and we prepend the BSD prefix.
func (u *UTUN) Write(pkt []byte) (int, error) {
	if len(pkt) < 1 {
		return 0, nil
	}
	var af uint32
	switch pkt[0] >> 4 {
	case 4:
		af = unix.AF_INET
	case 6:
		af = unix.AF_INET6
	default:
		return 0, fmt.Errorf("utun: unknown IP version 0x%x", pkt[0]>>4)
	}
	framed := make([]byte, 4+len(pkt))
	// macOS expects host byte order for the family.
	framed[0] = byte(af >> 24)
	framed[1] = byte(af >> 16)
	framed[2] = byte(af >> 8)
	framed[3] = byte(af)
	copy(framed[4:], pkt)
	n, err := u.f.Write(framed)
	if err != nil {
		return 0, err
	}
	if n < 4 {
		return 0, nil
	}
	return n - 4, nil
}
