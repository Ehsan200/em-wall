package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/xray"
)

func TestUniqueXrayNameDedupesDerivedNames(t *testing.T) {
	d := newSetTestDeps(t)
	ctx := context.Background()

	// Provider node names routinely collapse to the same slug once the
	// emoji and separators are stripped, so every promotion after the
	// first has to find its own name rather than failing on the unique
	// index.
	got, err := d.uniqueXrayName(ctx, "", "🇩🇪 DE-Frankfurt")
	if err != nil {
		t.Fatalf("uniqueXrayName: %v", err)
	}
	if got != "de-frankfurt" {
		t.Fatalf("first derived name = %q, want de-frankfurt", got)
	}
	addXrayEntry(t, d, got)

	got, err = d.uniqueXrayName(ctx, "", "🇩🇪 DE-Frankfurt")
	if err != nil {
		t.Fatalf("uniqueXrayName: %v", err)
	}
	if got != "de-frankfurt-2" {
		t.Fatalf("second derived name = %q, want de-frankfurt-2", got)
	}
	addXrayEntry(t, d, got)

	got, _ = d.uniqueXrayName(ctx, "", "de frankfurt")
	if got != "de-frankfurt-3" {
		t.Fatalf("third derived name = %q, want de-frankfurt-3", got)
	}
}

func TestUniqueXrayNameKeepsSuffixWithinLimit(t *testing.T) {
	d := newSetTestDeps(t)
	ctx := context.Background()

	long := strings.Repeat("a", 64)
	addXrayEntry(t, d, long)

	got, err := d.uniqueXrayName(ctx, "", long)
	if err != nil {
		t.Fatalf("uniqueXrayName: %v", err)
	}
	if len(got) > 64 {
		t.Fatalf("name is %d chars, over the 64 limit: %q", len(got), got)
	}
	if !xray.ValidName(got) {
		t.Fatalf("%q is not a ValidName, so Add would reject it", got)
	}
}

func TestUniqueXrayNameHonoursExplicitName(t *testing.T) {
	d := newSetTestDeps(t)
	ctx := context.Background()

	got, err := d.uniqueXrayName(ctx, "my-node", "🇩🇪 DE-Frankfurt")
	if err != nil {
		t.Fatalf("uniqueXrayName: %v", err)
	}
	if got != "my-node" {
		t.Fatalf("explicit name = %q, want my-node", got)
	}
	// An explicit name that can't be an entry name is rejected rather than
	// silently rewritten — the user chose it, so they should be told.
	if _, err := d.uniqueXrayName(ctx, "bad name!", "x"); err == nil {
		t.Fatal("invalid explicit name was accepted")
	}
}

func TestXrayToDTOWithOrigin(t *testing.T) {
	subNames := map[int64]string{7: "prov"}
	live := map[string]bool{xray.NodeKey(7, "alive"): true}

	// Hand-written entry: no provenance at all.
	dto := xrayToDTOWithOrigin(xray.Config{Name: "manual"}, subNames, live)
	if dto.SubName != "" || dto.SourceGone {
		t.Errorf("hand-written entry got provenance: %+v", dto)
	}

	// Promoted, source node still in the pool.
	dto = xrayToDTOWithOrigin(xray.Config{Name: "a", SubID: 7, SubFingerprint: "alive"}, subNames, live)
	if dto.SubName != "prov" || dto.SourceGone {
		t.Errorf("live promoted entry = %+v, want subName=prov sourceGone=false", dto)
	}

	// Promoted, node dropped or rotated out of the pool.
	dto = xrayToDTOWithOrigin(xray.Config{Name: "b", SubID: 7, SubFingerprint: "dead"}, subNames, live)
	if dto.SubName != "prov" || !dto.SourceGone {
		t.Errorf("stale promoted entry = %+v, want subName=prov sourceGone=true", dto)
	}

	// Subscription itself deleted: the entry reads as hand-written rather
	// than as a problem, since the pool is gone by the user's own action.
	dto = xrayToDTOWithOrigin(xray.Config{Name: "c", SubID: 99, SubFingerprint: "dead"}, subNames, live)
	if dto.SubName != "" || dto.SourceGone {
		t.Errorf("orphaned entry = %+v, want no provenance", dto)
	}
}

func TestXrayDTOIsStillProducedWithoutOrigins(t *testing.T) {
	d := newSetTestDeps(t)
	// Provenance is a label, not the payload: a lookup failure must still
	// yield a usable DTO for the entry that was just stored.
	var dto ipc.XrayDTO = d.xrayDTO(context.Background(), xray.Config{ID: 1, Name: "x", SocksPort: 10801})
	if dto.Name != "x" || dto.SocksPort != 10801 {
		t.Fatalf("degraded DTO lost its core fields: %+v", dto)
	}
}
