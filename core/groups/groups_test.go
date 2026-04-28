package groups

import "testing"

func TestKnownGroups_HaveExpectedKeys(t *testing.T) {
	want := []string{"anthropic", "openai", "google-ai"}
	have := map[string]bool{}
	for _, g := range KnownGroups() {
		have[g.Key] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing group %q", w)
		}
	}
}

func TestKnownGroups_PatternsLookValid(t *testing.T) {
	for _, g := range KnownGroups() {
		if g.Key == "" || g.DisplayName == "" {
			t.Errorf("group has empty key/name: %+v", g)
		}
		if len(g.Patterns) == 0 {
			t.Errorf("group %q has no patterns", g.Key)
		}
		for _, p := range g.Patterns {
			if p == "" {
				t.Errorf("group %q has an empty pattern", g.Key)
			}
		}
	}
}

func TestFindByKey(t *testing.T) {
	g := FindByKey("anthropic")
	if g == nil {
		t.Fatal("expected to find anthropic")
	}
	if g.Key != "anthropic" {
		t.Errorf("got %q", g.Key)
	}
	if FindByKey("nonexistent") != nil {
		t.Errorf("expected nil for unknown key")
	}
}
