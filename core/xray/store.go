package xray

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store persists xray Configs in the same SQLite file as rules + proxies.
type Store struct {
	db *gorm.DB
}

// Open opens path and migrates the xray_configs table. The same WAL +
// busy-timeout pragmas the other stores use; SetMaxOpenConns(1)
// keeps SQLite happy when multiple stores share the file.
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

	if err := db.AutoMigrate(&Config{}, &kvSetting{},
		&Subscription{}, &SubNode{}, &SubNodeOverride{}, &Set{}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// kvSetting is the xray-scoped key/value table backing global
// settings like the user-supplied routing-rules JSON. Kept here
// rather than in the rules store so the xray feature owns its data.
type kvSetting struct {
	Key   string `gorm:"primaryKey;column:key"`
	Value string `gorm:"column:value;type:text;not null;default:''"`
}

func (kvSetting) TableName() string { return "xray_settings" }

// GetSetting returns the stored value for key, or def if missing.
func (s *Store) GetSetting(ctx context.Context, key, def string) (string, error) {
	var row kvSetting
	err := s.db.WithContext(ctx).First(&row, "key = ?", key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return def, nil
	}
	if err != nil {
		return "", err
	}
	return row.Value, nil
}

// SetSetting upserts the value for key.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	row := kvSetting{Key: key, Value: value}
	// gorm's Save inserts on no-match, updates otherwise. Primary-key
	// match is enough for upsert here.
	return s.db.WithContext(ctx).Save(&row).Error
}

// GetRoutingRules returns the user's raw routing-rules JSON (a JSON
// array). Empty string means "no user rules — supervisor emits only
// the per-entry inbound→outbound pairs".
func (s *Store) GetRoutingRules(ctx context.Context) (string, error) {
	return s.GetSetting(ctx, SettingRoutingKey, "")
}

// SetRoutingRules validates raw and upserts it. Empty raw or "[]" both
// clear the rules. Anything else must parse as a JSON ARRAY of OBJECTS.
func (s *Store) SetRoutingRules(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if err := ValidateRoutingRules(raw); err != nil {
		return err
	}
	return s.SetSetting(ctx, SettingRoutingKey, raw)
}

