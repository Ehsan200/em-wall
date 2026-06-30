package rules

import (
	"context"
	"errors"
	"testing"
)

func TestCustomGroups_AddListGetUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	g, err := s.AddCustomGroup(ctx, CustomGroup{
		DisplayName: "My Stuff",
		Description: "personal",
		Patterns:    []string{"*.Example.com ", "example.com", "1.2.3.4"},
		Color:       "#abcdef",
	})
	if err != nil {
		t.Fatalf("AddCustomGroup: %v", err)
	}
	// Key derived + prefixed; patterns normalized and deduped.
	if g.Key != "custom:my-stuff" {
		t.Errorf("key = %q, want custom:my-stuff", g.Key)
	}
	if len(g.Patterns) != 3 {
		t.Fatalf("patterns = %v, want 3 (deduped/normalized)", g.Patterns)
	}
	if g.Patterns[0] != "*.example.com" {
		t.Errorf("pattern[0] = %q, want normalized *.example.com", g.Patterns[0])
	}

	// Duplicate key rejected.
	if _, err := s.AddCustomGroup(ctx, CustomGroup{DisplayName: "My Stuff"}); !errors.Is(err, ErrGroupDuplicate) {
		t.Errorf("dup add: got %v, want ErrGroupDuplicate", err)
	}

	got, err := s.GetCustomGroup(ctx, "my-stuff") // bare key gets prefixed
	if err != nil {
		t.Fatalf("GetCustomGroup: %v", err)
	}
	if got.DisplayName != "My Stuff" {
		t.Errorf("display = %q", got.DisplayName)
	}

	// Update rewrites patterns + clears description.
	if err := s.UpdateCustomGroup(ctx, CustomGroup{
		Key:         "custom:my-stuff",
		DisplayName: "Renamed",
		Patterns:    []string{"*.new.com"},
	}); err != nil {
		t.Fatalf("UpdateCustomGroup: %v", err)
	}
	got, _ = s.GetCustomGroup(ctx, "custom:my-stuff")
	if got.DisplayName != "Renamed" || got.Description != "" || len(got.Patterns) != 1 || got.Patterns[0] != "*.new.com" {
		t.Errorf("after update: %+v", got)
	}

	list, err := s.ListCustomGroups(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListCustomGroups: %v len=%d", err, len(list))
	}

	if err := s.DeleteCustomGroup(ctx, "custom:my-stuff"); err != nil {
		t.Fatalf("DeleteCustomGroup: %v", err)
	}
	if _, err := s.GetCustomGroup(ctx, "custom:my-stuff"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("get after delete: got %v, want ErrGroupNotFound", err)
	}
}

func TestCustomGroups_Validation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.AddCustomGroup(ctx, CustomGroup{DisplayName: "  "}); !errors.Is(err, ErrEmptyGroupName) {
		t.Errorf("empty name: got %v, want ErrEmptyGroupName", err)
	}
	if _, err := s.AddCustomGroup(ctx, CustomGroup{DisplayName: "Bad", Patterns: []string{"has space"}}); err == nil {
		t.Errorf("invalid pattern: expected error")
	}
	// Symbol-only name yields no slug → empty-key error.
	if _, err := s.AddCustomGroup(ctx, CustomGroup{DisplayName: "***"}); !errors.Is(err, ErrEmptyGroupKey) {
		t.Errorf("symbol-only name: got %v, want ErrEmptyGroupKey", err)
	}
}
