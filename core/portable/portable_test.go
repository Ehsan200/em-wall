package portable

import (
	"errors"
	"testing"
)

func sampleBundle() Bundle {
	return Bundle{
		Schema:     BundleSchema,
		ExportedAt: "2026-06-30T00:00:00Z",
		AppVersion: "test",
		Rules: []BundleRule{
			{Pattern: "*.example.com", Action: "block", Enabled: true},
			{Pattern: "api.example.com", Action: "route", Interface: "xray:work", Enabled: true},
		},
		CustomGroups: []BundleGroup{
			{Key: "custom:mine", DisplayName: "Mine", Patterns: []string{"*.x.com"}, Color: "#fff"},
		},
		Subscriptions: []BundleSubscription{
			{Name: "mysub", URL: "https://example.com/sub", UserAgent: "v2rayN/6.23", IntervalSec: 43200, NodeCap: 30, Enabled: true},
		},
		Masters: []BundleXray{
			{Name: "master", Outbound: `{"protocol":"vless"}`, Enabled: true, Dialer: "xraysub:mysub"},
		},
	}
}

func TestSealOpen_SubsAndMasters(t *testing.T) {
	sealed, err := Seal(sampleBundle(), "pw")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(sealed, "pw")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got.Subscriptions) != 1 || got.Subscriptions[0].URL != "https://example.com/sub" {
		t.Errorf("subscriptions round-trip mismatch: %+v", got.Subscriptions)
	}
	if len(got.Masters) != 1 || got.Masters[0].Dialer != "xraysub:mysub" {
		t.Errorf("masters round-trip mismatch: %+v", got.Masters)
	}
}

// An older bundle (no subs/masters keys) must still open cleanly with the
// new fields absent.
func TestOpen_BackwardCompatNoSubs(t *testing.T) {
	b := Bundle{Schema: BundleSchema, Rules: []BundleRule{{Pattern: "a.com", Action: "block", Enabled: true}}}
	sealed, _ := Seal(b, "pw")
	got, err := Open(sealed, "pw")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got.Subscriptions) != 0 || len(got.Masters) != 0 {
		t.Errorf("expected no subs/masters, got %+v / %+v", got.Subscriptions, got.Masters)
	}
}

func TestSealOpen_RoundTrip(t *testing.T) {
	sealed, err := Seal(sampleBundle(), "hunter2")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(sealed, "hunter2")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if len(got.Rules) != 2 || got.Rules[1].Interface != "xray:work" {
		t.Errorf("rules round-trip mismatch: %+v", got.Rules)
	}
	if len(got.CustomGroups) != 1 || got.CustomGroups[0].Key != "custom:mine" {
		t.Errorf("groups round-trip mismatch: %+v", got.CustomGroups)
	}
}

func TestSeal_DefaultsSchema(t *testing.T) {
	b := sampleBundle()
	b.Schema = ""
	sealed, err := Seal(b, "pw")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(sealed, "pw")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.Schema != BundleSchema {
		t.Errorf("schema = %q, want %q", got.Schema, BundleSchema)
	}
}

func TestOpen_WrongPassphrase(t *testing.T) {
	sealed, _ := Seal(sampleBundle(), "right")
	if _, err := Open(sealed, "wrong"); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("got %v, want ErrWrongPassphrase", err)
	}
}

func TestSealOpen_EmptyPassphrase(t *testing.T) {
	if _, err := Seal(sampleBundle(), ""); !errors.Is(err, ErrEmptyPassphrase) {
		t.Errorf("Seal empty: got %v, want ErrEmptyPassphrase", err)
	}
	sealed, _ := Seal(sampleBundle(), "x")
	if _, err := Open(sealed, ""); !errors.Is(err, ErrEmptyPassphrase) {
		t.Errorf("Open empty: got %v, want ErrEmptyPassphrase", err)
	}
}

func TestOpen_BadEnvelope(t *testing.T) {
	if _, err := Open([]byte(`{"not":"ours"}`), "pw"); !errors.Is(err, ErrBadEnvelope) {
		t.Errorf("got %v, want ErrBadEnvelope", err)
	}
	if _, err := Open([]byte(`not json at all`), "pw"); !errors.Is(err, ErrBadEnvelope) {
		t.Errorf("got %v, want ErrBadEnvelope", err)
	}
}

func TestSeal_DistinctSaltNonce(t *testing.T) {
	a, _ := Seal(sampleBundle(), "pw")
	b, _ := Seal(sampleBundle(), "pw")
	if string(a) == string(b) {
		t.Error("two seals of identical bundle are byte-identical; salt/nonce not random")
	}
}
