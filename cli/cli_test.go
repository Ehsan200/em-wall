package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/version"
)

// stubHandler is a test daemon handler: it sees the raw params the CLI
// sent and returns whatever the test wants the daemon to answer.
type stubHandler func(params json.RawMessage) (any, error)

type recorded struct {
	method string
	params json.RawMessage
}

// stub is a fake daemon: a real ipc.Server on a temp socket, with
// per-method canned answers and a record of everything the CLI sent.
// It exercises the actual wire format rather than mocking it away.
type stub struct {
	sock string

	mu  sync.Mutex
	got []recorded
}

func newStub(t *testing.T, handlers map[string]stubHandler) *stub {
	t.Helper()
	s := &stub{sock: filepath.Join(t.TempDir(), "d.sock")}

	srv := ipc.NewServer(s.sock, log.New(io.Discard, "", 0))
	for method, h := range handlers {
		method, h := method, h
		srv.Handle(method, func(_ context.Context, params json.RawMessage) (any, error) {
			s.mu.Lock()
			s.got = append(s.got, recorded{method: method, params: params})
			s.mu.Unlock()
			return h(params)
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(srv.Shutdown)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := ipc.Dial(s.sock); err == nil {
			_ = c.Close()
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stub daemon socket %s never came up", s.sock)
	return nil
}

// params returns the raw params of the first call to method.
func (s *stub) params(t *testing.T, method string) json.RawMessage {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.got {
		if r.method == method {
			return r.params
		}
	}
	t.Fatalf("no call to %s recorded (saw %v)", method, s.methodsLocked())
	return nil
}

func (s *stub) methodsLocked() []string {
	out := make([]string, 0, len(s.got))
	for _, r := range s.got {
		out = append(out, r.method)
	}
	return out
}

func (s *stub) callCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.got {
		if r.method == method {
			n++
		}
	}
	return n
}

// runCLI drives run() against the stub socket and captures both streams.
func runCLI(sock string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	full := append([]string{"--socket", sock}, args...)
	code := run(full, &out, &errOut)
	return code, out.String(), errOut.String()
}

func decodeParams(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal params %s: %v", raw, err)
	}
}

func TestStatus(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodStatus: func(json.RawMessage) (any, error) {
			return ipc.StatusResult{
				Version: "1.2.3", Uptime: "5m0s", ListenAddr: "127.0.0.1:53",
				UpstreamDNS: "1.1.1.1", RuleCount: 7, BlockEncryptedDNS: true,
			}, nil
		},
	})

	code, out, _ := runCLI(s.sock, "status")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	for _, want := range []string{"1.2.3", "127.0.0.1:53", "1.1.1.1", "7", "yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusJSON(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodStatus: func(json.RawMessage) (any, error) {
			return ipc.StatusResult{Version: "1.2.3", RuleCount: 2}, nil
		},
	})

	code, out, _ := runCLI(s.sock, "status", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var got ipc.StatusResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Version != "1.2.3" || got.RuleCount != 2 {
		t.Errorf("got %+v", got)
	}
}

func TestRulesAdd(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodRulesAdd: func(json.RawMessage) (any, error) {
			return ipc.RuleDTO{ID: 42, Pattern: "*.openai.com", Action: "route", Interface: "xray:tokyo", Enabled: true}, nil
		},
	})

	code, out, errOut := runCLI(s.sock, "rules", "add", "*.openai.com", "--action", "route", "--iface", "xray:tokyo")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.RulesAddParams
	decodeParams(t, s.params(t, ipc.MethodRulesAdd), &p)
	if p.Pattern != "*.openai.com" || p.Action != "route" || p.Interface != "xray:tokyo" || !p.Enabled {
		t.Errorf("params = %+v", p)
	}
	if !strings.Contains(out, "added rule 42") {
		t.Errorf("output = %q", out)
	}
}

func TestRulesAddDisabled(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodRulesAdd: func(json.RawMessage) (any, error) { return ipc.RuleDTO{ID: 1}, nil },
	})

	if code, _, errOut := runCLI(s.sock, "rules", "add", "x.com", "--action", "block", "--disabled"); code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.RulesAddParams
	decodeParams(t, s.params(t, ipc.MethodRulesAdd), &p)
	if p.Enabled {
		t.Errorf("--disabled did not clear Enabled: %+v", p)
	}
}

