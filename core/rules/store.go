package rules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// TrafficStat is one time-bucketed byte tally for proxied traffic.
// Only traffic that the daemon relays (SOCKS/xray routes) is counted —
// DNS-only and kernel-routed bytes never pass through userspace. Rows
// accumulate in place: (BucketTS, Domain, Proxy) is the composite key,
// and bytes are added on conflict rather than overwritten.
type TrafficStat struct {
	BucketTS  int64  `gorm:"primaryKey;column:bucket_ts"` // unix seconds, floored to TrafficBucket
	Domain    string `gorm:"primaryKey;column:domain"`
	Proxy     string `gorm:"primaryKey;column:proxy"`
	GroupKey  string `gorm:"column:group_key;index;default:''"` // "" = ungrouped
	BytesSent int64  `gorm:"column:bytes_sent;default:0"`       // client → proxy
	BytesRecv int64  `gorm:"column:bytes_recv;default:0"`       // proxy → client
}

func (TrafficStat) TableName() string { return "traffic_stats" }

// TrafficBucket is the finest-grained time-bucket width usage stats are
// STORED at. One minute is the floor; QueryTraffic re-buckets up to any
// coarser display width (e.g. 5 min) at read time, so the UI can switch
// resolution without a data migration. ~1440 rows/day/domain/proxy.
const TrafficBucket = 1 * time.Minute

// TrafficBucketSeconds is the stored granularity in seconds; the smallest
// display bucket a query may ask for.
const TrafficBucketSeconds = int64(TrafficBucket / time.Second)

// TrafficRetention is how long usage history is kept before SweepTraffic
// prunes it.
const TrafficRetention = 7 * 24 * time.Hour

// FloorBucket rounds a time down to the start of its TrafficBucket and
// returns the unix-seconds bucket key.
func FloorBucket(t time.Time) int64 {
	w := int64(TrafficBucket / time.Second)
	return (t.UTC().Unix() / w) * w
}

