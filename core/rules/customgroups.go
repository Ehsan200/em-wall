package rules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// CustomGroupPrefix namespaces user-created group keys so they can never
// collide with the curated registry (core/groups). Every key persisted by
// the store carries this prefix; callers that pass a bare key get it
// prepended automatically by canonGroupKey.
const CustomGroupPrefix = "custom:"

// CustomGroup is a user-defined bundle of domain/IP patterns, the DB-backed
// sibling of the code-defined core/groups registry. Patterns are stored as
// a JSON array via GORM's json serializer. Color is an optional brand-accent
// hex used by the UI badge.
type CustomGroup struct {
	ID          int64     `gorm:"primaryKey;column:id"`
	Key         string    `gorm:"not null;uniqueIndex;column:key"`
	DisplayName string    `gorm:"not null;column:display_name"`
	Description string    `gorm:"not null;default:'';column:description"`
	Patterns    []string  `gorm:"column:patterns;serializer:json"`
	Color       string    `gorm:"not null;default:'';column:color"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (CustomGroup) TableName() string { return "custom_groups" }

// SlugifyGroupKey derives a stable, prefixed key from a display name:
// lowercase, non-alphanumeric runs collapsed to '-', trimmed. Returns just
// the prefix (e.g. "custom:") for an all-symbol name; callers should treat
// that as needing a fallback. Existing prefixed keys pass through unchanged.
func SlugifyGroupKey(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, CustomGroupPrefix) {
		return s
	}
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	return CustomGroupPrefix + slug
}

// canonGroupKey ensures a key carries the custom: prefix exactly once.
func canonGroupKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if strings.HasPrefix(key, CustomGroupPrefix) {
		return key
	}
	return CustomGroupPrefix + key
}

// sanitizeGroup validates and canonicalises a custom group in place:
// trims the display name, normalizes + validates + dedupes patterns, and
// canonicalises the key (deriving one from the display name when empty).
func sanitizeGroup(g *CustomGroup) error {
	g.DisplayName = strings.TrimSpace(g.DisplayName)
	if g.DisplayName == "" {
		return ErrEmptyGroupName
	}
	if strings.TrimSpace(g.Key) == "" {
		g.Key = SlugifyGroupKey(g.DisplayName)
	} else {
		g.Key = canonGroupKey(g.Key)
	}
	if g.Key == "" || g.Key == CustomGroupPrefix {
		return ErrEmptyGroupKey
	}

	seen := make(map[string]bool, len(g.Patterns))
	out := make([]string, 0, len(g.Patterns))
	for _, p := range g.Patterns {
		np := normalize(p)
		if np == "" {
			continue
		}
		if err := Validate(np); err != nil {
			return fmt.Errorf("pattern %q: %w", p, err)
		}
		if seen[np] {
			continue
		}
		seen[np] = true
		out = append(out, np)
	}
	g.Patterns = out
	return nil
}

// AddCustomGroup inserts a new custom group. The key is derived from the
// display name when omitted. Returns ErrGroupDuplicate if the key is taken.
func (s *Store) AddCustomGroup(ctx context.Context, g CustomGroup) (CustomGroup, error) {
	if err := sanitizeGroup(&g); err != nil {
		return CustomGroup{}, err
	}
	g.ID = 0
	now := time.Now().UTC()
	g.CreatedAt = now
	g.UpdatedAt = now
	if err := s.db.WithContext(ctx).Create(&g).Error; err != nil {
		if isUniqueErr(err) {
			return CustomGroup{}, ErrGroupDuplicate
		}
		return CustomGroup{}, fmt.Errorf("insert custom group: %w", err)
	}
	return g, nil
}

// UpdateCustomGroup rewrites a group's display name, description, patterns,
// and color, matched by its (immutable) key. Returns ErrGroupNotFound when
// no row carries that key.
func (s *Store) UpdateCustomGroup(ctx context.Context, g CustomGroup) error {
	if err := sanitizeGroup(&g); err != nil {
		return err
	}
	g.UpdatedAt = time.Now().UTC()
	// Struct-based update with an explicit Select so the json serializer
	// runs on Patterns and zero-valued fields (e.g. cleared description)
	// are still written. A map update would bypass the serializer.
	res := s.db.WithContext(ctx).Model(&CustomGroup{}).
		Where("key = ?", g.Key).
		Select("display_name", "description", "patterns", "color", "updated_at").
		Updates(CustomGroup{
			DisplayName: g.DisplayName,
			Description: g.Description,
			Patterns:    g.Patterns,
			Color:       g.Color,
			UpdatedAt:   g.UpdatedAt,
		})
	if res.Error != nil {
		return fmt.Errorf("update custom group: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrGroupNotFound
	}
	return nil
}

// DeleteCustomGroup removes the group definition by key. It does NOT touch
// rules created from the group's patterns — the caller decides whether to
// also purge those (see the daemon's groups.delete handler).
func (s *Store) DeleteCustomGroup(ctx context.Context, key string) error {
	key = canonGroupKey(key)
	res := s.db.WithContext(ctx).Where("key = ?", key).Delete(&CustomGroup{})
	if res.Error != nil {
		return fmt.Errorf("delete custom group: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrGroupNotFound
	}
	return nil
}

// GetCustomGroup fetches a single custom group by key.
func (s *Store) GetCustomGroup(ctx context.Context, key string) (CustomGroup, error) {
	key = canonGroupKey(key)
	var g CustomGroup
	err := s.db.WithContext(ctx).First(&g, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CustomGroup{}, ErrGroupNotFound
	}
	return g, err
}

// ListCustomGroups returns all custom groups ordered by display name.
func (s *Store) ListCustomGroups(ctx context.Context) ([]CustomGroup, error) {
	var out []CustomGroup
	err := s.db.WithContext(ctx).Order("display_name ASC").Find(&out).Error
	return out, err
}
