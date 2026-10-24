package rules

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type preSuggestCustomGroup struct {
	ID          int64     `gorm:"primaryKey;column:id"`
	Key         string    `gorm:"not null;uniqueIndex;column:key"`
	DisplayName string    `gorm:"not null;column:display_name"`
	Description string    `gorm:"not null;default:'';column:description"`
	Patterns    []string  `gorm:"column:patterns;serializer:json"`
	Color       string    `gorm:"not null;default:'';column:color"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (preSuggestCustomGroup) TableName() string { return "custom_groups" }

// A DB written before source_tag existed must open, migrate additively and
// read its groups back unchanged.
func TestCustomGroups_OldSchemaLoadsUnchanged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	raw, err := gorm.Open(sqlite.Open("file:"+path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	// The custom_groups model exactly as shipped before group suggestions,
	// migrated the way an old daemon did it.
	if err := raw.AutoMigrate(&preSuggestCustomGroup{}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Exec(`INSERT INTO custom_groups (key, display_name, description, patterns, color, created_at, updated_at)
		 VALUES ('custom:work', 'Work', 'office stuff', '["*.corp.example","10.0.0.0/8"]', '#123456',
		         '2026-01-02 03:04:05', '2026-01-02 03:04:05')`).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := raw.DB()
	_ = sqlDB.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open old DB: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	list, err := s.ListCustomGroups(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	g := list[0]
	if g.Key != "custom:work" || g.DisplayName != "Work" || g.Description != "office stuff" ||
		g.Color != "#123456" || len(g.Patterns) != 2 || g.Patterns[0] != "*.corp.example" ||
		g.Patterns[1] != "10.0.0.0/8" || g.SourceTag != "" {
		t.Fatalf("old row changed: %+v", g)
	}

	// An edit of the old row keeps working, and a new row can carry a tag.
	if err := s.UpdateCustomGroup(ctx, CustomGroup{Key: g.Key, DisplayName: "Work", Patterns: g.Patterns}); err != nil {
		t.Fatalf("update old row: %v", err)
	}
	n, err := s.AddCustomGroup(ctx, CustomGroup{DisplayName: "Spotify", Patterns: []string{"*.spotify.com"}, SourceTag: "spotify"})
	if err != nil {
		t.Fatal(err)
	}
	if n.SourceTag != "SPOTIFY" {
		t.Errorf("source tag = %q, want SPOTIFY", n.SourceTag)
	}
}

// UpdateCustomGroup must never clear (or set) source_tag: it is written
// once, at creation.
func TestCustomGroups_UpdateKeepsSourceTag(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	g, err := s.AddCustomGroup(ctx, CustomGroup{DisplayName: "Spotify", Patterns: []string{"*.spotify.com"}, SourceTag: "SPOTIFY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateCustomGroup(ctx, CustomGroup{Key: g.Key, DisplayName: "Music", Patterns: []string{"*.scdn.co"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetCustomGroup(ctx, g.Key)
	if got.SourceTag != "SPOTIFY" || got.DisplayName != "Music" {
		t.Fatalf("after update: %+v", got)
	}
}
