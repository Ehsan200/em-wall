package xray

import (
	"context"
	"errors"
	"testing"
)

func TestParseSetName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"xrayset:iproute", "iproute"},
		{"xrayset:  IPRoute  ", "iproute"},
		{"xray:a,b", ""},
		{"proxy:p1", ""},
		{"utun4", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := ParseSetName(c.in); got != c.want {
			t.Errorf("ParseSetName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseSetMembers(t *testing.T) {
	refs, err := ParseSetMembers(" xray:HK-01 , proxy:office , xray:hk-01 ")
	if err != nil {
		t.Fatalf("ParseSetMembers: %v", err)
	}
	// duplicate xray:hk-01 collapses, first position kept
	want := []DialerRef{
		{Kind: DialerKindXray, Name: "hk-01"},
		{Kind: DialerKindProxy, Name: "office"},
	}
	if len(refs) != len(want) {
		t.Fatalf("got %d refs (%v), want %d", len(refs), refs, len(want))
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("ref[%d] = %+v, want %+v", i, refs[i], want[i])
		}
	}
}

func TestParseSetMembersRejects(t *testing.T) {
	if _, err := ParseSetMembers("xraysub:mysub"); !errors.Is(err, ErrInvalidSetMember) {
		t.Errorf("xraysub member err = %v, want ErrInvalidSetMember", err)
	}
	if _, err := ParseSetMembers("hk-01"); !errors.Is(err, ErrInvalidSetMember) {
		t.Errorf("untyped member err = %v, want ErrInvalidSetMember", err)
	}
	if _, err := ParseSetMembers("  "); !errors.Is(err, ErrEmptySet) {
		t.Errorf("empty members err = %v, want ErrEmptySet", err)
	}
}

func TestExpandMembers(t *testing.T) {
	allXray, err := ParseSetMembers("xray:a,xray:b,xray:c")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := ExpandMembers(allXray), "xray:a,b,c"; got != want {
		t.Errorf("all-xray expansion = %q, want %q", got, want)
	}

	mixed, err := ParseSetMembers("xray:a,proxy:office,xray:b")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := ExpandMembers(mixed), "proxy:_xray_a,office,_xray_b"; got != want {
		t.Errorf("mixed expansion = %q, want %q", got, want)
	}

	if got := ExpandMembers(nil); got != "" {
		t.Errorf("empty expansion = %q, want \"\"", got)
	}
}

// A rule bound to a set must survive expansion unchanged when it isn't a
// set reference, and must yield "" (fail closed) when the set is gone.
func TestExpandSetInterface(t *testing.T) {
	exp := map[string]string{"iproute": "xray:a,b"}
	if got := ExpandSetInterface("xrayset:iproute", exp); got != "xray:a,b" {
		t.Errorf("known set = %q", got)
	}
	if got := ExpandSetInterface("xrayset:gone", exp); got != "xrayset:gone" {
		t.Errorf("unknown set = %q, want the raw ref back (fails closed downstream)", got)
	}
	if got := ExpandSetInterface("xray:x,y", exp); got != "xray:x,y" {
		t.Errorf("passthrough = %q", got)
	}
	if got := ExpandSetInterface("utun4", exp); got != "utun4" {
		t.Errorf("passthrough iface = %q", got)
	}
}

func TestSetCRUDAndExpansions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	set, err := s.AddSet(ctx, Set{Name: "IPRoute", Members: "xray:a, xray:b", Enabled: true})
	if err != nil {
		t.Fatalf("AddSet: %v", err)
	}
	if set.Name != "iproute" || set.Members != "xray:a,xray:b" {
		t.Errorf("stored set = %+v, want normalized name + canonical members", set)
	}

	if _, err := s.AddSet(ctx, Set{Name: "iproute", Members: "xray:c", Enabled: true}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate name err = %v, want ErrDuplicate", err)
	}

	exp, err := s.SetExpansions(ctx)
	if err != nil {
		t.Fatalf("SetExpansions: %v", err)
	}
	if exp["iproute"] != "xray:a,b" {
		t.Errorf("expansion = %q, want %q", exp["iproute"], "xray:a,b")
	}

	// Disabled sets are omitted so their rules fail closed.
	set.Enabled = false
	if err := s.UpdateSet(ctx, set); err != nil {
		t.Fatalf("UpdateSet: %v", err)
	}
	exp, err = s.SetExpansions(ctx)
	if err != nil {
		t.Fatalf("SetExpansions: %v", err)
	}
	if _, ok := exp["iproute"]; ok {
		t.Error("disabled set present in expansions, want omitted")
	}

	if err := s.DeleteSet(ctx, set.ID); err != nil {
		t.Fatalf("DeleteSet: %v", err)
	}
	if _, err := s.GetSet(ctx, set.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSet after delete = %v, want ErrNotFound", err)
	}
}

func TestSetsReferencingAndRename(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.AddSet(ctx, Set{Name: "one", Members: "xray:old,proxy:office", Enabled: true}); err != nil {
		t.Fatalf("AddSet one: %v", err)
	}
	if _, err := s.AddSet(ctx, Set{Name: "two", Members: "proxy:office", Enabled: true}); err != nil {
		t.Fatalf("AddSet two: %v", err)
	}

	refs, err := s.SetsReferencing(ctx, DialerKindXray, "old")
	if err != nil {
		t.Fatalf("SetsReferencing: %v", err)
	}
	if len(refs) != 1 || refs[0] != "one" {
		t.Errorf("SetsReferencing(xray,old) = %v, want [one]", refs)
	}

	n, err := s.RenameSetMember(ctx, DialerKindXray, "old", "new")
	if err != nil {
		t.Fatalf("RenameSetMember: %v", err)
	}
	if n != 1 {
		t.Errorf("renamed %d rows, want 1", n)
	}
	got, err := s.GetSetByName(ctx, "one")
	if err != nil {
		t.Fatalf("GetSetByName: %v", err)
	}
	if got.Members != "xray:new,proxy:office" {
		t.Errorf("members after rename = %q", got.Members)
	}
}

// Renaming onto a name the set already contains must not leave a
// duplicate member behind.
func TestRenameSetMemberDedupes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.AddSet(ctx, Set{Name: "dup", Members: "xray:a,xray:b", Enabled: true}); err != nil {
		t.Fatalf("AddSet: %v", err)
	}
	if _, err := s.RenameSetMember(ctx, DialerKindXray, "b", "a"); err != nil {
		t.Fatalf("RenameSetMember: %v", err)
	}
	got, err := s.GetSetByName(ctx, "dup")
	if err != nil {
		t.Fatalf("GetSetByName: %v", err)
	}
	if got.Members != "xray:a" {
		t.Errorf("members = %q, want %q", got.Members, "xray:a")
	}
}

// A reference written with different casing than the stored set row must
// still resolve — the engine matches the expansion map by exact string.
func TestCanonicalizeInterface(t *testing.T) {
	cases := []struct{ in, want string }{
		{"xrayset:IPRoute", "xrayset:iproute"},
		{"xrayset:  iproute ", "xrayset:iproute"},
		{"xrayset:iproute", "xrayset:iproute"},
		{"xray:A,B", "xray:A,B"},
		{"utun4", "utun4"},
		{"", ""},
	}
	for _, c := range cases {
		if got := CanonicalizeInterface(c.in); got != c.want {
			t.Errorf("CanonicalizeInterface(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
