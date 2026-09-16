package xray

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSuggestNameStripsToEntryCharset(t *testing.T) {
	cases := []struct{ in, want string }{
		{"🇩🇪 DE-Frankfurt-01", "de-frankfurt-01"},
		{"DE-Frankfurt-01 | 2x", "de-frankfurt-01-2x"},
		{"香港 01", "01"},
		{"  Trim  Me  ", "trim-me"},
		{"UPPER_case", "upper_case"},
		{"---dashes---", "dashes"},
		{"🇯🇵🇯🇵🇯🇵", "node"}, // nothing survives; must still be a valid name
		{"", "node"},
	}
	for _, c := range cases {
		got := SuggestName(c.in)
		if got != c.want {
			t.Errorf("SuggestName(%q) = %q, want %q", c.in, got, c.want)
		}
		if !ValidName(got) {
			t.Errorf("SuggestName(%q) = %q, which is not a ValidName", c.in, got)
		}
	}
}

func TestSuggestNameTruncatesToValidLength(t *testing.T) {
	got := SuggestName(strings.Repeat("a", 200))
	if len(got) != 64 {
		t.Errorf("length = %d, want 64", len(got))
	}
	if !ValidName(got) {
		t.Errorf("%q is not a ValidName", got)
	}
	// A cut landing on junk must not leave a trailing dash, which would
	// still be valid but reads as a truncation artifact.
	long := strings.Repeat("ab ", 40) // 60 chars of "ab-" once collapsed
	if s := SuggestName(long); strings.HasSuffix(s, "-") {
		t.Errorf("SuggestName(%q) = %q, want no trailing dash", long, s)
	}
}

func TestGetNodeAndImportedNames(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub, err := s.AddSub(ctx, Subscription{Name: "prov", URL: "https://example.com/sub"})
	if err != nil {
		t.Fatalf("AddSub: %v", err)
	}
	ob := `{"protocol":"freedom"}`
	fp := Fingerprint(ob)
	if err := s.ReplaceNodes(ctx, sub.ID, []SubNode{
		{SubID: sub.ID, Name: "DE-01", Fingerprint: fp, Outbound: ob},
	}); err != nil {
		t.Fatalf("ReplaceNodes: %v", err)
	}

	node, err := s.GetNode(ctx, sub.ID, fp)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if node.Name != "DE-01" {
		t.Errorf("node name = %q, want DE-01", node.Name)
	}
	if _, err := s.GetNode(ctx, sub.ID, "nosuchfingerprint"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetNode for an absent fingerprint = %v, want ErrNotFound", err)
	}

	// Nothing promoted yet.
	imported, err := s.ImportedNames(ctx, sub.ID)
	if err != nil {
		t.Fatalf("ImportedNames: %v", err)
	}
	if len(imported) != 0 {
		t.Fatalf("ImportedNames = %v, want empty", imported)
	}

	if _, err := s.Add(ctx, Config{
		Name: "de-01", Outbound: ob, Enabled: true,
		SubID: sub.ID, SubFingerprint: fp,
	}); err != nil {
		t.Fatalf("Add promoted entry: %v", err)
	}
	imported, err = s.ImportedNames(ctx, sub.ID)
	if err != nil {
		t.Fatalf("ImportedNames: %v", err)
	}
	if imported[fp] != "de-01" {
		t.Errorf("ImportedNames[%q] = %q, want de-01", fp, imported[fp])
	}

	// A hand-written entry has no origin and must not appear.
	if _, err := s.Add(ctx, Config{Name: "manual", Outbound: ob, Enabled: true}); err != nil {
		t.Fatalf("Add manual entry: %v", err)
	}
	imported, _ = s.ImportedNames(ctx, sub.ID)
	if len(imported) != 1 {
		t.Errorf("ImportedNames = %v, want only the promoted entry", imported)
	}
}

func TestPromotedEntrySurvivesPoolReplacement(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub, _ := s.AddSub(ctx, Subscription{Name: "prov", URL: "https://example.com/sub"})
	ob := `{"protocol":"freedom"}`
	fp := Fingerprint(ob)
	_ = s.ReplaceNodes(ctx, sub.ID, []SubNode{{SubID: sub.ID, Name: "DE-01", Fingerprint: fp, Outbound: ob}})

	added, err := s.Add(ctx, Config{
		Name: "de-01", Outbound: ob, Enabled: true,
		SubID: sub.ID, SubFingerprint: fp,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The provider drops that node. The whole point of copying rather than
	// referencing: the entry, and any rule bound to it, must be untouched.
	otherOb := `{"protocol":"blackhole"}`
	if err := s.ReplaceNodes(ctx, sub.ID, []SubNode{
		{SubID: sub.ID, Name: "NL-02", Fingerprint: Fingerprint(otherOb), Outbound: otherOb},
	}); err != nil {
		t.Fatalf("ReplaceNodes: %v", err)
	}

	got, err := s.Get(ctx, added.ID)
	if err != nil {
		t.Fatalf("entry vanished with its source node: %v", err)
	}
	if got.SubID != sub.ID || got.SubFingerprint != fp {
		t.Errorf("origin lost: SubID=%d SubFingerprint=%q", got.SubID, got.SubFingerprint)
	}

	// ...but it is now detectably stale.
	live, err := s.LiveNodeKeys(ctx)
	if err != nil {
		t.Fatalf("LiveNodeKeys: %v", err)
	}
	if live[NodeKey(sub.ID, fp)] {
		t.Error("dropped node still reported live")
	}
	if !live[NodeKey(sub.ID, Fingerprint(otherOb))] {
		t.Error("current node not reported live")
	}
}

func TestUpdateKeepsOrigin(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	added, err := s.Add(ctx, Config{
		Name: "de-01", Outbound: `{"protocol":"freedom"}`, Enabled: true,
		SubID: 7, SubFingerprint: "abc123",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Renaming or editing a promoted entry must not erase where it came
	// from — Update writes an explicit field list for exactly this reason.
	if err := s.Update(ctx, Config{
		ID: added.ID, Name: "de-01-renamed", Outbound: `{"protocol":"blackhole"}`, Enabled: false,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(ctx, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubID != 7 || got.SubFingerprint != "abc123" {
		t.Errorf("origin cleared by Update: SubID=%d SubFingerprint=%q", got.SubID, got.SubFingerprint)
	}
}
