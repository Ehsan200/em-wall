package geo

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// siteIndex is the reverse index name → tags, over the domain/full
// entries of every non-Broad tag.
//
// It holds no strings: each entry is a 64-bit FNV-1a hash of (kind,
// value) plus a tag number, sorted by hash — 10 bytes an entry, where a
// map[string][]uint16 costs ~60. Loyalsoldier's geosite has a few hundred
// thousand such entries, so this is the difference between ~4 MB and
// ~25 MB resident. A hash collision could at worst attach one wrong tag
// to one name; at 2^64 that is not a practical concern.
type siteIndex struct {
	hashes []uint64
	tags   []uint16
	names  []string // tag number → name
	sizes  []int    // tag number → indexed entries, for "most specific" ordering
}

const (
	kindDomain = 'd'
	kindFull   = 'f'
)

func keyHash(kind byte, v string) uint64 {
	h := fnv.New64a()
	h.Write([]byte{kind})
	h.Write([]byte(v))
	return h.Sum64()
}

// buildSiteIndex decodes every non-Broad tag of df once.
func buildSiteIndex(df *datFile) (*siteIndex, error) {
	buf, err := df.readAll()
	if err != nil {
		return nil, err
	}
	type pair struct {
		h uint64
		t uint16
	}
	var pairs []pair
	idx := &siteIndex{}
	for _, name := range df.order {
		if Broad[name] {
			continue
		}
		if len(idx.names) >= 1<<16 {
			return nil, fmt.Errorf("geo: too many geosite tags")
		}
		t := uint16(len(idx.names))
		s := df.tags[name]
		n := 0
		err := decodeSite(buf[s.off:s.off+s.n], func(d Domain) {
			v := strings.TrimSuffix(strings.ToLower(d.Value), ".")
			switch d.Type {
			case DomainDomain:
				pairs = append(pairs, pair{keyHash(kindDomain, v), t})
			case DomainFull:
				pairs = append(pairs, pair{keyHash(kindFull, v), t})
			default:
				return
			}
			n++
		})
		if err != nil {
			return nil, fmt.Errorf("geo: tag %s: %w", name, err)
		}
		idx.names = append(idx.names, name)
		idx.sizes = append(idx.sizes, n)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].h != pairs[j].h {
			return pairs[i].h < pairs[j].h
		}
		return pairs[i].t < pairs[j].t
	})
	idx.hashes = make([]uint64, 0, len(pairs))
	idx.tags = make([]uint16, 0, len(pairs))
	for i, p := range pairs {
		if i > 0 && p == pairs[i-1] {
			continue // a tag listing the same entry twice
		}
		idx.hashes = append(idx.hashes, p.h)
		idx.tags = append(idx.tags, p.t)
	}
	return idx, nil
}

// lookup appends the tags indexed under h.
func (x *siteIndex) lookup(h uint64, into []uint16) []uint16 {
	i := sort.Search(len(x.hashes), func(i int) bool { return x.hashes[i] >= h })
	for ; i < len(x.hashes) && x.hashes[i] == h; i++ {
		into = append(into, x.tags[i])
	}
	return into
}

// tagsFor walks name from the full name to its shortest parent: an exact
// (full) or domain entry at the full name, then domain entries at each
// parent. A deeper match is more specific; among tags matching at the
// same depth the smaller tag (fewer entries) comes first — "SPOTIFY"
// before a category tag that also lists spotify.com.
func (x *siteIndex) tagsFor(name string) []string {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" {
		return nil
	}
	var out []string
	seen := map[uint16]bool{}
	var level []uint16
	emit := func() {
		sort.SliceStable(level, func(i, j int) bool {
			a, b := level[i], level[j]
			if x.sizes[a] != x.sizes[b] {
				return x.sizes[a] < x.sizes[b]
			}
			return x.names[a] < x.names[b]
		})
		for _, t := range level {
			if !seen[t] {
				seen[t] = true
				out = append(out, x.names[t])
			}
		}
		level = level[:0]
	}
	level = x.lookup(keyHash(kindFull, name), level)
	for s := name; ; {
		level = x.lookup(keyHash(kindDomain, s), level)
		emit()
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			break
		}
		s = s[dot+1:]
	}
	return out
}

// matchesFor is tagsFor with how each tag matched: every (tag, entry)
// pair listing name, deepest first. A tag listing name under both a full
// and a domain entry appears twice; callers pick.
func (x *siteIndex) matchesFor(name string) []TagMatch {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if name == "" {
		return nil
	}
	var out []TagMatch
	var level []uint16
	for _, t := range x.lookup(keyHash(kindFull, name), level) {
		out = append(out, TagMatch{Tag: x.names[t], Full: true, Suffix: len(name), Size: x.sizes[t]})
	}
	for s := name; ; {
		level = x.lookup(keyHash(kindDomain, s), level[:0])
		for _, t := range level {
			out = append(out, TagMatch{Tag: x.names[t], Suffix: len(s), Size: x.sizes[t]})
		}
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			break
		}
		s = s[dot+1:]
	}
	return out
}

// memBytes is the index's approximate heap size.
func (x *siteIndex) memBytes() int {
	return len(x.hashes)*8 + len(x.tags)*2 + len(x.names)*24 + len(x.sizes)*8
}
