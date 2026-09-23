package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Store persists proxies in the same SQLite file as the rules store.
// It opens its own *gorm.DB connection — SQLite in WAL mode handles
// concurrent readers and a single writer, so coexisting with rules.Store
// is fine.
type Store struct {
	db *gorm.DB

	// byName is an in-memory copy of every row keyed by canonical name.
	// GetByName sits on the hot path — every routed DNS query and every
	// upstream attempt of every proxied connection — and the connection
	// pool is a single SQLite conn, so without this every one of those
	// lookups queued behind the others (and behind every other writer of
	// this store). nil means "not loaded"; each write resets it and the
	// next read reloads. fillMu serialises reload against invalidation so
	// a reload that raced a write can never store the pre-write rows.
	byName atomic.Pointer[map[string]Proxy]
	fillMu sync.Mutex
}

// invalidate drops the name cache after a write.
func (s *Store) invalidate() {
	s.fillMu.Lock()
	s.byName.Store(nil)
	s.fillMu.Unlock()
}

// names returns the name cache, loading it from the DB when cold.
func (s *Store) names(ctx context.Context) (map[string]Proxy, error) {
	if m := s.byName.Load(); m != nil {
		return *m, nil
	}
	s.fillMu.Lock()
	defer s.fillMu.Unlock()
	if m := s.byName.Load(); m != nil {
		return *m, nil
	}
	var rows []Proxy
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	m := make(map[string]Proxy, len(rows))
	for _, p := range rows {
		m[p.Name] = p
	}
	s.byName.Store(&m)
	return m, nil
}

// Open opens the SQLite database at path and migrates the proxies
// table. The path should be the same as rules.Open() is given — the
// two stores share one file.
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

	if err := db.AutoMigrate(&Proxy{}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
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

func (s *Store) Add(ctx context.Context, p Proxy) (Proxy, error) {
	if err := validate(&p); err != nil {
		return Proxy{}, err
	}
	p.ID = 0
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now

	if err := s.db.WithContext(ctx).Create(&p).Error; err != nil {
		if isUniqueErr(err) {
			return Proxy{}, ErrDuplicate
		}
		return Proxy{}, fmt.Errorf("insert proxy: %w", err)
	}
	s.invalidate()
	return p, nil
}

// Update overwrites every mutable field. If p.Password is empty, the
// existing password is preserved so the UI can submit edits without
// re-typing the password every time.
func (s *Store) Update(ctx context.Context, p Proxy) error {
	if err := validate(&p); err != nil {
		return err
	}
	p.UpdatedAt = time.Now().UTC()

	fields := map[string]any{
		"name":       p.Name,
		"protocol":   string(p.Protocol),
		"host":       p.Host,
		"port":       p.Port,
		"username":   p.Username,
		"updated_at": p.UpdatedAt,
	}
	if p.Password != "" {
		fields["password"] = p.Password
	}

	res := s.db.WithContext(ctx).Model(&Proxy{}).
		Where("id = ?", p.ID).
		Updates(fields)
	s.invalidate()
	if res.Error != nil {
		if isUniqueErr(res.Error) {
			return ErrDuplicate
		}
		return fmt.Errorf("update proxy: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	res := s.db.WithContext(ctx).Delete(&Proxy{}, id)
	s.invalidate()
	if res.Error != nil {
		return fmt.Errorf("delete proxy: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id int64) (Proxy, error) {
	var p Proxy
	err := s.db.WithContext(ctx).First(&p, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Proxy{}, ErrNotFound
	}
	return p, err
}

// GetByName looks up a proxy by its canonical (lowercased) name.
// Returns ErrNotFound if no row matches.
func (s *Store) GetByName(ctx context.Context, name string) (Proxy, error) {
	n := normalizeName(name)
	if n == "" {
		return Proxy{}, ErrNotFound
	}
	m, err := s.names(ctx)
	if err != nil {
		return Proxy{}, err
	}
	p, ok := m[n]
	if !ok {
		return Proxy{}, ErrNotFound
	}
	return p, nil
}

func (s *Store) List(ctx context.Context) ([]Proxy, error) {
	var out []Proxy
	err := s.db.WithContext(ctx).Order("name ASC").Find(&out).Error
	return out, err
}

// NamesExist reports which names from the input list are missing from
// the store. The result is the missing names in the order they were
// given. Used for rule validation so a rule referencing an unknown
// proxy is rejected at write time rather than silently failing at
// query time.
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
	var rows []Proxy
	if err := s.db.WithContext(ctx).Select("name").
		Where("name IN ?", canon).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("lookup proxies: %w", err)
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

func validate(p *Proxy) error {
	p.Name = normalizeName(p.Name)
	if !ValidName(p.Name) {
		return ErrInvalidName
	}
	if !ValidProtocol(p.Protocol) {
		return ErrInvalidProtocol
	}
	p.Host = strings.TrimSpace(p.Host)
	if p.Host == "" {
		return ErrInvalidHost
	}
	if p.Port < 1 || p.Port > 65535 {
		return ErrInvalidPort
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
