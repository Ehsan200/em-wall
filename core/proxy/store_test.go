package proxy

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

	p1, err := s.Add(ctx, Proxy{
		Name: "work", Protocol: ProtocolSOCKS5,
		Host: "proxy.example.com", Port: 1080,
		Username: "alice", Password: "s3cret",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if p1.ID == 0 {
		t.Fatalf("expected non-zero ID")
	}

	p2, err := s.Add(ctx, Proxy{
		Name: "home", Protocol: ProtocolHTTP,
		Host: "127.0.0.1", Port: 8080,
	})
	if err != nil {
		t.Fatalf("Add 2: %v", err)
	}

	// Duplicate name (case-insensitive, since the store normalizes).
	if _, err := s.Add(ctx, Proxy{
		Name: "WORK", Protocol: ProtocolSOCKS5, Host: "x", Port: 1,
	}); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate add: got %v, want ErrDuplicate", err)
	}

	got, err := s.Get(ctx, p2.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "home" || got.Protocol != ProtocolHTTP || got.Port != 8080 {
		t.Errorf("Get returned wrong row: %+v", got)
	}

	byName, err := s.GetByName(ctx, "WORK")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if byName.ID != p1.ID {
		t.Errorf("GetByName: got id %d, want %d", byName.ID, p1.ID)
	}

	all, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("List len = %d, want 2", len(all))
	}

	if err := s.Delete(ctx, p1.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, p1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: got %v, want ErrNotFound", err)
	}
}

func TestStore_Validation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	cases := []struct {
		name string
		in   Proxy
		want error
	}{
		{"empty name", Proxy{Protocol: ProtocolSOCKS5, Host: "x", Port: 1}, ErrInvalidName},
		{"bad chars", Proxy{Name: "a b", Protocol: ProtocolSOCKS5, Host: "x", Port: 1}, ErrInvalidName},
		{"bad protocol", Proxy{Name: "n", Protocol: "ftp", Host: "x", Port: 1}, ErrInvalidProtocol},
		{"empty host", Proxy{Name: "n", Protocol: ProtocolSOCKS5, Host: "", Port: 1}, ErrInvalidHost},
		{"bad port low", Proxy{Name: "n", Protocol: ProtocolSOCKS5, Host: "x", Port: 0}, ErrInvalidPort},
		{"bad port high", Proxy{Name: "n", Protocol: ProtocolSOCKS5, Host: "x", Port: 70000}, ErrInvalidPort},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Add(ctx, c.in); !errors.Is(err, c.want) {
				t.Errorf("Add: got %v, want %v", err, c.want)
			}
		})
	}
}

func TestStore_UpdatePreservesPassword(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	p, err := s.Add(ctx, Proxy{
		Name: "work", Protocol: ProtocolSOCKS5,
		Host: "h", Port: 1080, Username: "a", Password: "original",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Update with empty Password → original kept.
	p.Host = "newhost"
	p.Password = ""
	if err := s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, p.ID)
	if got.Host != "newhost" || got.Password != "original" {
		t.Errorf("expected host=newhost password=original, got %+v", got)
	}

	// Update with new Password → replaced.
	p.Password = "rotated"
	if err := s.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, p.ID)
	if got.Password != "rotated" {
		t.Errorf("expected password=rotated, got %q", got.Password)
	}
}

func TestStore_NamesExist(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, name := range []string{"work", "home"} {
		if _, err := s.Add(ctx, Proxy{
			Name: name, Protocol: ProtocolSOCKS5, Host: "h", Port: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"work"}, nil},
		{[]string{"home", "work"}, nil},
		{[]string{"missing"}, []string{"missing"}},
		{[]string{"work", "missing", "home"}, []string{"missing"}},
		{[]string{"WORK", "Home"}, nil},     // normalized
		{[]string{"work", "WORK"}, nil},     // duplicates collapsed
		{nil, nil},
	}
	for _, c := range cases {
		got, err := s.NamesExist(ctx, c.in)
		if err != nil {
			t.Fatalf("NamesExist(%v): %v", c.in, err)
		}
		if len(got) != len(c.want) {
			t.Errorf("NamesExist(%v) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("NamesExist(%v)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
