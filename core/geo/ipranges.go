package geo

import (
	"encoding/binary"
	"net/netip"
	"sort"
)

// countryRanges is geoip.dat's country tags flattened into sorted address
// ranges, for Locate when no city mmdb is present. Built lazily: with the
// mmdb installed it is never needed. IPv6 is kept at /64 granularity
// (upper 64 bits), which is far finer than any country allocation.
type countryRanges struct {
	v4    []range4
	v6    []range6
	codes []string // index → ISO code
}

type range4 struct {
	lo, hi uint32
	cc     uint16
}

type range6 struct {
	lo, hi uint64
	cc     uint16
}

// buildCountryRanges reads every two-letter tag the centroid table knows.
// Non-country tags (PRIVATE, CLOUDFLARE, TELEGRAM, …) are skipped.
func buildCountryRanges(df *datFile) (*countryRanges, error) {
	buf, err := df.readAll()
	if err != nil {
		return nil, err
	}
	cr := &countryRanges{}
	for _, name := range df.order {
		if len(name) != 2 {
			continue
		}
		if _, ok := centroids()[name]; !ok {
			continue
		}
		cc := uint16(len(cr.codes))
		cr.codes = append(cr.codes, name)
		s := df.tags[name]
		err := decodeIP(buf[s.off:s.off+s.n], func(p netip.Prefix) {
			p = p.Masked()
			a := p.Addr()
			if a.Is4() {
				lo := binary.BigEndian.Uint32(a.AsSlice())
				hi := lo | (^uint32(0) >> uint(p.Bits()))
				cr.v4 = append(cr.v4, range4{lo, hi, cc})
				return
			}
			b := a.As16()
			lo := binary.BigEndian.Uint64(b[:8])
			bits := p.Bits()
			if bits > 64 {
				bits = 64
			}
			hi := lo | (^uint64(0) >> uint(bits))
			cr.v6 = append(cr.v6, range6{lo, hi, cc})
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(cr.v4, func(i, j int) bool { return cr.v4[i].lo < cr.v4[j].lo })
	sort.Slice(cr.v6, func(i, j int) bool { return cr.v6[i].lo < cr.v6[j].lo })
	cr.v4 = merge4(cr.v4)
	cr.v6 = merge6(cr.v6)
	return cr, nil
}

// merge4 joins adjacent or overlapping same-country ranges (sorted by lo).
// geoip.dat lists a country as hundreds of thousands of CIDRs, most of
// them contiguous; merged, the table is a fraction of the size.
func merge4(rs []range4) []range4 {
	out := rs[:0]
	for _, r := range rs {
		if n := len(out); n > 0 && out[n-1].cc == r.cc && (out[n-1].hi == ^uint32(0) || r.lo <= out[n-1].hi+1) {
			if r.hi > out[n-1].hi {
				out[n-1].hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return append([]range4(nil), out...)
}

func merge6(rs []range6) []range6 {
	out := rs[:0]
	for _, r := range rs {
		if n := len(out); n > 0 && out[n-1].cc == r.cc && (out[n-1].hi == ^uint64(0) || r.lo <= out[n-1].hi+1) {
			if r.hi > out[n-1].hi {
				out[n-1].hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return append([]range6(nil), out...)
}

// nestedLookback is how many earlier ranges a lookup checks when the
// nearest one doesn't contain the address — enough for the rare nested
// allocation, without scanning.
const nestedLookback = 8

// country returns the ISO code holding ip, "" if none.
func (cr *countryRanges) country(ip netip.Addr) string {
	ip = ip.Unmap()
	if ip.Is4() {
		v := binary.BigEndian.Uint32(ip.AsSlice())
		i := sort.Search(len(cr.v4), func(i int) bool { return cr.v4[i].lo > v }) - 1
		for k := 0; i >= 0 && k < nestedLookback; i, k = i-1, k+1 {
			if cr.v4[i].hi >= v {
				return cr.codes[cr.v4[i].cc]
			}
		}
		return ""
	}
	b := ip.As16()
	v := binary.BigEndian.Uint64(b[:8])
	i := sort.Search(len(cr.v6), func(i int) bool { return cr.v6[i].lo > v }) - 1
	for k := 0; i >= 0 && k < nestedLookback; i, k = i-1, k+1 {
		if cr.v6[i].hi >= v {
			return cr.codes[cr.v6[i].cc]
		}
	}
	return ""
}
