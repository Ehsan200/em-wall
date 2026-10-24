package geo

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// A minimal MaxMind DB writer — IPv4 tree, 24-bit records — enough to
// build fixture City/ASN files without a writer dependency. Values are
// map[string]any / string / float64 / uint32 / uint64 / []any.

func mmdbEncode(buf *bytes.Buffer, v any) {
	ctrl := func(typ, size int) {
		low, extra := size, -1
		if size >= 29 {
			if size >= 29+256 {
				panic("fixture value too large")
			}
			low, extra = 29, size-29
		}
		if typ <= 7 {
			buf.WriteByte(byte(typ<<5 | low))
		} else {
			buf.WriteByte(byte(low))
			buf.WriteByte(byte(typ - 7))
		}
		if extra >= 0 {
			buf.WriteByte(byte(extra))
		}
	}
	uintBytes := func(u uint64) []byte {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], u)
		i := 0
		for i < 8 && b[i] == 0 {
			i++
		}
		return b[i:]
	}
	switch x := v.(type) {
	case string:
		ctrl(2, len(x))
		buf.WriteString(x)
	case float64:
		ctrl(3, 8)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(x))
		buf.Write(b[:])
	case uint16:
		b := uintBytes(uint64(x))
		ctrl(5, len(b))
		buf.Write(b)
	case uint32:
		b := uintBytes(uint64(x))
		ctrl(6, len(b))
		buf.Write(b)
	case uint64:
		b := uintBytes(x)
		ctrl(9, len(b))
		buf.Write(b)
	case []any:
		ctrl(11, len(x))
		for _, e := range x {
			mmdbEncode(buf, e)
		}
	case map[string]any:
		ctrl(7, len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			mmdbEncode(buf, k)
			mmdbEncode(buf, x[k])
		}
	default:
		panic("unsupported fixture value")
	}
}

// writeMMDB writes an IPv4 mmdb mapping each prefix to its record.
func writeMMDB(t *testing.T, path, dbType string, recs map[string]map[string]any) {
	t.Helper()
	const empty = -1
	type node struct{ rec [2]int } // child node index, or -(2+dataIdx), or empty
	nodes := []node{{[2]int{empty, empty}}}
	var data bytes.Buffer
	var offsets []int
	keys := make([]string, 0, len(recs))
	for k := range recs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := netip.MustParsePrefix(k)
		offsets = append(offsets, data.Len())
		mmdbEncode(&data, recs[k])
		ip := binary.BigEndian.Uint32(p.Addr().AsSlice())
		cur := 0
		for bit := 0; bit < p.Bits(); bit++ {
			side := int(ip>>(31-bit)) & 1
			if bit == p.Bits()-1 {
				nodes[cur].rec[side] = -(2 + len(offsets) - 1)
				break
			}
			next := nodes[cur].rec[side]
			if next < 0 {
				nodes = append(nodes, node{[2]int{empty, empty}})
				next = len(nodes) - 1
				nodes[cur].rec[side] = next
			}
			cur = next
		}
	}
	n := len(nodes)
	var out bytes.Buffer
	put24 := func(v int) { out.Write([]byte{byte(v >> 16), byte(v >> 8), byte(v)}) }
	for _, nd := range nodes {
		for _, r := range nd.rec {
			switch {
			case r == empty:
				put24(n)
			case r >= 0:
				put24(r)
			default:
				put24(n + 16 + offsets[-r-2])
			}
		}
	}
	out.Write(make([]byte, 16))
	out.Write(data.Bytes())
	out.WriteString("\xAB\xCD\xEFMaxMind.com")
	mmdbEncode(&out, map[string]any{
		"binary_format_major_version": uint16(2),
		"binary_format_minor_version": uint16(0),
		"build_epoch":                 uint64(1700000000),
		"database_type":               dbType,
		"description":                 map[string]any{"en": "em-wall fixture"},
		"ip_version":                  uint16(4),
		"languages":                   []any{"en"},
		"node_count":                  uint32(n),
		"record_size":                 uint16(24),
	})
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cityRec(cc, country, city string, lat, lon float64) map[string]any {
	return map[string]any{
		"city":     map[string]any{"names": map[string]any{"en": city}},
		"country":  map[string]any{"iso_code": cc, "names": map[string]any{"en": country}},
		"location": map[string]any{"latitude": lat, "longitude": lon},
	}
}

func fixtureCity(t *testing.T, path string) {
	writeMMDB(t, path, "DBIP-City-Lite", map[string]map[string]any{
		"1.1.1.0/24": cityRec("AU", "Australia", "Sydney", -33.87, 151.21),
		"8.8.8.0/24": cityRec("US", "United States", "Mountain View", 37.39, -122.08),
		"5.10.0.0/16": map[string]any{ // no city, no coordinates
			"country": map[string]any{"iso_code": "NL", "names": map[string]any{"en": "Netherlands"}},
		},
	})
}

func fixtureASN(t *testing.T, path string) {
	writeMMDB(t, path, "DBIP-ASN-Lite", map[string]map[string]any{
		"1.1.1.0/24": {"autonomous_system_number": uint32(13335), "autonomous_system_organization": "Cloudflare, Inc."},
		"5.0.0.0/8":  {"autonomous_system_number": uint32(3320), "autonomous_system_organization": "Deutsche Telekom AG"},
	})
}

// gzipFile returns path's bytes gzipped (DB-IP ships .mmdb.gz).
func gzipFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(b)
	zw.Close()
	return out.Bytes()
}

func TestLocateCityASN(t *testing.T) {
	db := fixtureDB(t)
	dir := filepath.Dir(db.paths.GeoSite)
	db.paths.CityMMDB = filepath.Join(dir, "city.mmdb")
	db.paths.ASNMMDB = filepath.Join(dir, "asn.mmdb")
	fixtureCity(t, db.paths.CityMMDB)
	fixtureASN(t, db.paths.ASNMMDB)
	if err := db.Reload(); err != nil {
		t.Fatal(err)
	}

	loc, ok := db.Locate(netip.MustParseAddr("1.1.1.1"))
	want := Location{Country: "AU", CountryName: "Australia", City: "Sydney", Lat: -33.87, Lon: 151.21,
		Precision: PrecisionCity, ASN: 13335, Org: "Cloudflare, Inc.", Anycast: true}
	if !ok || loc != want {
		t.Errorf("1.1.1.1 → %+v %v\nwant %+v", loc, ok, want)
	}
	// City db answers without coordinates: country centroid, country precision.
	loc, _ = db.Locate(netip.MustParseAddr("5.10.1.1"))
	if loc.Country != "NL" || loc.Precision != PrecisionCountry || loc.Lat == 0 || loc.City != "" {
		t.Errorf("5.10.1.1 → %+v", loc)
	}
	// Not in the city db: geoip.dat country, still with the ASN.
	loc, _ = db.Locate(netip.MustParseAddr("5.1.1.1"))
	if loc.Country != "DE" || loc.Precision != PrecisionCountry || loc.ASN != 3320 || loc.Anycast {
		t.Errorf("5.1.1.1 → %+v", loc)
	}
	if n, err := CheckMMDB(db.paths.CityMMDB, "city"); err != nil || n == 0 {
		t.Errorf("CheckMMDB city: %d %v", n, err)
	}
	if _, err := CheckMMDB(db.paths.CityMMDB, "asn"); err == nil {
		t.Error("city file passed as asn")
	}
	if st := db.Stats(); !st.CityLoaded || !st.ASNLoaded || st.CityNodes == 0 {
		t.Errorf("stats %+v", st)
	}
}