func TestRulesAddRequiresAction(t *testing.T) {
	s := newStub(t, map[string]stubHandler{})
	code, _, errOut := runCLI(s.sock, "rules", "add", "x.com")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitUsage, errOut)
	}
	if s.callCount(ipc.MethodRulesAdd) != 0 {
		t.Errorf("daemon was called despite the usage error")
	}
}

func TestRulesListFilters(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodRulesList: func(json.RawMessage) (any, error) {
			return []ipc.RuleDTO{
				{ID: 1, Pattern: "ads.example.com", Action: "block", Enabled: true},
				{ID: 2, Pattern: "api.openai.com", Action: "route", Interface: "xray:tokyo", Enabled: true},
			}, nil
		},
	})

	code, out, _ := runCLI(s.sock, "rules", "list", "--action", "route")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "ads.example.com") {
		t.Errorf("--action route leaked a block rule:\n%s", out)
	}
	if !strings.Contains(out, "api.openai.com") {
		t.Errorf("--action route dropped the route rule:\n%s", out)
	}

	if _, out, _ = runCLI(s.sock, "rules", "list", "--match", "ads"); !strings.Contains(out, "ads.example.com") ||
		strings.Contains(out, "api.openai.com") {
		t.Errorf("--match filtered wrong:\n%s", out)
	}
}

// rules.update replaces the whole row, so disable must read the rule
// back and re-send pattern/action/interface unchanged.
func TestRulesDisablePreservesFields(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodRulesList: func(json.RawMessage) (any, error) {
			return []ipc.RuleDTO{{ID: 9, Pattern: "a.com", Action: "route", Interface: "proxy:home", Enabled: true}}, nil
		},
		ipc.MethodRulesUpdate: func(json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil },
	})

	code, _, errOut := runCLI(s.sock, "rules", "disable", "9")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.RulesUpdateParams
	decodeParams(t, s.params(t, ipc.MethodRulesUpdate), &p)
	if p.ID != 9 || p.Pattern != "a.com" || p.Action != "route" || p.Interface != "proxy:home" || p.Enabled {
		t.Errorf("params = %+v", p)
	}
}

func TestRulesEnableSkipsNoOp(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodRulesList: func(json.RawMessage) (any, error) {
			return []ipc.RuleDTO{{ID: 9, Pattern: "a.com", Action: "block", Enabled: true}}, nil
		},
	})

	code, out, _ := runCLI(s.sock, "rules", "enable", "9")
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if s.callCount(ipc.MethodRulesUpdate) != 0 {
		t.Errorf("already-enabled rule was still updated")
	}
	if !strings.Contains(out, "already enabled") {
		t.Errorf("output = %q", out)
	}
}

func TestGroupAdd(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsAdd: func(json.RawMessage) (any, error) {
			return ipc.GroupDTO{Key: "custom:work-vpn", DisplayName: "Work VPN", Patterns: []string{"a.com", "b.com", "c.com"}, Custom: true}, nil
		},
	})

	code, out, errOut := runCLI(s.sock, "group", "add", "Work", "VPN", "-p", "a.com,b.com", "-p", "c.com", "--desc", "office")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.GroupsAddParams
	decodeParams(t, s.params(t, ipc.MethodGroupsAdd), &p)
	if p.DisplayName != "Work VPN" {
		t.Errorf("display name = %q, want %q", p.DisplayName, "Work VPN")
	}
	if p.Description != "office" {
		t.Errorf("description = %q", p.Description)
	}
	if strings.Join(p.Patterns, ",") != "a.com,b.com,c.com" {
		t.Errorf("patterns = %v", p.Patterns)
	}
	if !strings.Contains(out, "custom:work-vpn") {
		t.Errorf("output = %q", out)
	}
}

func TestGroupAddFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patterns.txt")
	if err := writeFile(path, "# comment\n\n*.openai.com\napi.anthropic.com\n"); err != nil {
		t.Fatal(err)
	}
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsAdd: func(json.RawMessage) (any, error) { return ipc.GroupDTO{Key: "custom:ai"}, nil },
	})

	code, _, errOut := runCLI(s.sock, "group", "add", "AI", "--from-file", path)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.GroupsAddParams
	decodeParams(t, s.params(t, ipc.MethodGroupsAdd), &p)
	if strings.Join(p.Patterns, ",") != "*.openai.com,api.anthropic.com" {
		t.Errorf("patterns = %v (comments/blanks should be skipped)", p.Patterns)
	}
}

func TestGroupAddRequiresPatterns(t *testing.T) {
	s := newStub(t, map[string]stubHandler{})
	if code, _, _ := runCLI(s.sock, "group", "add", "Empty"); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if s.callCount(ipc.MethodGroupsAdd) != 0 {
		t.Errorf("daemon called with no patterns")
	}
}

// groups.update rewrites every field, so unchanged ones must be echoed
// back from groups.list rather than sent empty.
func TestGroupEditMergesPatterns(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsList: func(json.RawMessage) (any, error) {
			return []ipc.GroupDTO{{
				Key: "custom:ai", DisplayName: "AI", Description: "models",
				Color: "#7c5cff", Patterns: []string{"a.com", "b.com"}, Custom: true,
			}}, nil
		},
		ipc.MethodGroupsUpdate: func(json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil },
	})

	code, _, errOut := runCLI(s.sock, "group", "edit", "ai", "--add-pattern", "c.com", "--rm-pattern", "a.com")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.GroupsUpdateParams
	decodeParams(t, s.params(t, ipc.MethodGroupsUpdate), &p)
	if p.Key != "custom:ai" {
		t.Errorf("key = %q, want the prefixed key", p.Key)
	}
	if strings.Join(p.Patterns, ",") != "b.com,c.com" {
		t.Errorf("patterns = %v", p.Patterns)
	}
	if p.DisplayName != "AI" || p.Description != "models" || p.Color != "#7c5cff" {
		t.Errorf("unchanged fields were dropped: %+v", p)
	}
}

func TestGroupEditRefusesCurated(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsList: func(json.RawMessage) (any, error) {
			return []ipc.GroupDTO{{Key: "anthropic", DisplayName: "Anthropic", Patterns: []string{"a.com"}}}, nil
		},
	})

	code, _, errOut := runCLI(s.sock, "group", "edit", "anthropic", "--add-pattern", "x.com")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(errOut, "curated") {
		t.Errorf("stderr = %q", errOut)
	}
	if s.callCount(ipc.MethodGroupsUpdate) != 0 {
		t.Errorf("curated group was still updated")
	}
}

func TestGroupApplyResolvesBareKey(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsList: func(json.RawMessage) (any, error) {
			return []ipc.GroupDTO{{Key: "custom:ai", DisplayName: "AI", Patterns: []string{"a.com"}, Custom: true}}, nil
		},
		ipc.MethodGroupsApply: func(json.RawMessage) (any, error) {
			return ipc.GroupsApplyResult{
				Created: []ipc.RuleDTO{{ID: 3, Pattern: "a.com", Action: "route", Interface: "xray:tokyo"}},
				Skipped: []string{"b.com"},
			}, nil
		},
	})

	code, out, errOut := runCLI(s.sock, "group", "apply", "ai", "--action", "route", "--iface", "xray:tokyo")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.GroupsApplyParams
	decodeParams(t, s.params(t, ipc.MethodGroupsApply), &p)
	if p.Key != "custom:ai" {
		t.Errorf("key = %q, want custom:ai", p.Key)
	}
	if p.Action != "route" || p.Interface != "xray:tokyo" || !p.Enabled {
		t.Errorf("params = %+v", p)
	}
	if !strings.Contains(out, "created 1 rule(s), skipped 1") {
		t.Errorf("output = %q", out)
	}
}

