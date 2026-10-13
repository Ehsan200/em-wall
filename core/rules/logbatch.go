package rules

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// LogBatch inserts entries in one transaction and trims the table to
// MaxLogEntries once, instead of an insert plus a trim per DNS query.
// Entries are expected oldest first; only the newest MaxLogEntries of a
// batch are written, since the rest would be trimmed straight away.
func (s *Store) LogBatch(ctx context.Context, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > MaxLogEntries {
		entries = entries[len(entries)-MaxLogEntries:]
	}
	now := time.Now().UTC()
	for i := range entries {
		entries[i].ID = 0
		if entries[i].Timestamp.IsZero() {
			entries[i].Timestamp = now
		}
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.CreateInBatches(entries, 100).Error; err != nil {
			return err
		}
		return tx.Exec(
			`DELETE FROM log_entries WHERE id < (
                SELECT id FROM log_entries ORDER BY id DESC LIMIT 1 OFFSET ?
            )`, MaxLogEntries-1,
		).Error
	})
}