// TrafficPoint is one aggregated row returned by QueryTraffic: total
// bytes for a single (bucket, key) where key is the grouping dimension's
// value (group key, domain, or proxy name).
type TrafficPoint struct {
	BucketTS  int64  `gorm:"column:bucket_ts"`
	Key       string `gorm:"column:k"`
	BytesSent int64  `gorm:"column:bytes_sent"`
	BytesRecv int64  `gorm:"column:bytes_recv"`
}

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

	if err := db.AutoMigrate(&Rule{}, &Setting{}, &LogEntry{}, &TrafficStat{}, &CustomGroup{}); err != nil {
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
	// Remember the caller's intent: Create both skips the zero value AND
	// writes the column default back into the struct, so r.Enabled is true
	// by the time the insert returns regardless of what was asked for.
	wantEnabled := r.Enabled

	if err := s.db.WithContext(ctx).Create(&r).Error; err != nil {
		if isUniqueErr(err) {
			return Rule{}, ErrDuplicate
		}
		return Rule{}, fmt.Errorf("insert rule: %w", err)
	}
	// GORM omits a zero-valued field whose column carries a `default:` tag,
	// so `Enabled: false` on insert would silently land as enabled=true —
	// i.e. adding a rule (or applying a group) as disabled would still route
	// traffic. Force the column when the caller asked for disabled.
	if !wantEnabled {
		if err := s.db.WithContext(ctx).Model(&Rule{}).
			Where("id = ?", r.ID).Update("enabled", false).Error; err != nil {
			return Rule{}, fmt.Errorf("insert rule (disable): %w", err)
		}
		r.Enabled = false
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

// DeleteByInterfacePrefix hard-deletes every rule whose Interface field
// begins with prefix. Used to purge the retired "app:KEY" routing rules
// on daemon startup. Returns the number of rows removed.
func (s *Store) DeleteByInterfacePrefix(ctx context.Context, prefix string) (int64, error) {
	res := s.db.WithContext(ctx).
		Where("interface LIKE ?", prefix+"%").
		Delete(&Rule{})
	if res.Error != nil {
		return 0, fmt.Errorf("delete rules by interface prefix: %w", res.Error)
	}
	return res.RowsAffected, nil
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

// RenameInterfaceRef rewrites every Rule whose Interface field is of
// the form "PREFIX:NAME[,NAME...]" so each occurrence of oldName in the
// comma-separated list becomes newName. Used when the user renames an
// outbound (proxy, xray) so existing rules don't silently break.
//
// Returns the number of rule rows updated. No-op (returns 0 with no
// error) when prefix is empty, oldName == newName, or no rule matches.
// Names are compared case-insensitively after trimming whitespace, to
// match the normalisation proxy/xray stores apply on insert. Duplicate
// resulting names are collapsed so a rule like "proxy:a,b" renamed
// (a→b) becomes "proxy:b", not "proxy:b,b".
func (s *Store) RenameInterfaceRef(ctx context.Context, prefix, oldName, newName string) (int64, error) {
	prefix = strings.TrimSpace(prefix)
	o := strings.ToLower(strings.TrimSpace(oldName))
	n := strings.ToLower(strings.TrimSpace(newName))
	if prefix == "" || o == "" || n == "" || o == n {
		return 0, nil
	}

	var rows []Rule
	if err := s.db.WithContext(ctx).
		Where("interface LIKE ?", prefix+"%").
		Find(&rows).Error; err != nil {
		return 0, fmt.Errorf("list rules for rename: %w", err)
	}

	var updated int64
	now := time.Now().UTC()
	for _, r := range rows {
		newIface, changed := rewriteMultiNameInterface(r.Interface, prefix, o, n)
		if !changed {
			continue
		}
		res := s.db.WithContext(ctx).Model(&Rule{}).
			Where("id = ?", r.ID).
			Updates(map[string]any{
				"interface":  newIface,
				"updated_at": now,
			})
		if res.Error != nil {
			return updated, fmt.Errorf("update rule %d: %w", r.ID, res.Error)
		}
		updated += res.RowsAffected
	}
	return updated, nil
}

// rewriteMultiNameInterface returns the rewritten Interface field plus
// a boolean indicating whether anything changed. The input must start
// with prefix; names in the comma list are compared case-insensitively
// after trim. Duplicate resulting names are dropped (keeping first
// occurrence) so a rename that collides with an already-listed name
// collapses cleanly.
func rewriteMultiNameInterface(iface, prefix, oldName, newName string) (string, bool) {
	if !strings.HasPrefix(iface, prefix) {
		return iface, false
	}
	body := strings.TrimPrefix(iface, prefix)
	parts := strings.Split(body, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	changed := false
	for _, p := range parts {
		name := strings.ToLower(strings.TrimSpace(p))
		if name == "" {
			continue
		}
		if name == oldName {
			name = newName
			changed = true
		}
		if seen[name] {
			changed = true
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if !changed {
		return iface, false
	}
	return prefix + strings.Join(out, ","), true
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

// ClearLogs deletes all rows from the log_entries table.
func (s *Store) ClearLogs(ctx context.Context) error {
	return s.db.WithContext(ctx).Exec("DELETE FROM log_entries").Error
}

// Traffic usage stats.

// AddTraffic accumulates sent/recv byte deltas into the (bucketTS,
// domain, proxy) row, inserting it if absent. On conflict the byte
// columns are incremented (not overwritten) so repeated flushes for the
// same connection sum correctly. groupKey is refreshed on every write so
// a late-resolved group attribution still lands. Zero-byte deltas are a
// no-op.
func (s *Store) AddTraffic(ctx context.Context, bucketTS int64, domain, proxy, groupKey string, sent, recv int64) error {
	if sent == 0 && recv == 0 {
		return nil
	}
	row := TrafficStat{
		BucketTS:  bucketTS,
		Domain:    domain,
		Proxy:     proxy,
		GroupKey:  groupKey,
		BytesSent: sent,
		BytesRecv: recv,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "bucket_ts"}, {Name: "domain"}, {Name: "proxy"}},
		DoUpdates: clause.Assignments(map[string]any{
			"bytes_sent": gorm.Expr("bytes_sent + ?", sent),
			"bytes_recv": gorm.Expr("bytes_recv + ?", recv),
			"group_key":  groupKey,
		}),
	}).Create(&row).Error
}

// TrafficDimension is the column a usage query groups by.
const (
	TrafficByGroup  = "group"
	TrafficByDomain = "domain"
	TrafficByProxy  = "proxy"
)

// QueryTraffic returns per-bucket byte totals between from and to (unix
// seconds, inclusive), grouped by both the time bucket and the chosen
// dimension. dim is one of TrafficByGroup/Domain/Proxy; anything else
// defaults to domain. Group dimension reports "" rows under the literal
// key "ungrouped" so the UI has a stable label.
//
// bucketSec is the display bucket width in seconds. Rows stored at the
// 1-minute granularity are re-floored to multiples of bucketSec so the
// caller can request coarser resolution (e.g. 300 for 5-minute buckets)
// off the same data. Values below the stored granularity are clamped up.
func (s *Store) QueryTraffic(ctx context.Context, from, to int64, dim string, bucketSec int64) ([]TrafficPoint, error) {
	if bucketSec < TrafficBucketSeconds {
		bucketSec = TrafficBucketSeconds
	}
	var keyExpr string
	switch dim {
	case TrafficByGroup:
		keyExpr = "CASE WHEN group_key = '' THEN 'ungrouped' ELSE group_key END"
	case TrafficByProxy:
		keyExpr = "proxy"
	default:
		keyExpr = "domain"
	}
	// bucketSec is an integer we fully control (clamped above), so inlining
	// it into the re-floor expression is injection-safe.
	timeExpr := fmt.Sprintf("(bucket_ts / %d) * %d", bucketSec, bucketSec)
	var out []TrafficPoint
	err := s.db.WithContext(ctx).
		Model(&TrafficStat{}).
		Select(timeExpr+" AS bucket_ts, "+keyExpr+" AS k, SUM(bytes_sent) AS bytes_sent, SUM(bytes_recv) AS bytes_recv").
		Where("bucket_ts >= ? AND bucket_ts <= ?", from, to).
		Group(timeExpr+", k").
		Order(timeExpr+" ASC").
		Scan(&out).Error
	return out, err
}

// SweepTraffic deletes traffic rows whose bucket starts before the given
// time. Called on the daemon's TTL sweep tick with now-TrafficRetention.
func (s *Store) SweepTraffic(ctx context.Context, before time.Time) error {
	return s.db.WithContext(ctx).
		Where("bucket_ts < ?", before.UTC().Unix()).
		Delete(&TrafficStat{}).Error
}

// normalizeAction validates the action + interface combination and
// canonicalises r in place:
//
//   - block: interface forced to "".
//   - allow: interface forced to "" (kept internal — UI never creates
//     these directly; it's the implicit default for unmatched
//     queries, but stored explicit "allow" rules still serve as
//     "override broader block").
//   - route: interface required (utunN, enN, proxy:NAME, or xray:NAME).
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
