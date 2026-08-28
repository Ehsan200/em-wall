package rules

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FakeIPLease persists one hostname → fake-IP binding.
//
// Without persistence the in-memory pool in core/dnsproxy restarts its
// allocation scan at offset 0 on every daemon start, so the same low
// addresses get handed out again — to whichever hostnames happen to query
// first. Any client still holding a fake IP cached from the previous run
// then reaches a slot that now belongs to a different hostname, and the
// netstack layer dutifully redials that other host through the proxy. The
// observed symptom is a client connecting to one service on another
// service's port (Spotify's access point on 4070 arriving as a CDN host).
//
// Host is the primary key; IP carries a unique index so the table can
// never describe two hostnames sharing one address.
type FakeIPLease struct {
	Host      string    `gorm:"primaryKey;column:host"`
	IP        string    `gorm:"not null;uniqueIndex;column:ip"`
	ExpiresAt time.Time `gorm:"not null;index;column:expires_at"`
}

func (FakeIPLease) TableName() string { return "fake_ip_leases" }

// ListFakeIPLeases returns every stored lease that has not expired as of
// now. Expired rows are left in place for PutFakeIPLease to overwrite;
// DeleteExpiredFakeIPLeases is the explicit reaper.
func (s *Store) ListFakeIPLeases(ctx context.Context, now time.Time) ([]FakeIPLease, error) {
	var out []FakeIPLease
	if err := s.db.WithContext(ctx).
		Where("expires_at > ?", now.UTC()).
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("list fake-ip leases: %w", err)
	}
	return out, nil
}

// PutFakeIPLease upserts a lease. Any other hostname currently recorded
// against the same IP is dropped first, in the same transaction, so a slot
// the pool reclaimed can't leave a stale row behind that would resurrect
// the old binding on the next restart.
func (s *Store) PutFakeIPLease(ctx context.Context, l FakeIPLease) error {
	if l.Host == "" || l.IP == "" {
		return fmt.Errorf("fake-ip lease: host and ip required")
	}
	l.ExpiresAt = l.ExpiresAt.UTC()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("ip = ? AND host <> ?", l.IP, l.Host).
			Delete(&FakeIPLease{}).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "host"}},
			DoUpdates: clause.AssignmentColumns([]string{"ip", "expires_at"}),
		}).Create(&l).Error
	})
	if err != nil {
		return fmt.Errorf("put fake-ip lease: %w", err)
	}
	return nil
}

// DeleteFakeIPLease drops the lease held by host, if any.
func (s *Store) DeleteFakeIPLease(ctx context.Context, host string) error {
	if err := s.db.WithContext(ctx).
		Where("host = ?", host).
		Delete(&FakeIPLease{}).Error; err != nil {
		return fmt.Errorf("delete fake-ip lease: %w", err)
	}
	return nil
}

// DeleteExpiredFakeIPLeases reaps rows whose lease ran out, keeping the
// table proportional to the set of hosts actually in use.
func (s *Store) DeleteExpiredFakeIPLeases(ctx context.Context, now time.Time) error {
	if err := s.db.WithContext(ctx).
		Where("expires_at <= ?", now.UTC()).
		Delete(&FakeIPLease{}).Error; err != nil {
		return fmt.Errorf("sweep fake-ip leases: %w", err)
	}
	return nil
}
