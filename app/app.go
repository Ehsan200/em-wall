package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"app/internal/installer"

	"github.com/ehsan/em-wall/core/ipc"
)

// App is the Wails-bound surface. Every public method is callable from
// the Vue frontend via the generated `wailsjs/go/main/App` bindings.
//
// All real work happens in the daemon — App is a thin client that
// talks to it over the Unix socket.
type App struct {
	ctx        context.Context
	socketPath string

	mu     sync.Mutex
	client *ipc.Client
}

func NewApp(socketPath string) *App {
	if socketPath == "" {
		socketPath = ipc.DefaultSocketPath
	}
	return &App{socketPath: socketPath}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// rpc returns a connected client, dialing lazily and reconnecting if
// the daemon was restarted.
func (a *App) rpc() (*ipc.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	c, err := ipc.Dial(a.socketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon not reachable at %s: %w", a.socketPath, err)
	}
	a.client = c
	return c, nil
}

func (a *App) call(method string, params, result any) error {
	c, err := a.rpc()
	if err != nil {
		return err
	}
	if err := c.Call(method, params, result); err != nil {
		// One-shot reconnect on transport errors.
		a.mu.Lock()
		a.client = nil
		a.mu.Unlock()
		c2, derr := a.rpc()
		if derr != nil {
			return err
		}
		return c2.Call(method, params, result)
	}
	return nil
}

// ---- Public methods (bound to the frontend) ----

func (a *App) Status() (ipc.StatusResult, error) {
	var out ipc.StatusResult
	err := a.call(ipc.MethodStatus, nil, &out)
	return out, err
}

func (a *App) ListRules() ([]ipc.RuleDTO, error) {
	var out []ipc.RuleDTO
	err := a.call(ipc.MethodRulesList, nil, &out)
	return out, err
}

func (a *App) AddRule(pattern, action, iface string, enabled bool) (ipc.RuleDTO, error) {
	var out ipc.RuleDTO
	err := a.call(ipc.MethodRulesAdd, ipc.RulesAddParams{
		Pattern: pattern, Action: action, Interface: iface, Enabled: enabled,
	}, &out)
	return out, err
}

func (a *App) UpdateRule(id int64, pattern, action, iface string, enabled bool) error {
	return a.call(ipc.MethodRulesUpdate, ipc.RulesUpdateParams{
		ID: id, Pattern: pattern, Action: action, Interface: iface, Enabled: enabled,
	}, nil)
}

func (a *App) DeleteRule(id int64) error {
	return a.call(ipc.MethodRulesDelete, ipc.RulesDeleteParams{ID: id}, nil)
}

func (a *App) GetSetting(key, def string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := a.call(ipc.MethodSettingsGet, ipc.SettingsGetParams{Key: key, Default: def}, &out)
	return out.Value, err
}

func (a *App) SetSetting(key, value string) error {
	return a.call(ipc.MethodSettingsSet, ipc.SettingsSetParams{Key: key, Value: value}, nil)
}

func (a *App) RecentLogs(limit int, filter string) ([]ipc.LogDTO, error) {
	var out []ipc.LogDTO
	err := a.call(ipc.MethodLogsRecent, ipc.LogsRecentParams{Limit: limit, Filter: filter}, &out)
	return out, err
}

func (a *App) ActiveRoutes() ([]ipc.ActiveRouteDTO, error) {
	var out []ipc.ActiveRouteDTO
	err := a.call(ipc.MethodRoutesActive, nil, &out)
	return out, err
}

func (a *App) Interfaces() ([]ipc.InterfaceDTO, error) {
	var out []ipc.InterfaceDTO
	err := a.call(ipc.MethodInterfacesList, nil, &out)
	return out, err
}

func (a *App) SystemDNSStatus() (ipc.SystemDNSStatus, error) {
	var out ipc.SystemDNSStatus
	err := a.call(ipc.MethodSystemDNSStatus, nil, &out)
	return out, err
}

func (a *App) ActivateSystemDNS() (ipc.SystemDNSStatus, error) {
	var out ipc.SystemDNSStatus
	err := a.call(ipc.MethodSystemDNSActivate, nil, &out)
	return out, err
}

func (a *App) DeactivateSystemDNS() (ipc.SystemDNSStatus, error) {
	var out ipc.SystemDNSStatus
	err := a.call(ipc.MethodSystemDNSDeactivate, nil, &out)
	return out, err
}

func (a *App) SystemRoutes() ([]ipc.SystemRouteDTO, error) {
	var out []ipc.SystemRouteDTO
	err := a.call(ipc.MethodSystemRoutesList, nil, &out)
	return out, err
}

func (a *App) Apps() ([]ipc.AppDTO, error) {
	var out []ipc.AppDTO
	err := a.call(ipc.MethodAppsList, nil, &out)
	return out, err
}

func (a *App) AppIcon(key string) (ipc.AppIconDTO, error) {
	var out ipc.AppIconDTO
	err := a.call(ipc.MethodAppsIcon, ipc.AppsIconParams{Key: key}, &out)
	return out, err
}

func (a *App) Groups() ([]ipc.GroupDTO, error) {
	var out []ipc.GroupDTO
	err := a.call(ipc.MethodGroupsList, nil, &out)
	return out, err
}

func (a *App) ApplyGroup(key, action, iface string, enabled bool) (ipc.GroupsApplyResult, error) {
	var out ipc.GroupsApplyResult
	err := a.call(ipc.MethodGroupsApply, ipc.GroupsApplyParams{
		Key: key, Action: action, Interface: iface, Enabled: enabled,
	}, &out)
	return out, err
}

func (a *App) GroupIcon(key string) (ipc.GroupIconDTO, error) {
	var out ipc.GroupIconDTO
	err := a.call(ipc.MethodGroupsIcon, ipc.GroupsIconParams{Key: key}, &out)
	return out, err
}

// DeleteGroupRules deletes every rule whose pattern matches one of the
// group's canonical patterns. Hand-edited rules drop out of group
// membership and aren't touched.
func (a *App) DeleteGroupRules(key string) (ipc.GroupsBulkResult, error) {
	var out ipc.GroupsBulkResult
	err := a.call(ipc.MethodGroupsDeleteRules, ipc.GroupsDeleteRulesParams{Key: key}, &out)
	return out, err
}

// SetGroupEnabled flips enabled on every rule belonging to the group.
// Same matching rule as DeleteGroupRules.
func (a *App) SetGroupEnabled(key string, enabled bool) (ipc.GroupsBulkResult, error) {
	var out ipc.GroupsBulkResult
	err := a.call(ipc.MethodGroupsSetEnabled, ipc.GroupsSetEnabledParams{Key: key, Enabled: enabled}, &out)
	return out, err
}

// ---- Install / uninstall (local, no daemon needed) ----
//
// These methods don't go over IPC — they manipulate the host directly
// from the unprivileged UI process via osascript admin escalation. The
// frontend uses InstallStatus to gate the install panel and the
// uninstall section in Settings.

// InstallStatus reports what's on disk and whether the LaunchDaemon
// is running. Polled by the UI; cheap.
func (a *App) InstallStatus() installer.Status {
	return installer.Probe(a.ctx)
}

// IsPackaged is true when this app build embedded the daemon binary.
// Plain `wails dev` builds return false — the install panel hides
// itself in that case so devs aren't told to run an install that
// would fail.
func (a *App) IsPackaged() bool {
	return installer.IsPackaged()
}

// Install runs the privileged install script. Surfaces user-cancelled
// prompts as the literal string "cancelled" so the frontend can
// distinguish them from real errors.
func (a *App) Install() error {
	if err := installer.Install(a.ctx); err != nil {
		if installer.IsCancelled(err) {
			return fmt.Errorf("cancelled")
		}
		return err
	}
	return nil
}

// Uninstall runs the privileged uninstall script. Before tearing the
// daemon down, it asks the still-running daemon to restore the system
// DNS settings — otherwise removing the daemon while every network
// service has 127.0.0.1 as its resolver would brick DNS for the whole
// machine. Best-effort: if the daemon isn't reachable (e.g. already
// crashed), the uninstall proceeds anyway.
func (a *App) Uninstall(purge bool) error {
	var sys ipc.SystemDNSStatus
	if err := a.call(ipc.MethodSystemDNSStatus, nil, &sys); err == nil && sys.Active {
		var out ipc.SystemDNSStatus
		_ = a.call(ipc.MethodSystemDNSDeactivate, nil, &out)
	}

	if err := installer.Uninstall(a.ctx, purge); err != nil {
		if installer.IsCancelled(err) {
			return fmt.Errorf("cancelled")
		}
		return err
	}
	// Drop any cached IPC connection — the daemon (and its socket) are
	// gone now, and the next Status() call should fail cleanly rather
	// than blocking on a half-dead connection.
	a.mu.Lock()
	if a.client != nil {
		_ = a.client.Close()
		a.client = nil
	}
	a.mu.Unlock()
	return nil
}

// WaitForDaemon polls until the daemon answers Status() or the
// timeout elapses. Used by the UI right after Install completes — the
// LaunchDaemon takes a moment to come up and the user shouldn't see
// "daemon not reachable" in between.
func (a *App) WaitForDaemon(timeoutMs int) bool {
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		// Drop stale client; force fresh dial each tick.
		a.mu.Lock()
		if a.client != nil {
			_ = a.client.Close()
			a.client = nil
		}
		a.mu.Unlock()
		var out ipc.StatusResult
		if err := a.call(ipc.MethodStatus, nil, &out); err == nil {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}
