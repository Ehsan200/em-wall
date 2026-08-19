package xray

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// AddSet inserts s after validating and canonicalizing its members.
func (st *Store) AddSet(ctx context.Context, s Set) (Set, error) {
	if err := validateSet(&s); err != nil {
		return Set{}, err
	}
	s.ID = 0
	now := time.Now().UTC()
	s.CreatedAt = now
	s.UpdatedAt = now

	if err := st.db.WithContext(ctx).Create(&s).Error; err != nil {
		if isUniqueErr(err) {
			return Set{}, ErrDuplicate
		}
		return Set{}, fmt.Errorf("insert xray set: %w", err)
	}
	return s, nil
}

// UpdateSet overwrites name + members + enabled for the row with s.ID.
// Renaming is allowed; the caller is responsible for cascading the new
// name into rules that reference the old one (see
// rules.Store.RenameInterfaceRef with SetPrefix).
func (st *Store) UpdateSet(ctx context.Context, s Set) error {
	if err := validateSet(&s); err != nil {
		return err
	}
	s.UpdatedAt = time.Now().UTC()

	res := st.db.WithContext(ctx).Model(&Set{}).
		Where("id = ?", s.ID).
		Updates(map[string]any{
			"name":       s.Name,
			"members":    s.Members,
			"enabled":    s.Enabled,
			"updated_at": s.UpdatedAt,
		})
	if res.Error != nil {
		if isUniqueErr(res.Error) {
			return ErrDuplicate
		}
		return fmt.Errorf("update xray set: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (st *Store) DeleteSet(ctx context.Context, id int64) error {
	res := st.db.WithContext(ctx).Delete(&Set{}, id)
	if res.Error != nil {
		return fmt.Errorf("delete xray set: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (st *Store) GetSet(ctx context.Context, id int64) (Set, error) {
	var s Set
	err := st.db.WithContext(ctx).First(&s, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Set{}, ErrNotFound
	}
	return s, err
}

func (st *Store) GetSetByName(ctx context.Context, name string) (Set, error) {
	n := normalizeName(name)
	if n == "" {
		return Set{}, ErrNotFound
	}
	var s Set
	err := st.db.WithContext(ctx).First(&s, "name = ?", n).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Set{}, ErrNotFound
	}
	return s, err
}

func (st *Store) ListSets(ctx context.Context) ([]Set, error) {
	var out []Set
	err := st.db.WithContext(ctx).Order("name ASC").Find(&out).Error
	return out, err
}

// SetNamesExist reports which requested set names are missing, mirroring
// NamesExist so rule-time validation of "xrayset:NAME" reads the same.
func (st *Store) SetNamesExist(ctx context.Context, names []string) (missing []string, err error) {
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
	var rows []Set
	if err := st.db.WithContext(ctx).Select("name").
		Where("name IN ?", canon).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("lookup xray sets: %w", err)
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

// SetExpansions returns set name → literal interface string for every
// stored set, the map decision.Engine consumes at Reload time to rewrite
// "xrayset:NAME" rule bindings.
//
// A DISABLED set is deliberately absent from the map, so rules bound to
// it expand to "" and fail closed (NXDOMAIN) rather than silently
// leaking their traffic out of the default route. Same reasoning as a
// disabled xray entry making its rule un-dialable.
func (st *Store) SetExpansions(ctx context.Context) (map[string]string, error) {
	sets, err := st.ListSets(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(sets))
	for _, s := range sets {
		if !s.Enabled {
			continue
		}
		refs, err := ParseSetMembers(s.Members)
		if err != nil {
			// A row that no longer parses (hand-edited DB, format change)
			// must not take down every OTHER set's expansion. Leave it out;
			// its rules fail closed, exactly like a disabled set.
			continue
		}
		out[s.Name] = ExpandMembers(refs)
	}
	return out, nil
}

// Expansions makes *Store satisfy decision.InterfaceExpander: the same
// data as SetExpansions, but keyed by the FULL stored Rule.Interface
// value ("xrayset:NAME") so the engine can rewrite by plain map lookup
// without knowing what a set is.
func (st *Store) Expansions(ctx context.Context) (map[string]string, error) {
	byName, err := st.SetExpansions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(byName))
	for name, iface := range byName {
		out[SetPrefix+name] = iface
	}
	return out, nil
}

// SetsReferencing returns the names of sets that include the given typed
// ref as a member. Used to block deleting an xray entry or proxy that a
// set still depends on.
func (st *Store) SetsReferencing(ctx context.Context, kind, name string) ([]string, error) {
	sets, err := st.ListSets(ctx)
	if err != nil {
		return nil, err
	}
	want := DialerRef{Kind: kind, Name: normalizeName(name)}
	var out []string
	for _, s := range sets {
		refs, err := ParseSetMembers(s.Members)
		if err != nil {
			continue
		}
		for _, r := range refs {
			if r == want {
				out = append(out, s.Name)
				break
			}
		}
	}
	return out, nil
}

// RenameSetMember rewrites every set that references (kind, oldName) to
// point at newName instead, returning how many rows changed. Called when
// an xray entry or proxy is renamed so set membership survives, matching
// the rules.Store.RenameInterfaceRef cascade.
func (st *Store) RenameSetMember(ctx context.Context, kind, oldName, newName string) (int64, error) {
	sets, err := st.ListSets(ctx)
	if err != nil {
		return 0, err
	}
	from := DialerRef{Kind: kind, Name: normalizeName(oldName)}
	to := DialerRef{Kind: kind, Name: normalizeName(newName)}
	if from == to || !ValidName(to.Name) {
		return 0, nil
	}
	var changed int64
	for _, s := range sets {
		refs, err := ParseSetMembers(s.Members)
		if err != nil {
			continue
		}
		hit := false
		for i, r := range refs {
			if r == from {
				refs[i] = to
				hit = true
			}
		}
		if !hit {
			continue
		}
		// The rename can collide with a member that already names the new
		// target; re-parsing the formatted list drops the duplicate and
		// keeps the first position, same rule as ParseSetMembers.
		deduped, err := ParseSetMembers(FormatSetMembers(refs))
		if err != nil {
			continue
		}
		res := st.db.WithContext(ctx).Model(&Set{}).
			Where("id = ?", s.ID).
			Updates(map[string]any{
				"members":    FormatSetMembers(deduped),
				"updated_at": time.Now().UTC(),
			})
		if res.Error != nil {
			return changed, fmt.Errorf("rename set member: %w", res.Error)
		}
		changed += res.RowsAffected
	}
	return changed, nil
}

func validateSet(s *Set) error {
	s.Name = normalizeName(s.Name)
	if !ValidName(s.Name) {
		return ErrInvalidName
	}
	refs, err := ParseSetMembers(s.Members)
	if err != nil {
		return err
	}
	s.Members = FormatSetMembers(refs)
	return nil
}
