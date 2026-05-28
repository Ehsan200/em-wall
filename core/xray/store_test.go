package xray

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAddAllocatesUniqueSocksPort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c1, err := s.Add(ctx, Config{
		Name:     "first",
		Outbound: `{"protocol":"freedom"}`,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if c1.SocksPort != PortStart {
		t.Errorf("first SocksPort = %d, want PortStart (%d)", c1.SocksPort, PortStart)
	}

	c2, err := s.Add(ctx, Config{
		Name:     "second",
		Outbound: `{"protocol":"freedom"}`,
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("Add second: %v", err)
	}
	if c2.SocksPort == c1.SocksPort {
		t.Errorf("second SocksPort = %d, collides with first", c2.SocksPort)
	}
}

func TestAddRejectsBadOutbound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.Add(ctx, Config{
		Name:     "x",
		Outbound: `not-json`,
		Enabled:  true,
	})
	if !errors.Is(err, ErrInvalidOutbound) {
		t.Errorf("Add with bad JSON: err=%v, want ErrInvalidOutbound", err)
	}

	_, err = s.Add(ctx, Config{
		Name:     "y",
		Outbound: `{"settings":{}}`, // no protocol
		Enabled:  true,
	})
	if !errors.Is(err, ErrInvalidOutbound) {
		t.Errorf("Add without protocol field: err=%v, want ErrInvalidOutbound", err)
	}
}

func TestUpdatePreservesSocksPort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.Add(ctx, Config{
		Name: "keep-port", Outbound: `{"protocol":"freedom"}`, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	originalPort := c.SocksPort

	// Caller may pass SocksPort=0 in the update payload — Update should
	// not zero out the row's stored port, since the field is read-only
	// after Add.
	c.SocksPort = 0
	c.Outbound = `{"protocol":"freedom","settings":{"domainStrategy":"AsIs"}}`
	if err := s.Update(ctx, c); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := s.Get(ctx, c.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SocksPort != originalPort {
		t.Errorf("SocksPort drifted: got %d, want %d", got.SocksPort, originalPort)
	}
}

func TestNamesExist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, n := range []string{"have-one", "have-two"} {
		if _, err := s.Add(ctx, Config{
			Name: n, Outbound: `{"protocol":"freedom"}`, Enabled: true,
		}); err != nil {
			t.Fatalf("Add %s: %v", n, err)
		}
	}

	missing, err := s.NamesExist(ctx, []string{"have-one", "missing", "have-two", "MISSING-TOO"})
	if err != nil {
		t.Fatalf("NamesExist: %v", err)
	}
	if len(missing) != 2 || missing[0] != "missing" || missing[1] != "missing-too" {
		t.Errorf("missing = %v, want [missing missing-too]", missing)
	}
}
