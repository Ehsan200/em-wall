package xray

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

const (
	linkVLESS  = "vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&security=tls&type=tcp#DE-Node1"
	linkVLESS2 = "vless://11111111-1111-1111-1111-111111111111@example.com:443?encryption=none&security=tls&type=tcp#DE-Node1-dup"
	linkTrojan = "trojan://secretpass@example.org:8443?security=tls&sni=example.org#NL-Node2"
)

func b64Body(links ...string) []byte {
	return []byte(base64.StdEncoding.EncodeToString([]byte(strings.Join(links, "\n"))))
}

func TestParseSubscriptionBody_DedupeAndNames(t *testing.T) {
	// linkVLESS2 is linkVLESS with a different fragment only — must collapse
	// to a single node by fingerprint.
	body := b64Body(linkVLESS, linkVLESS2, linkTrojan)
	nodes, err := ParseSubscriptionBody(body, "mysub")
	if err != nil {
		t.Fatalf("ParseSubscriptionBody: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2 (dedupe by fingerprint): %+v", len(nodes), nodes)
	}
	for _, n := range nodes {
		if !strings.HasPrefix(n.Name, "mysub/") {
			t.Errorf("node name %q not prefixed with subscription name", n.Name)
		}
		if n.Fingerprint == "" {
			t.Errorf("node %q has empty fingerprint", n.Name)
		}
		if n.LastLatencyMs != -1 {
			t.Errorf("node %q LastLatencyMs = %d, want -1", n.Name, n.LastLatencyMs)
		}
	}
	if nodes[0].Fingerprint == nodes[1].Fingerprint {
		t.Errorf("distinct nodes share a fingerprint")
	}
}

func TestFingerprint_StableAcrossParses(t *testing.T) {
	body := b64Body(linkVLESS, linkTrojan)
	a, err := ParseSubscriptionBody(body, "s")
	if err != nil {
		t.Fatalf("parse a: %v", err)
	}
	b, err := ParseSubscriptionBody(body, "s")
	if err != nil {
		t.Fatalf("parse b: %v", err)
	}
	for i := range a {
		if a[i].Fingerprint != b[i].Fingerprint {
			t.Errorf("fingerprint[%d] unstable: %q vs %q", i, a[i].Fingerprint, b[i].Fingerprint)
		}
	}
}

func TestDecodeSubscription_Plaintext(t *testing.T) {
	txt := strings.Join([]string{linkVLESS, linkTrojan}, "\n")
	lines := DecodeSubscription([]byte(txt))
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "vless://") {
		t.Errorf("plaintext decode = %v, want the two links verbatim", lines)
	}
}

func TestParseSubscriptionBody_Empty(t *testing.T) {
	_, err := ParseSubscriptionBody(b64Body("not-a-link", "also nope"), "s")
	if err == nil {
		t.Fatal("expected ErrEmptySubscription for a body with no parseable links")
	}
}

func TestSubStore_ReplaceNodesAndActivate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub, err := s.AddSub(ctx, Subscription{Name: "mysub", URL: "https://example.com/sub"})
	if err != nil {
		t.Fatalf("AddSub: %v", err)
	}

	nodes, err := ParseSubscriptionBody(b64Body(linkVLESS, linkTrojan), sub.Name)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := s.ReplaceNodes(ctx, sub.ID, nodes); err != nil {
		t.Fatalf("ReplaceNodes: %v", err)
	}

	active, err := s.ActiveNodesForSubs(ctx, []string{"mysub"})
	if err != nil {
		t.Fatalf("ActiveNodesForSubs: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active = %d, want 2 (default cap, all enabled)", len(active))
	}
}

func TestSubStore_DisablePersistsAcrossRefresh(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub, err := s.AddSub(ctx, Subscription{Name: "mysub", URL: "https://example.com/sub"})
	if err != nil {
		t.Fatalf("AddSub: %v", err)
	}
	nodes, _ := ParseSubscriptionBody(b64Body(linkVLESS, linkTrojan), sub.Name)
	if err := s.ReplaceNodes(ctx, sub.ID, nodes); err != nil {
		t.Fatalf("ReplaceNodes: %v", err)
	}

	// Disable the first node by fingerprint.
	if err := s.SetNodeDisabled(ctx, sub.ID, nodes[0].Fingerprint, true); err != nil {
		t.Fatalf("SetNodeDisabled: %v", err)
	}
	active, _ := s.ActiveNodesForSubs(ctx, []string{"mysub"})
	if len(active) != 1 || active[0].Fingerprint != nodes[1].Fingerprint {
		t.Fatalf("after disable, active = %+v, want only node[1]", active)
	}

	// Simulate an auto-refresh: same node set replaces the volatile rows.
	if err := s.ReplaceNodes(ctx, sub.ID, nodes); err != nil {
		t.Fatalf("ReplaceNodes (refresh): %v", err)
	}
	active, _ = s.ActiveNodesForSubs(ctx, []string{"mysub"})
	if len(active) != 1 || active[0].Fingerprint != nodes[1].Fingerprint {
		t.Fatalf("disable did not survive refresh: active = %+v", active)
	}

	// Re-enable clears the override.
	if err := s.SetNodeDisabled(ctx, sub.ID, nodes[0].Fingerprint, false); err != nil {
		t.Fatalf("SetNodeDisabled(false): %v", err)
	}
	active, _ = s.ActiveNodesForSubs(ctx, []string{"mysub"})
	if len(active) != 2 {
		t.Fatalf("after re-enable, active = %d, want 2", len(active))
	}
}

