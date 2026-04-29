package rules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// GORM models. The Rule type already lives in rules.go; we add tags here
// via a separate (non-exported) view if needed. Keeping the same struct
// for both API and persistence is simpler — GORM tags below.

type Setting struct {
	Key   string `gorm:"primaryKey;column:key"`
	Value string `gorm:"not null;column:value"`
}

func (Setting) TableName() string { return "settings" }

// LogEntry persisted by the daemon for the UI to display.
type LogEntry struct {
	ID        int64     `gorm:"primaryKey;column:id"`
	Timestamp time.Time `gorm:"not null;index;column:ts;autoCreateTime"`
	QueryName string    `gorm:"not null;column:query_name"`
	Action    string    `gorm:"not null;column:action"`
	RuleID    int64     `gorm:"column:rule_id;default:0"`
	Interface string    `gorm:"not null;column:interface;default:''"`
	ClientIP  string    `gorm:"not null;column:client_ip;default:''"`
}

func (LogEntry) TableName() string { return "log_entries" }

// gorm tags for Rule (defined in rules.go). We attach them via this
// alias only to keep the model declaration in one place.
func (Rule) TableName() string { return "rules" }

type Store struct {
	db *gorm.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Silent),
		DisableForeignKeyConstraintWhenMigrating: false,
	})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&Rule{}, &Setting{}, &LogEntry{}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// Data migration: previously, "allow" with a non-empty interface
	// meant "route via that interface". Now that's a distinct action.
	// Promote those rows so the data model is consistent.
	if err := db.Exec(
		`UPDATE rules SET action = 'route' WHERE action = 'allow' AND interface != ''`,
	).Error; err != nil {
		return nil, fmt.Errorf("migrate allow→route: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func (s *Store) Add(ctx context.Context, r Rule) (Rule, error) {
	if err := Validate(r.Pattern); err != nil {
		return Rule{}, err
	}
	if err := normalizeAction(&r); err != nil {
		return Rule{}, err
	}
	r.Pattern = normalize(r.Pattern)
	r.ID = 0
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now

	if err := s.db.WithContext(ctx).Create(&r).Error; err != nil {
		if isUniqueErr(err) {
			return Rule{}, ErrDuplicate
		}
		return Rule{}, fmt.Errorf("insert rule: %w", err)
	}
	return r, nil
}

func (s *Store) Update(ctx context.Context, r Rule) error {
	if err := Validate(r.Pattern); err != nil {
		return err
	}
	if err := normalizeAction(&r); err != nil {
		return err
	}
	r.Pattern = normalize(r.Pattern)
	r.UpdatedAt = time.Now().UTC()

	res := s.db.WithContext(ctx).Model(&Rule{}).
		Where("id = ?", r.ID).
		Select("Pattern", "Action", "Interface", "Enabled", "UpdatedAt").
		Updates(map[string]any{
			"pattern":    r.Pattern,
			"action":     string(r.Action),
			"interface":  r.Interface,
			"enabled":    r.Enabled,
			"updated_at": r.UpdatedAt,
		})
	if res.Error != nil {
		if isUniqueErr(res.Error) {
			return ErrDuplicate
		}
		return fmt.Errorf("update rule: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	res := s.db.WithContext(ctx).Delete(&Rule{}, id)
	if res.Error != nil {
		return fmt.Errorf("delete rule: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id int64) (Rule, error) {
	var r Rule
	err := s.db.WithContext(ctx).First(&r, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Rule{}, ErrNotFound
	}
	return r, err
}

func (s *Store) List(ctx context.Context) ([]Rule, error) {
	var out []Rule
	err := s.db.WithContext(ctx).Order("pattern ASC").Find(&out).Error
	return out, err
}

// Setting helpers (keyed key/value bag).

func (s *Store) GetSetting(ctx context.Context, key, def string) (string, error) {
	var row Setting
	err := s.db.WithContext(ctx).First(&row, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return def, nil
	}
	if err != nil {
		return "", err
	}
	return row.Value, nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	row := Setting{Key: key, Value: value}
	return s.db.WithContext(ctx).Save(&row).Error
}

// Logging.

// MaxLogEntries is the rolling cap on stored DNS log rows. Older rows
// are pruned by Log on every write, so the table stays bounded
// regardless of query volume. Bumping this is fine — the prune query
// is a single cutoff-id DELETE which is cheap at any reasonable size.
const MaxLogEntries = 500

func (s *Store) Log(ctx context.Context, e LogEntry) error {
	e.ID = 0
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if err := s.db.WithContext(ctx).Create(&e).Error; err != nil {
		return err
	}
	// Trim everything older than the Nth-newest row. Subquery returns
	// NULL when there are <N rows, in which case `id < NULL` matches
	// nothing — safe no-op while the table is still filling up.
	return s.db.WithContext(ctx).Exec(
		`DELETE FROM log_entries WHERE id < (
            SELECT id FROM log_entries ORDER BY id DESC LIMIT 1 OFFSET ?
        )`, MaxLogEntries-1,
	).Error
}

// RecentLogs returns up to limit log entries, newest first.
//
// filter narrows what's returned at the DB level — important so a
// "Routed" filter doesn't get hidden by a flood of recent
// block-app-down rows pushing routes off the latest-N window.
//
//	""             → no filter (default)
//	"route"        → only successful routes
//	"block"        → only rule-matched blocks
//	"unavailable"  → every block-* + forward-failed (anything that
//	                 didn't reach the destination)
//	any other str  → exact match on action column
func (s *Store) RecentLogs(ctx context.Context, limit int, filter string) ([]LogEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	q := s.db.WithContext(ctx).Order("ts DESC").Limit(limit)
	switch filter {
	case "":
		// no filter
	case "unavailable":
		q = q.Where("action LIKE ? OR action = ?", "block-%", "forward-failed")
	default:
		q = q.Where("action = ?", filter)
	}
	var out []LogEntry
	err := q.Find(&out).Error
	return out, err
}

// normalizeAction validates the action + interface combination and
// canonicalises r in place:
//
//   - block: interface forced to "".
//   - allow: interface forced to "" (kept internal — UI never creates
//     these directly; it's the implicit default for unmatched
//     queries, but stored explicit "allow" rules still serve as
//     "override broader block").
//   - route: interface required (utunN, enN, or "app:KEY[,KEY...]").
func normalizeAction(r *Rule) error {
	if !ValidAction(r.Action) {
		return ErrInvalidAction
	}
	switch r.Action {
	case ActionBlock, ActionAllow:
		r.Interface = ""
	case ActionRoute:
		if r.Interface == "" {
			return ErrInvalidAction
		}
	}
	return nil
}

func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
