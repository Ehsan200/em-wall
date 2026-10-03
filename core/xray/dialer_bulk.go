package xray

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Bulk dialer edit modes. Replace overwrites the whole Dialer (empty
// clears it, turning masters back into plain entries); Add appends refs an
// entry doesn't already name; Remove drops the named refs and leaves the
// rest alone.
const (
	DialerEditReplace = "replace"
	DialerEditAdd     = "add"
	DialerEditRemove  = "remove"
)

// EditDialer applies one bulk edit to an entry's current Dialer and
// returns the new canonical value. current is the stored field (already
// canonical); an unparsable one is treated as empty rather than failing
// the whole batch, since Replace/Add then rewrite it to something valid.
func EditDialer(current, mode string, refs []DialerRef) (string, error) {
	cur, err := ParseDialer(current)
	if err != nil {
		cur = nil
	}
	key := func(r DialerRef) string { return r.Kind + ":" + r.Name }
	switch mode {
	case DialerEditReplace:
		return FormatDialer(dedupeRefs(refs)), nil
	case DialerEditAdd:
		return FormatDialer(dedupeRefs(append(cur, refs...))), nil
	case DialerEditRemove:
		drop := make(map[string]bool, len(refs))
		for _, r := range refs {
			drop[key(r)] = true
		}
		out := cur[:0:0]
		for _, r := range cur {
			if !drop[key(r)] {
				out = append(out, r)
			}
		}
		return FormatDialer(dedupeRefs(out)), nil
	}
	return "", fmt.Errorf("%w: unknown edit mode %q (want replace/add/remove)", ErrInvalidDialer, mode)
}

func dedupeRefs(refs []DialerRef) []DialerRef {
	seen := make(map[string]bool, len(refs))
	out := make([]DialerRef, 0, len(refs))
	for _, r := range refs {
		k := r.Kind + ":" + r.Name
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

// SetDialers writes the Dialer field of several entries atomically (all
// or none). Values must already be canonical and validated — the daemon
// checks existence and cycles against the whole proposed graph first.
// Only dialer + updated_at are touched, so origin fields and everything
// else on the row survive.
func (s *Store) SetDialers(ctx context.Context, dialers map[int64]string) error {
	if len(dialers) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for id, d := range dialers {
			res := tx.Model(&Config{}).Where("id = ?", id).
				Updates(map[string]any{"dialer": d, "updated_at": now})
			if res.Error != nil {
				return fmt.Errorf("update xray dialer: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				return ErrNotFound
			}
		}
		return nil
	})
}