func TestSubStore_CapLimitsActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub, err := s.AddSub(ctx, Subscription{Name: "capped", URL: "https://example.com/sub", NodeCap: 1})
	if err != nil {
		t.Fatalf("AddSub: %v", err)
	}
	nodes, _ := ParseSubscriptionBody(b64Body(linkVLESS, linkTrojan), sub.Name)
	if err := s.ReplaceNodes(ctx, sub.ID, nodes); err != nil {
		t.Fatalf("ReplaceNodes: %v", err)
	}
	active, _ := s.ActiveNodesForSubs(ctx, []string{"capped"})
	if len(active) != 1 {
		t.Fatalf("cap=1: active = %d, want 1", len(active))
	}
}

func TestSubStore_DeleteCascades(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sub, _ := s.AddSub(ctx, Subscription{Name: "gone", URL: "https://example.com/sub"})
	nodes, _ := ParseSubscriptionBody(b64Body(linkVLESS, linkTrojan), sub.Name)
	_ = s.ReplaceNodes(ctx, sub.ID, nodes)
	_ = s.SetNodeDisabled(ctx, sub.ID, nodes[0].Fingerprint, true)

	if err := s.DeleteSub(ctx, sub.ID); err != nil {
		t.Fatalf("DeleteSub: %v", err)
	}
	if left, _ := s.ListNodes(ctx, sub.ID); len(left) != 0 {
		t.Errorf("nodes survived subscription delete: %d", len(left))
	}
	var ov []SubNodeOverride
	if err := s.db.WithContext(ctx).Where("sub_id = ?", sub.ID).Find(&ov).Error; err != nil {
		t.Fatalf("query overrides: %v", err)
	}
	if len(ov) != 0 {
		t.Errorf("overrides survived subscription delete: %d", len(ov))
	}
}

func TestStore_RenameDialerRefs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fro := `{"protocol":"freedom"}`

	m, err := s.Add(ctx, Config{Name: "m", Outbound: fro, Enabled: true, Dialer: "xray:a,xraysub:s1"})
	if err != nil {
		t.Fatalf("Add master: %v", err)
	}

	if n, err := s.RenameDialerSubRef(ctx, "s1", "s2"); err != nil || n != 1 {
		t.Fatalf("RenameDialerSubRef = %d,%v, want 1,nil", n, err)
	}
	got, _ := s.Get(ctx, m.ID)
	if got.Dialer != "xray:a,xraysub:s2" {
		t.Errorf("after sub rename Dialer = %q, want %q", got.Dialer, "xray:a,xraysub:s2")
	}

	if n, err := s.RenameDialerXrayRef(ctx, "a", "b"); err != nil || n != 1 {
		t.Fatalf("RenameDialerXrayRef = %d,%v, want 1,nil", n, err)
	}
	got, _ = s.Get(ctx, m.ID)
	if got.Dialer != "xray:b,xraysub:s2" {
		t.Errorf("after xray rename Dialer = %q, want %q", got.Dialer, "xray:b,xraysub:s2")
	}

	// Collision dedupes: xraysub:s1,xraysub:s2 renamed s1→s2 becomes xraysub:s2.
	m2, _ := s.Add(ctx, Config{Name: "m2", Outbound: fro, Enabled: true, Dialer: "xraysub:s1,xraysub:s2"})
	if _, err := s.RenameDialerSubRef(ctx, "s1", "s2"); err != nil {
		t.Fatalf("RenameDialerSubRef dedupe: %v", err)
	}
	got, _ = s.Get(ctx, m2.ID)
	if got.Dialer != "xraysub:s2" {
		t.Errorf("dedupe Dialer = %q, want %q", got.Dialer, "xraysub:s2")
	}
}

func TestStore_AllNodeCounts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sub, _ := s.AddSub(ctx, Subscription{Name: "s", URL: "https://example.com/sub"})
	nodes, _ := ParseSubscriptionBody(b64Body(linkVLESS, linkTrojan), sub.Name)
	_ = s.ReplaceNodes(ctx, sub.ID, nodes)
	_ = s.SetNodeDisabled(ctx, sub.ID, nodes[0].Fingerprint, true)

	counts, err := s.AllNodeCounts(ctx)
	if err != nil {
		t.Fatalf("AllNodeCounts: %v", err)
	}
	c := counts[sub.ID]
	if c[0] != 2 || c[1] != 1 {
		t.Errorf("counts = total %d active %d, want 2,1", c[0], c[1])
	}
}

func TestSubStore_RejectsBadURL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.AddSub(ctx, Subscription{Name: "bad", URL: "ftp://nope"}); err == nil {
		t.Error("expected error for non-http subscription URL")
	}
	if _, err := s.AddSub(ctx, Subscription{Name: "bad2", URL: ""}); err == nil {
		t.Error("expected error for empty subscription URL")
	}
}