func TestGroupApplyDefaultsToBlock(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsList: func(json.RawMessage) (any, error) {
			return []ipc.GroupDTO{{Key: "custom:ai", Patterns: []string{"a.com"}, Custom: true}}, nil
		},
		ipc.MethodGroupsApply: func(json.RawMessage) (any, error) { return ipc.GroupsApplyResult{}, nil },
	})

	if code, _, errOut := runCLI(s.sock, "group", "apply", "custom:ai"); code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	var p ipc.GroupsApplyParams
	decodeParams(t, s.params(t, ipc.MethodGroupsApply), &p)
	if p.Action != "block" || p.Interface != "" {
		t.Errorf("params = %+v, want a plain block", p)
	}
}

// The daemon reports MissingPatterns only for groups it considers
// applied (see missingGroupPatterns), so that field alone selects the
// sync targets — the same selector the UI's "Sync all" uses.
func TestGroupSyncAllSkipsCleanGroups(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsList: func(json.RawMessage) (any, error) {
			return []ipc.GroupDTO{
				{Key: "applied-clean", RuleCount: 4},
				{Key: "applied-drifted", RuleCount: 4, MissingPatterns: []string{"new.com"}},
				{Key: "never-applied", RuleCount: 0},
			}, nil
		},
		ipc.MethodGroupsSync: func(json.RawMessage) (any, error) {
			return ipc.GroupsApplyResult{Created: []ipc.RuleDTO{{ID: 1, Pattern: "new.com", Action: "block"}}}, nil
		},
	})

	code, _, errOut := runCLI(s.sock, "group", "sync", "--all")
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, errOut)
	}
	if n := s.callCount(ipc.MethodGroupsSync); n != 1 {
		t.Fatalf("sync called %d times, want 1 (only the drifted applied group)", n)
	}
	var p ipc.GroupsSyncParams
	decodeParams(t, s.params(t, ipc.MethodGroupsSync), &p)
	if p.Key != "applied-drifted" {
		t.Errorf("synced %q", p.Key)
	}
}

func TestGroupUnknownKey(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodGroupsList: func(json.RawMessage) (any, error) { return []ipc.GroupDTO{}, nil },
	})
	code, _, errOut := runCLI(s.sock, "group", "apply", "nope")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(errOut, "no group with key") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestDaemonErrorExitCode(t *testing.T) {
	s := newStub(t, map[string]stubHandler{
		ipc.MethodRulesAdd: func(json.RawMessage) (any, error) { return nil, errDaemon },
	})
	code, _, errOut := runCLI(s.sock, "rules", "add", "x.com", "--action", "route")
	if code != exitErr {
		t.Fatalf("exit = %d, want %d", code, exitErr)
	}
	if !strings.Contains(errOut, "route requires an interface") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestUnreachableDaemon(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.sock")
	code, _, errOut := runCLI(missing, "status")
	if code != exitUnreachable {
		t.Fatalf("exit = %d, want %d", code, exitUnreachable)
	}
	if !strings.Contains(errOut, "not reachable") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code, _, _ := runCLI("/nonexistent.sock", "frobnicate"); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
}

func TestMergePatterns(t *testing.T) {
	got := mergePatterns([]string{"a.com", "B.com"}, []string{"c.com", "a.com"}, []string{"b.com"})
	if strings.Join(got, ",") != "a.com,c.com" {
		t.Errorf("got %v, want [a.com c.com] (case-insensitive removal + dedup)", got)
	}
}

func TestVersionSkew(t *testing.T) {
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })

	version.Version = "dev"
	if w := versionSkew("1.2.3"); w != "" {
		t.Errorf("a dev CLI should not warn, got %q", w)
	}

	version.Version = "1.2.3"
	if w := versionSkew("dev"); w != "" {
		t.Errorf("a dev daemon should not warn, got %q", w)
	}
	if w := versionSkew("1.2.3"); w != "" {
		t.Errorf("matching versions should not warn, got %q", w)
	}
	if w := versionSkew("1.2.4"); !strings.Contains(w, "1.2.3") || !strings.Contains(w, "1.2.4") {
		t.Errorf("skew warning = %q, want both versions named", w)
	}
}

// errDaemon stands in for a rejection from the daemon's validation.
var errDaemon = errors.New("route requires an interface")

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
