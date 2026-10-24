package geo

import (
	"net/netip"
	"sort"

	"google.golang.org/protobuf/encoding/protowire"
)

// EncodeGeoSite builds a geosite.dat from tag → domains. Used by tests
// (here and in the daemon's xray integration test) to make small fixture
// files; the shipped files come from upstream releases. Tags are written
// in sorted order so output is deterministic.
func EncodeGeoSite(tags map[string][]Domain) []byte {
	var out []byte
	for _, name := range sortedKeys(tags) {
		var msg []byte
		msg = protowire.AppendTag(msg, 1, protowire.BytesType)
		msg = protowire.AppendString(msg, name)
		for _, d := range tags[name] {
			var dm []byte
			if d.Type != 0 {
				dm = protowire.AppendTag(dm, 1, protowire.VarintType)
				dm = protowire.AppendVarint(dm, uint64(d.Type))
			}
			dm = protowire.AppendTag(dm, 2, protowire.BytesType)
			dm = protowire.AppendString(dm, d.Value)
			for _, a := range d.Attrs {
				var am []byte
				am = protowire.AppendTag(am, 1, protowire.BytesType)
				am = protowire.AppendString(am, a)
				am = protowire.AppendTag(am, 2, protowire.VarintType)
				am = protowire.AppendVarint(am, 1)
				dm = protowire.AppendTag(dm, 3, protowire.BytesType)
				dm = protowire.AppendBytes(dm, am)
			}
			msg = protowire.AppendTag(msg, 2, protowire.BytesType)
			msg = protowire.AppendBytes(msg, dm)
		}
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, msg)
	}
	return out
}

// EncodeGeoIP builds a geoip.dat from tag → CIDRs ("1.2.3.0/24"); bad
// CIDRs panic — fixtures only.
func EncodeGeoIP(tags map[string][]string) []byte {
	var out []byte
	for _, name := range sortedKeys(tags) {
		var msg []byte
		msg = protowire.AppendTag(msg, 1, protowire.BytesType)
		msg = protowire.AppendString(msg, name)
		for _, c := range tags[name] {
			p := netip.MustParsePrefix(c)
			var cm []byte
			cm = protowire.AppendTag(cm, 1, protowire.BytesType)
			cm = protowire.AppendBytes(cm, p.Addr().AsSlice())
			cm = protowire.AppendTag(cm, 2, protowire.VarintType)
			cm = protowire.AppendVarint(cm, uint64(p.Bits()))
			msg = protowire.AppendTag(msg, 2, protowire.BytesType)
			msg = protowire.AppendBytes(msg, cm)
		}
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, msg)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
