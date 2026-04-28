package main

import (
	"context"
	"fmt"
	"sync"

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
