package main

import (
	"net"
	"os"
	"sync"
	"time"
)

// geoipCache is a process-wide cache of the parsed geoip.dat. Re-parsed
// only when the file's mtime changes (e.g. after an xray upgrade).
var geoipCache struct {
	sync.Mutex
	path    string
	modTime time.Time
	fn      func(net.IP) string
}

// lookupCountry returns the ISO-3166-1 alpha-2 country code for ip using
// the v2ray/xray geoip.dat at path, or "" if not found or unreadable.
func lookupCountry(path string, ip net.IP) string {
	if path == "" || ip == nil {
		return ""
	}
	geoipCache.Lock()
	defer geoipCache.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if geoipCache.fn == nil || geoipCache.path != path || !geoipCache.modTime.Equal(info.ModTime()) {
		fn, err := parseGeoIPDat(path)
		if err != nil {
			return ""
		}
		geoipCache.path = path
		geoipCache.modTime = info.ModTime()
		geoipCache.fn = fn
	}
	return geoipCache.fn(ip)
}

type geoEntry struct {
	net     *net.IPNet
	country string
}

// parseGeoIPDat reads a v2ray/xray geoip.dat (proto-encoded GeoIPList)
// and returns a function that maps a net.IP to its country code.
func parseGeoIPDat(path string) (func(net.IP) string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var entries []geoEntry
	buf := data
	for len(buf) > 0 {
		// GeoIPList: field 1 (GeoIP), wire type 2 (length-delimited)
		tag, n := protoVarint(buf)
		if n < 0 {
			break
		}
		buf = buf[n:]
		if tag>>3 != 1 || tag&7 != 2 {
			var ok bool
			buf, ok = protoSkip(buf, tag&7)
			if !ok {
				break
			}
			continue
		}
		msgLen, n := protoVarint(buf)
		if n < 0 || uint64(len(buf)-n) < msgLen {
			break
		}
		geoData := buf[n : n+int(msgLen)]
		buf = buf[n+int(msgLen):]

		cc, cidrs, reverse := parseGeoIP(geoData)
		// Only keep standard ISO 3166-1 alpha-2 codes (exactly 2 ASCII letters).
		// geoip.dat also ships special entries like "private", "cloudflare",
		// "netflix", etc. that are not country codes and would corrupt lookups.
		if reverse || len(cc) != 2 {
			continue
		}
		for _, e := range cidrs {
			entries = append(entries, geoEntry{net: e, country: cc})
		}
	}

	return func(ip net.IP) string {
		for _, e := range entries {
			if e.net.Contains(ip) {
				return e.country
			}
		}
		return ""
	}, nil
}

// parseGeoIP parses a single GeoIP proto message and returns its country
// code, list of parsed CIDR networks, and whether reverse_match is set.
func parseGeoIP(b []byte) (cc string, cidrs []*net.IPNet, reverse bool) {
	for len(b) > 0 {
		tag, n := protoVarint(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch tag {
		case 0x0a: // field 1 (country_code), wire 2
			l, n := protoVarint(b)
			if n < 0 || uint64(len(b)-n) < l {
				return
			}
			cc = string(b[n : n+int(l)])
			b = b[n+int(l):]
		case 0x12: // field 2 (cidr), wire 2
			l, n := protoVarint(b)
			if n < 0 || uint64(len(b)-n) < l {
				return
			}
			cidrData := b[n : n+int(l)]
			b = b[n+int(l):]
			if ipNet := parseCIDR(cidrData); ipNet != nil {
				cidrs = append(cidrs, ipNet)
			}
		case 0x18: // field 3 (reverse_match), wire 0
			v, n := protoVarint(b)
			if n < 0 {
				return
			}
			reverse = v != 0
			b = b[n:]
		default:
			var ok bool
			b, ok = protoSkip(b, tag&7)
			if !ok {
				return
			}
		}
	}
	return
}

// parseCIDR parses a single CIDR proto message (ip bytes + prefix varint)
// into a net.IPNet, or returns nil on error.
func parseCIDR(b []byte) *net.IPNet {
	var ipBytes []byte
	var prefix uint32
	for len(b) > 0 {
		tag, n := protoVarint(b)
		if n < 0 {
			return nil
		}
		b = b[n:]
		switch tag {
		case 0x0a: // field 1 (ip), wire 2
			l, n := protoVarint(b)
			if n < 0 || uint64(len(b)-n) < l {
				return nil
			}
			ipBytes = make([]byte, l)
			copy(ipBytes, b[n:n+int(l)])
			b = b[n+int(l):]
		case 0x10: // field 2 (prefix), wire 0
			v, n := protoVarint(b)
			if n < 0 {
				return nil
			}
			prefix = uint32(v)
			b = b[n:]
		default:
			var ok bool
			b, ok = protoSkip(b, tag&7)
			if !ok {
				return nil
			}
		}
	}
	if len(ipBytes) != 4 && len(ipBytes) != 16 {
		return nil
	}
	bits := len(ipBytes) * 8
	mask := net.CIDRMask(int(prefix), bits)
	ip := net.IP(ipBytes)
	return &net.IPNet{IP: ip.Mask(mask), Mask: mask}
}

// protoVarint reads a protobuf varint from b.
// Returns (value, bytesConsumed) or (0, -1) on error.
func protoVarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i == 10 {
			return 0, -1
		}
		if c < 0x80 {
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, -1
}

// protoSkip skips one proto field of the given wire type in b.
func protoSkip(b []byte, wireType uint64) ([]byte, bool) {
	switch wireType {
	case 0: // varint
		_, n := protoVarint(b)
		if n < 0 {
			return nil, false
		}
		return b[n:], true
	case 1: // 64-bit
		if len(b) < 8 {
			return nil, false
		}
		return b[8:], true
	case 2: // length-delimited
		l, n := protoVarint(b)
		if n < 0 || uint64(len(b)-n) < l {
			return nil, false
		}
		return b[n+int(l):], true
	case 5: // 32-bit
		if len(b) < 4 {
			return nil, false
		}
		return b[4:], true
	default:
		return nil, false
	}
}
