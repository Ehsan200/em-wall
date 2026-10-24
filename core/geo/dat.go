package geo

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// The .dat files are v2fly's routercommon protobuf, decoded by hand so the
// daemon needs no xray-core dependency:
//
//	GeoSiteList { repeated GeoSite entry = 1; }
//	GeoSite     { string country_code = 1; repeated Domain domain = 2; ... }
//	Domain      { Type type = 1; string value = 2; repeated Attribute attribute = 3; }
//	Attribute   { string key = 1; oneof { bool bool_value = 2; int64 int_value = 3; } }
//	GeoIPList   { repeated GeoIP entry = 1; }
//	GeoIP       { string country_code = 1; repeated CIDR cidr = 2; ... }
//	CIDR        { bytes ip = 1; uint32 prefix = 2; }
//
// A list is only scanned for its entries' names and byte spans at load;
// an entry's body is decoded when somebody asks for it, so a 10–20 MB file
// costs a few KB of resident memory until the reverse index is built.

// Geosite domain types.
const (
	DomainPlain  = 0 // keyword: substring match — not expressible as an em-wall pattern
	DomainRegex  = 1 // regexp — not expressible
	DomainDomain = 2 // apex + subdomains → "*.x"
	DomainFull   = 3 // exact name → "x"
)

// errMalformed wraps every decode failure.
var errMalformed = errors.New("geo: malformed .dat")

// span is one list entry's message bytes within the file.
type span struct {
	off, n int64
}

// datFile is a loaded .dat: the open file plus each entry's span, keyed
// by upper-case tag name. The file stays open for on-demand decoding;
// a reload opens the replacement and the old *os.File is closed by its
// finalizer once no snapshot references it.
type datFile struct {
	f     *os.File
	size  int64
	tags  map[string]span
	order []string // tag names in file order
}

// openDat opens path and scans its list. A missing file is (nil, nil).
func openDat(path string) (*datFile, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	buf := make([]byte, fi.Size())
	if _, err := io.ReadFull(f, buf); err != nil {
		f.Close()
		return nil, err
	}
	df, err := scanDat(buf)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	df.f, df.size = f, fi.Size()
	return df, nil
}

// scanDat walks a GeoSiteList / GeoIPList (same outer shape) and records
// each entry's span and name (field 1 of the entry).
func scanDat(buf []byte) (*datFile, error) {
	df := &datFile{tags: map[string]span{}}
	off := 0
	for off < len(buf) {
		num, typ, n := protowire.ConsumeTag(buf[off:])
		if n < 0 {
			return nil, fmt.Errorf("%w: list tag at %d", errMalformed, off)
		}
		off += n
		if num != 1 || typ != protowire.BytesType {
			m := protowire.ConsumeFieldValue(num, typ, buf[off:])
			if m < 0 {
				return nil, fmt.Errorf("%w: list field at %d", errMalformed, off)
			}
			off += m
			continue
		}
		msg, m := protowire.ConsumeBytes(buf[off:])
		if m < 0 {
			return nil, fmt.Errorf("%w: entry at %d", errMalformed, off)
		}
		start := off + (m - len(msg))
		off += m
		name, err := entryName(msg)
		if err != nil {
			return nil, err
		}
		name = strings.ToUpper(name)
		if name == "" {
			continue
		}
		if _, dup := df.tags[name]; !dup {
			df.order = append(df.order, name)
		}
		df.tags[name] = span{off: int64(start), n: int64(len(msg))}
	}
	return df, nil
}

// entryName reads field 1 (country_code) of a GeoSite/GeoIP message.
func entryName(msg []byte) (string, error) {
	var name string
	err := eachField(msg, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num == 1 && typ == protowire.BytesType {
			name = string(v)
		}
		return nil
	})
	return name, err
}

// read returns the bytes of one entry.
func (d *datFile) read(s span) ([]byte, error) {
	b := make([]byte, s.n)
	if _, err := d.f.ReadAt(b, s.off); err != nil {
		return nil, err
	}
	return b, nil
}

// readAll returns the whole file, for one-off passes like the index build.
func (d *datFile) readAll() ([]byte, error) {
	b := make([]byte, d.size)
	if _, err := d.f.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b, nil
}

// eachField calls fn for every field of msg. For BytesType v is the
// payload; for varints u is the value.
func eachField(msg []byte, fn func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error) error {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return errMalformed
		}
		msg = msg[n:]
		switch typ {
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(msg)
			if m < 0 {
				return errMalformed
			}
			if err := fn(num, typ, v, 0); err != nil {
				return err
			}
			msg = msg[m:]
		case protowire.VarintType:
			u, m := protowire.ConsumeVarint(msg)
			if m < 0 {
				return errMalformed
			}
			if err := fn(num, typ, nil, u); err != nil {
				return err
			}
			msg = msg[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, msg)
			if m < 0 {
				return errMalformed
			}
			msg = msg[m:]
		}
	}
	return nil
}

// Domain is one geosite entry.
type Domain struct {
	Type  int
	Value string
	// Attrs are the entry's attributes ("cn", "ads", …) — the @attr in
	// "geosite:google@cn". Kept for attribute filtering; not used yet.
	Attrs []string
}

// decodeSite decodes a GeoSite message's domains. fn is called per
// domain; the Domain is only valid during the call (Attrs reused).
func decodeSite(msg []byte, fn func(Domain)) error {
	return eachField(msg, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num != 2 || typ != protowire.BytesType {
			return nil
		}
		var d Domain
		err := eachField(v, func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error {
			switch {
			case num == 1 && typ == protowire.VarintType:
				d.Type = int(u)
			case num == 2 && typ == protowire.BytesType:
				d.Value = string(v)
			case num == 3 && typ == protowire.BytesType:
				return eachField(v, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
					if num == 1 && typ == protowire.BytesType {
						d.Attrs = append(d.Attrs, string(v))
					}
					return nil
				})
			}
			return nil
		})
		if err != nil {
			return err
		}
		fn(d)
		return nil
	})
}

// decodeIP decodes a GeoIP message's CIDRs. Malformed CIDRs (bad length
// or prefix) are skipped, not fatal: one odd row must not fail a file.
func decodeIP(msg []byte, fn func(netip.Prefix)) error {
	return eachField(msg, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num != 2 || typ != protowire.BytesType {
			return nil
		}
		var ip []byte
		var bits uint64
		err := eachField(v, func(num protowire.Number, typ protowire.Type, v []byte, u uint64) error {
			switch {
			case num == 1 && typ == protowire.BytesType:
				ip = v
			case num == 2 && typ == protowire.VarintType:
				bits = u
			}
			return nil
		})
		if err != nil {
			return err
		}
		addr, ok := netip.AddrFromSlice(ip)
		if !ok || bits > uint64(addr.BitLen()) {
			return nil
		}
		if p, err := addr.Prefix(int(bits)); err == nil {
			fn(p)
		}
		return nil
	})
}

// sitePattern translates a geosite domain to an em-wall pattern; ok is
// false for keyword/regexp entries.
func sitePattern(d Domain) (string, bool) {
	v := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d.Value)), ".")
	if v == "" {
		return "", false
	}
	switch d.Type {
	case DomainDomain:
		return "*." + v, true
	case DomainFull:
		return v, true
	}
	return "", false
}