// ValidateRoutingRules enforces the minimum invariant Generate relies
// on: empty (treated as no rules) OR a JSON array whose every element
// is an object. Beyond that, xray itself rejects malformed rules at
// startup — we don't try to schema-check field-by-field here.
func ValidateRoutingRules(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRoutingRules, err)
	}
	for i, r := range arr {
		t := strings.TrimSpace(string(r))
		if len(t) == 0 || t[0] != '{' {
			return fmt.Errorf("%w: rule[%d] is not a JSON object", ErrInvalidRoutingRules, i)
		}
		var obj map[string]any
		if err := json.Unmarshal(r, &obj); err != nil {
			return fmt.Errorf("%w: rule[%d]: %v", ErrInvalidRoutingRules, i, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Add inserts c, allocating a SocksPort if c.SocksPort == 0. Returns
// the stored row (with allocated ID and Port). Enforces uniqueness on
// both Name and SocksPort.
func (s *Store) Add(ctx context.Context, c Config) (Config, error) {
	if err := validate(&c); err != nil {
		return Config{}, err
	}
	c.ID = 0
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now

	if c.SocksPort == 0 {
		used, err := s.usedPorts(ctx)
		if err != nil {
			return Config{}, err
		}
		port, err := nextFreePort(used)
		if err != nil {
			return Config{}, err
		}
		c.SocksPort = port
	}

	if err := s.db.WithContext(ctx).Create(&c).Error; err != nil {
		if isUniqueErr(err) {
			return Config{}, ErrDuplicate
		}
		return Config{}, fmt.Errorf("insert xray config: %w", err)
	}
	return c, nil
}

// Update overwrites name + outbound + enabled. SocksPort stays put
// (changing it would move active connections to a new port), and the
// caller can't change CreatedAt.
func (s *Store) Update(ctx context.Context, c Config) error {
	if err := validate(&c); err != nil {
		return err
	}
	c.UpdatedAt = time.Now().UTC()

	fields := map[string]any{
		"name":       c.Name,
		"outbound":   c.Outbound,
		"enabled":    c.Enabled,
		"dialer":     c.Dialer,
		"updated_at": c.UpdatedAt,
	}

	res := s.db.WithContext(ctx).Model(&Config{}).
		Where("id = ?", c.ID).
		Updates(fields)
	if res.Error != nil {
		if isUniqueErr(res.Error) {
			return ErrDuplicate
		}
		return fmt.Errorf("update xray config: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	res := s.db.WithContext(ctx).Delete(&Config{}, id)
	if res.Error != nil {
		return fmt.Errorf("delete xray config: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id int64) (Config, error) {
	var c Config
	err := s.db.WithContext(ctx).First(&c, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Config{}, ErrNotFound
	}
	return c, err
}

func (s *Store) GetByName(ctx context.Context, name string) (Config, error) {
	n := normalizeName(name)
	if n == "" {
		return Config{}, ErrNotFound
	}
	var c Config
	err := s.db.WithContext(ctx).First(&c, "name = ?", n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Config{}, ErrNotFound
	}
	return c, err
}

func (s *Store) List(ctx context.Context) ([]Config, error) {
	var out []Config
	err := s.db.WithContext(ctx).Order("name ASC").Find(&out).Error
	return out, err
}

// NamesExist reports which requested names are missing from the store
// (same shape as proxy.Store.NamesExist), used for rule-time validation
// so xray:NAME references can be rejected at write time.
func (s *Store) NamesExist(ctx context.Context, names []string) (missing []string, err error) {
	if len(names) == 0 {
		return nil, nil
	}
	canon := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		n = normalizeName(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		canon = append(canon, n)
	}
	var rows []Config
	if err := s.db.WithContext(ctx).Select("name").
		Where("name IN ?", canon).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("lookup xray configs: %w", err)
	}
	have := make(map[string]bool, len(rows))
	for _, r := range rows {
		have[r.Name] = true
	}
	for _, n := range canon {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

func (s *Store) usedPorts(ctx context.Context) (map[int]bool, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	used := make(map[int]bool, len(list))
	for _, c := range list {
		used[c.SocksPort] = true
	}
	return used, nil
}

func validate(c *Config) error {
	c.Name = normalizeName(c.Name)
	if !ValidName(c.Name) {
		return ErrInvalidName
	}
	c.Outbound = strings.TrimSpace(c.Outbound)
	rewritten, err := normalizeOutboundJSON(c.Outbound, OutboundTag(c.Name))
	if err != nil {
		return err
	}
	c.Outbound = rewritten

	refs, err := ParseDialer(c.Dialer)
	if err != nil {
		return err
	}
	for _, r := range refs {
		// A master dialing its own entry would route its transport through
		// itself — reject at the syntax layer. (Cross-entry cycles that
		// span two masters are caught daemon-side where the full graph is
		// visible.)
		if r.Kind == DialerKindXray && r.Name == c.Name {
			return ErrDialerCycle
		}
	}
	c.Dialer = FormatDialer(refs)
	return nil
}

// normalizeOutboundJSON validates raw and rewrites its "tag" to
// forceTag so the stored JSON always reflects the auto-managed value
// the supervisor uses at runtime. Surfacing the tag in the editor lets
// users see it (and stops them from filing bugs when their typed tag
// silently doesn't take effect) — Generate ultimately overwrites it
// anyway, so the user's value would be discarded. Returns the
// re-serialized indented JSON.
func normalizeOutboundJSON(raw, forceTag string) (string, error) {
	if raw == "" {
		return "", ErrInvalidOutbound
	}
	var ob map[string]any
	if err := json.Unmarshal([]byte(raw), &ob); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidOutbound, err)
	}
	proto, _ := ob["protocol"].(string)
	if strings.TrimSpace(proto) == "" {
		return "", ErrInvalidOutbound
	}
	ob["tag"] = forceTag
	out, err := json.MarshalIndent(ob, "", "  ")
	if err != nil {
		return "", fmt.Errorf("re-marshal outbound: %w", err)
	}
	return string(out), nil
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
