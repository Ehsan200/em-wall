package xray

import (
	"context"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm/clause"
)

// SubExit is one exit IP observed through one subscription node
// (docs/xray-sub-exit.md). A node normally has one row; a node seen with
// two or more IPs inside the memory window is a rotating exit. Keyed by
// fingerprint, not by node row: SubNode rows are rebuilt on every fetch.
type SubExit struct {
	ID          int64     `gorm:"primaryKey;column:id"`
	SubID       int64     `gorm:"not null;uniqueIndex:idx_subexit;column:sub_id"`
	Fingerprint string    `gorm:"not null;uniqueIndex:idx_subexit;column:fingerprint"`
	IP          string    `gorm:"not null;uniqueIndex:idx_subexit;column:ip"`
	Country     string    `gorm:"not null;default:'';column:country"`
	FirstSeen   time.Time `gorm:"column:first_seen"`
	LastSeen    time.Time `gorm:"index;column:last_seen"`
}

func (SubExit) TableName() string { return "xray_sub_exits" }

// RecordSubExit notes that node fp of subscription subID left from ip at at.
func (s *Store) RecordSubExit(ctx context.Context, subID int64, fp, ip, country string, at time.Time) error {
	row := SubExit{SubID: subID, Fingerprint: fp, IP: ip, Country: country, FirstSeen: at, LastSeen: at}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "sub_id"}, {Name: "fingerprint"}, {Name: "ip"}},
		DoUpdates: clause.Assignments(map[string]any{"last_seen": at, "country": country}),
	}).Create(&row).Error
}

// PruneSubExits drops observations last seen before cutoff.
func (s *Store) PruneSubExits(ctx context.Context, cutoff time.Time) error {
	return s.db.WithContext(ctx).Where("last_seen < ?", cutoff).Delete(&SubExit{}).Error
}

// SubExits returns every remembered exit observation, grouped by
// subscription ID.
func (s *Store) SubExits(ctx context.Context) (map[int64][]SubExit, error) {
	var rows []SubExit
	if err := s.db.WithContext(ctx).Order("sub_id, fingerprint, ip").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[int64][]SubExit{}
	for _, r := range rows {
		out[r.SubID] = append(out[r.SubID], r)
	}
	return out, nil
}

// AckSubExits records ips as the exit set the user accepted for subID.
func (s *Store) AckSubExits(ctx context.Context, subID int64, ips []string) error {
	res := s.db.WithContext(ctx).Model(&Subscription{}).Where("id = ?", subID).
		Updates(map[string]any{"exit_ack": JoinExitIPs(ips), "exit_ack_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// JoinExitIPs renders an IP set in ExitAck's canonical form.
func JoinExitIPs(ips []string) string {
	seen := map[string]bool{}
	var out []string
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip != "" && !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// SplitExitIPs parses ExitAck into a set.
func SplitExitIPs(s string) map[string]bool {
	out := map[string]bool{}
	for _, ip := range strings.Split(s, ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			out[ip] = true
		}
	}
	return out
}
