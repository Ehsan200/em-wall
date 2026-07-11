package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ehsan/em-wall/core/xray"
)

// xrayAPITimeout is the per-call timeout (seconds) handed to `xray api -t`.
const xrayAPITimeout = "5"

// apiServerAddr is the loopback address the running config's api inbound
// listens on. Only valid while a config with dialer slots is running.
func apiServerAddr() string { return "127.0.0.1:" + strconv.Itoa(xray.ApiPort) }

// runAPI shells `xray api <sub> --server=... -t=... [rest...]` and returns
// combined output. Callers may hold s.mu (the call is bounded by -t).
func (s *xraySupervisor) runAPI(ctx context.Context, sub string, rest ...string) ([]byte, error) {
	args := append([]string{"api", sub, "--server=" + apiServerAddr(), "-t=" + xrayAPITimeout}, rest...)
	cmd := exec.CommandContext(ctx, s.binaryPath, args...)
	cmd.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+s.dataDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("xray api %s: %w: %s", sub, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// SyncDialerMembers reconciles slot membership against the stores using
// live xray-api calls, so routine node churn (subscription refresh, node
// enable/disable, cap changes) never restarts xray or drops connections.
//
// When the *set* of slotted masters changes — a new master needs its slot
// inbound + balancer baked into the config — it falls back to a full
// Reconcile. No-op when disabled or nothing is running (the next Reconcile
// bakes a fresh config anyway).
//
// Caveat: s.mu is held across the `xray api` execs (each bounded by
// xrayAPITimeout), so a sync can briefly (≤ a few seconds) delay a
// concurrent Reconcile/Status. Acceptable given how infrequently membership
// changes (subscription refresh / manual); revisit if it ever contends.
func (s *xraySupervisor) SyncDialerMembers(ctx context.Context) error {
	s.mu.Lock()
	if !s.enabled || s.cmd == nil {
		s.mu.Unlock()
		return nil
	}
	entries, err := s.xrayStore.List(ctx)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	desired, err := s.resolveDialerSlots(ctx, entries)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if !sameSlotMasters(desired, s.loadedSlots) {
		s.mu.Unlock()
		return s.Reconcile(ctx)
	}
	err = s.applyMemberDeltaLocked(ctx, desired)
	s.mu.Unlock()
	return err
}

// applyMemberDeltaLocked adds/removes slot member outbounds so the running
// process matches desired. Caller holds s.mu and has verified the slot
// master-set is unchanged (member keys are content fingerprints, so a
// changed node yields a new key — a clean remove+add, never a same-key
// mutation). loadedSlots is advanced PER slot, so a mid-way failure still
// records the slots that converged and the next sync retries only the
// rest. Caller holds s.mu.
func (s *xraySupervisor) applyMemberDeltaLocked(ctx context.Context, desired []xray.DialerSlot) error {
	loadedByIdx := make(map[int]xray.DialerSlot, len(s.loadedSlots))
	for _, ls := range s.loadedSlots {
		loadedByIdx[ls.Index] = ls
	}
	// Rebuild loadedSlots from the (possibly partially) updated map on exit
	// so partial progress is never lost.
	defer func() {
		out := make([]xray.DialerSlot, 0, len(desired))
		for _, d := range desired {
			out = append(out, loadedByIdx[d.Index])
		}
		s.loadedSlots = out
	}()

	for _, d := range desired {
		want := membersByKey(d)
		have := memberKeySet(loadedByIdx[d.Index])

		// Removals: drop members no longer wanted. Tolerate errors (an
		// already-gone tag would fail rmo) and log — convergence matters
		// more than a strict result here; a structural Reconcile heals any
		// residual drift.
		var rmTags []string
		for k := range have {
			if _, keep := want[k]; !keep {
				rmTags = append(rmTags, xray.SlotMemberTag(d.Index, k))
			}
		}
		if len(rmTags) > 0 {
			if _, err := s.runAPI(ctx, "rmo", rmTags...); err != nil {
				s.logger.Printf("xray supervisor: rmo slot %d: %v (continuing)", d.Index, err)
			}
		}

		// Additions.
		var files []string
		for k, m := range want {
			if _, exists := have[k]; exists {
				continue
			}
			f, err := s.writeMemberFile(d.Index, k, m)
			if err != nil {
				return err
			}
			files = append(files, f)
		}
		if len(files) > 0 {
			_, err := s.runAPI(ctx, "ado", files...)
			for _, f := range files {
				_ = os.Remove(f)
			}
			if err != nil {
				// Leave this slot's loaded entry unchanged so the next sync
				// retries it; the deferred rebuild records slots done so far.
				return err
			}
		}
		loadedByIdx[d.Index] = d
	}
	return nil
}

// writeMemberFile writes a one-outbound `{"outbounds":[...]}` document for
// `xray api ado` and returns its path. The member's tag is set the same
// way Generate bakes it.
func (s *xraySupervisor) writeMemberFile(idx int, key string, m xray.DialerMember) (string, error) {
	ob, err := xray.MemberOutboundJSON(idx, key, m.Outbound)
	if err != nil {
		return "", err
	}
	doc, err := json.Marshal(map[string]any{"outbounds": []json.RawMessage{ob}})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.runtimeDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("ado-%d-%s.json", idx, safeFileKey(key))
	path := filepath.Join(s.runtimeDir, name)
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// BalancerInfoRaw returns the raw `xray api bi` output for the loaded slot
// balancers. NOTE: bi requires explicit balancer tags (a no-arg call
// errors), and its output is human-formatted TEXT (not JSON) exposing each
// balancer's current "Selects" (the live winner) — per-node RTT is not
// available via the CLI. The UI parses the winner tags out of the text.
// Empty when no slots run.
func (s *xraySupervisor) BalancerInfoRaw(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	running := s.enabled && s.cmd != nil && len(s.loadedSlots) > 0
	tags := make([]string, 0, len(s.loadedSlots))
	for _, sl := range s.loadedSlots {
		tags = append(tags, xray.SlotBalancerTag(sl.Index))
	}
	s.mu.Unlock()
	if !running || len(tags) == 0 {
		return nil, nil
	}
	return s.runAPI(ctx, "bi", tags...)
}

// sameSlotMasters reports whether two slot lists cover the same (index,
// master) set, ignoring member contents.
func sameSlotMasters(a, b []xray.DialerSlot) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[slotMasterKey(s)] = true
	}
	for _, s := range b {
		if !set[slotMasterKey(s)] {
			return false
		}
	}
	return true
}

func slotMasterKey(s xray.DialerSlot) string {
	return strconv.Itoa(s.Index) + ":" + strings.ToLower(s.Master)
}

func membersByKey(slot xray.DialerSlot) map[string]xray.DialerMember {
	m := make(map[string]xray.DialerMember, len(slot.Members))
	for _, mem := range slot.Members {
		m[mem.Key] = mem
	}
	return m
}

func memberKeySet(slot xray.DialerSlot) map[string]bool {
	m := make(map[string]bool, len(slot.Members))
	for _, mem := range slot.Members {
		m[mem.Key] = true
	}
	return m
}

// safeFileKey keeps a member key usable as a filename component.
func safeFileKey(key string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, key)
}
