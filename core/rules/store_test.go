package rules

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_AddListGetDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	r1, err := s.Add(ctx, Rule{Pattern: "*.y.com", Action: ActionBlock, Enabled: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if r1.ID == 0 {
		t.Fatalf("expected non-zero ID")
	}

	r2, err := s.Add(ctx, Rule{Pattern: "*.public.y.com", Action: ActionRoute, Interface: "utun3", Enabled: true})
	if err != nil {
		t.Fatalf("Add 2: %v", err)
	}

	if _, err := s.Add(ctx, Rule{Pattern: "*.y.com", Action: ActionBlock, Enabled: true}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate add: got %v, want ErrDuplicate", err)
	}

	got, err := s.Get(ctx, r2.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pattern != "*.public.y.com" || got.Interface != "utun3" || got.Action != ActionRoute {
		t.Errorf("Get returned wrong row: %+v", got)
	}

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List len = %d, want 2", len(all))
	}

	if err := s.Delete(ctx, r1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, r1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: got %v, want ErrNotFound", err)
	}
}

func TestStore_BlockClearsInterface(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	r, err := s.Add(ctx, Rule{Pattern: "*.x.com", Action: ActionBlock, Interface: "utun3", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Interface != "" {
		t.Errorf("block rule should have empty interface, got %q", r.Interface)
	}
}

func TestStore_Update(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	r, err := s.Add(ctx, Rule{Pattern: "*.y.com", Action: ActionBlock, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	r.Action = ActionRoute
	r.Interface = "utun7"
	r.Enabled = false
	if err := s.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != ActionRoute || got.Interface != "utun7" || got.Enabled {
		t.Errorf("update not persisted: %+v", got)
	}
}

func TestStore_Settings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	v, err := s.GetSetting(ctx, "block_encrypted_dns", "false")
	if err != nil || v != "false" {
		t.Fatalf("default: %q %v", v, err)
	}
	if err := s.SetSetting(ctx, "block_encrypted_dns", "true"); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetSetting(ctx, "block_encrypted_dns", "false")
	if v != "true" {
		t.Errorf("expected true, got %q", v)
	}
}

func TestStore_Logs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, q := range []string{"a.com", "b.com", "c.com"} {
		if err := s.Log(ctx, LogEntry{QueryName: q, Action: "block"}); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := s.RecentLogs(ctx, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 3 {
		t.Errorf("got %d logs, want 3", len(logs))
	}
}

func TestStore_RenameInterfaceRef(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Mix of rules: single-name proxy/xray, multi-name, a non-matching
	// app: prefix (must be untouched), and a literal interface name
	// (must be untouched). Pattern uniqueness is the only constraint
	// the store enforces here, so each gets a distinct pattern.
	seed := []Rule{
		{Pattern: "a.example.com", Action: ActionRoute, Interface: "proxy:work", Enabled: true},
		{Pattern: "b.example.com", Action: ActionRoute, Interface: "proxy:work,home", Enabled: true},
		{Pattern: "c.example.com", Action: ActionRoute, Interface: "proxy:home,work", Enabled: true},
		{Pattern: "d.example.com", Action: ActionRoute, Interface: "proxy:home", Enabled: true},
		{Pattern: "e.example.com", Action: ActionRoute, Interface: "xray:work", Enabled: true},
		{Pattern: "f.example.com", Action: ActionRoute, Interface: "app:work", Enabled: true},
		{Pattern: "g.example.com", Action: ActionRoute, Interface: "utun3", Enabled: true},
	}
	for i, r := range seed {
		if _, err := s.Add(ctx, r); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	n, err := s.RenameInterfaceRef(ctx, "proxy:", "work", "office")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	// Exactly the three proxy:* rules containing "work" must be updated.
	if n != 3 {
		t.Errorf("rename returned %d, want 3", n)
	}

	want := map[string]string{
		"a.example.com": "proxy:office",
		"b.example.com": "proxy:office,home",
		"c.example.com": "proxy:home,office",
		"d.example.com": "proxy:home",
		"e.example.com": "xray:work", // xray prefix not touched
		"f.example.com": "app:work",  // app prefix not touched
		"g.example.com": "utun3",     // literal iface not touched
	}
	all, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all {
		if got := r.Interface; got != want[r.Pattern] {
			t.Errorf("rule %q: interface = %q, want %q", r.Pattern, got, want[r.Pattern])
		}
	}
}

func TestStore_RenameInterfaceRef_DedupeOnCollision(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// "proxy:a,b" with rename a→b must collapse to "proxy:b" (the
	// duplicate is dropped — not "proxy:b,b") so the multi-name
	// fallback list stays meaningful.
	if _, err := s.Add(ctx, Rule{
		Pattern: "x.example.com", Action: ActionRoute,
		Interface: "proxy:a,b", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := s.RenameInterfaceRef(ctx, "proxy:", "a", "b")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if n != 1 {
		t.Errorf("rename returned %d, want 1", n)
	}
	all, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := all[0].Interface; got != "proxy:b" {
		t.Errorf("interface = %q, want %q", got, "proxy:b")
	}
}

func TestStore_RenameInterfaceRef_Xray(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.Add(ctx, Rule{
		Pattern: "x.example.com", Action: ActionRoute,
		Interface: "xray:Riyahi,Backup", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Case-insensitive match (xray.normalizeName lowercases) — input
	// "Riyahi" should still resolve to a hit for oldName "riyahi".
	n, err := s.RenameInterfaceRef(ctx, "xray:", "riyahi", "riyahi-d20")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if n != 1 {
		t.Errorf("rename returned %d, want 1", n)
	}
	all, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := all[0].Interface; got != "xray:riyahi-d20,backup" {
		t.Errorf("interface = %q, want %q", got, "xray:riyahi-d20,backup")
	}
}

func TestStore_RenameInterfaceRef_NoOp(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.Add(ctx, Rule{
		Pattern: "x.example.com", Action: ActionRoute,
		Interface: "proxy:work", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ prefix, old, new string }{
		{"", "work", "office"},      // empty prefix
		{"proxy:", "", "office"},    // empty old
		{"proxy:", "work", ""},      // empty new
		{"proxy:", "work", "work"},  // identical
		{"proxy:", "absent", "new"}, // no rule references it
	}
	for _, c := range cases {
		n, err := s.RenameInterfaceRef(ctx, c.prefix, c.old, c.new)
		if err != nil {
			t.Errorf("prefix=%q old=%q new=%q: err = %v", c.prefix, c.old, c.new, err)
		}
		if n != 0 {
			t.Errorf("prefix=%q old=%q new=%q: n = %d, want 0", c.prefix, c.old, c.new, n)
		}
	}
}

func TestStore_LogsFilter(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, e := range []LogEntry{
		{QueryName: "a.com", Action: "block"},
		{QueryName: "b.com", Action: "route"},
		{QueryName: "c.com", Action: "block-app-down"},
		{QueryName: "d.com", Action: "block-iface-down"},
		{QueryName: "e.com", Action: "forward-failed"},
		{QueryName: "f.com", Action: "route"},
	} {
		if err := s.Log(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		filter string
		want   int
	}{
		{"", 6},
		{"route", 2},
		{"block", 1},
		{"unavailable", 3}, // 2x block-* + 1x forward-failed
		{"forward-failed", 1},
	}
	for _, tc := range cases {
		got, err := s.RecentLogs(ctx, 100, tc.filter)
		if err != nil {
			t.Fatalf("filter=%q: %v", tc.filter, err)
		}
		if len(got) != tc.want {
			names := make([]string, 0, len(got))
			for _, e := range got {
				names = append(names, e.QueryName+":"+e.Action)
			}
			t.Errorf("filter=%q: got %d, want %d (entries: %v)", tc.filter, len(got), tc.want, names)
		}
	}
}
