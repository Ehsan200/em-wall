package xray

import (
	"context"
	"errors"
	"testing"
)

func TestEditDialer(t *testing.T) {
	refs := func(s string) []DialerRef {
		r, err := ParseDialer(s)
		if err != nil {
			t.Fatalf("ParseDialer(%q): %v", s, err)
		}
		return r
	}
	cases := []struct {
		name, cur, mode, edit, want string
	}{
		{"replace", "xraysub:a", DialerEditReplace, "xray:b,proxy:c", "xray:b,proxy:c"},
		{"replace clears", "xraysub:a", DialerEditReplace, "", ""},
		{"add appends new only", "xraysub:a,xray:b", DialerEditAdd, "xray:b,proxy:c", "xraysub:a,xray:b,proxy:c"},
		{"add to empty", "", DialerEditAdd, "xraysub:a", "xraysub:a"},
		{"remove", "xraysub:a,xray:b,proxy:c", DialerEditRemove, "xray:b", "xraysub:a,proxy:c"},
		{"remove kind-sensitive", "xraysub:a", DialerEditRemove, "xray:a", "xraysub:a"},
		{"remove last", "xraysub:a", DialerEditRemove, "xraysub:a", ""},
	}
	for _, c := range cases {
		got, err := EditDialer(c.cur, c.mode, refs(c.edit))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if _, err := EditDialer("", "bogus", nil); !errors.Is(err, ErrInvalidDialer) {
		t.Errorf("unknown mode: err = %v, want ErrInvalidDialer", err)
	}
}

func TestSetDialersAtomic(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, err := s.Add(ctx, Config{Name: "a", Outbound: `{"protocol":"freedom"}`, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Add(ctx, Config{Name: "b", Outbound: `{"protocol":"freedom"}`, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDialers(ctx, map[int64]string{a.ID: "xray:b", b.ID: "proxy:p"}); err != nil {
		t.Fatalf("SetDialers: %v", err)
	}
	ga, _ := s.Get(ctx, a.ID)
	gb, _ := s.Get(ctx, b.ID)
	if ga.Dialer != "xray:b" || gb.Dialer != "proxy:p" {
		t.Fatalf("dialers = %q, %q", ga.Dialer, gb.Dialer)
	}
	// A missing ID rolls the whole batch back.
	err = s.SetDialers(ctx, map[int64]string{a.ID: "", 9999: ""})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: err = %v, want ErrNotFound", err)
	}
	if ga, _ = s.Get(ctx, a.ID); ga.Dialer != "xray:b" {
		t.Errorf("batch not rolled back: a.Dialer = %q", ga.Dialer)
	}
}
