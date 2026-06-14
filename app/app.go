package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"app/internal/installer"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/version"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// releasesAPI is the GitHub Releases endpoint for this project.
const releasesAPI = "https://api.github.com/repos/ehsan200/em-wall/releases/latest"

// App is the Wails-bound surface. Every public method is callable from
// the Vue frontend via the generated `wailsjs/go/main/App` bindings.
//
// All real work happens in the daemon — App is a thin client that
// talks to it over the Unix socket.
type App struct {
	ctx        context.Context
	socketPath string
	pool       chan *ipc.Client // buffered channel used as a connection pool
}

// rpcPoolCap is the max number of idle daemon connections kept open.
// Concurrent frontend calls each take their own connection, so up to
// rpcPoolCap calls can be in-flight at the same time without queueing.
const rpcPoolCap = 4

func NewApp(socketPath string) *App {
	if socketPath == "" {
		socketPath = ipc.DefaultSocketPath
	}
	return &App{
		socketPath: socketPath,
		pool:       make(chan *ipc.Client, rpcPoolCap),
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// rpc takes a client from the pool or dials a fresh connection.
func (a *App) rpc() (*ipc.Client, error) {
	select {
	case c := <-a.pool:
		return c, nil
	default:
		c, err := ipc.Dial(a.socketPath)
		if err != nil {
			return nil, fmt.Errorf("daemon not reachable at %s: %w", a.socketPath, err)
		}
		return c, nil
	}
}

// putRPC returns a healthy client to the pool, or closes it if the pool is full.
func (a *App) putRPC(c *ipc.Client) {
	select {
	case a.pool <- c:
	default:
		_ = c.Close()
	}
}

// drainPool closes and discards all pooled connections (e.g. after daemon restart).
func (a *App) drainPool() {
	for {
		select {
		case c := <-a.pool:
			_ = c.Close()
		default:
			return
		}
	}
}

func (a *App) call(method string, params, result any) error {
	c, err := a.rpc()
	if err != nil {
		return err
	}
	if err := c.Call(method, params, result); err != nil {
		_ = c.Close()
		// One-shot reconnect on transport errors.
		c2, derr := ipc.Dial(a.socketPath)
		if derr != nil {
			return err
		}
		if err2 := c2.Call(method, params, result); err2 != nil {
			_ = c2.Close()
			return err2
		}
		a.putRPC(c2)
		return nil
	}
	a.putRPC(c)
	return nil
}

// ---- Public methods (bound to the frontend) ----

func (a *App) AppVersion() string {
	return version.Version
}

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

// PublicIP returns the public egress identity for the system default
// route — what sites see for traffic that matches no rule.
func (a *App) PublicIP() (ipc.ExitIPResult, error) {
	var out ipc.ExitIPResult
	err := a.call(ipc.MethodPublicIP, nil, &out)
	return out, err
}

// RuleExitIP probes the public egress identity for a single rule's
// interface (the IP/location its matched traffic appears to come from).
func (a *App) RuleExitIP(ruleID int64) (ipc.ExitIPResult, error) {
	var out ipc.ExitIPResult
	err := a.call(ipc.MethodRuleExitIP, ipc.RuleExitIPParams{RuleID: ruleID}, &out)
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

// BulkUpdateRules retargets every rule in ids to the same
// (action, iface) pair. Action is "block" or "route"; iface must be
// empty for block and non-empty for route. The daemon validates the
// combination once before touching any row, so a malformed call leaves
// the rule list unchanged.
func (a *App) BulkUpdateRules(ids []int64, action, iface string) (ipc.GroupsBulkResult, error) {
	var out ipc.GroupsBulkResult
	err := a.call(ipc.MethodRulesBulkUpdate, ipc.RulesBulkUpdateParams{
		IDs: ids, Action: action, Interface: iface,
	}, &out)
	return out, err
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

func (a *App) ClearLogs() error {
	return a.call(ipc.MethodLogsClear, nil, nil)
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

// ---- Proxies ----

func (a *App) ListProxies() ([]ipc.ProxyDTO, error) {
	var out []ipc.ProxyDTO
	err := a.call(ipc.MethodProxiesList, nil, &out)
	return out, err
}

func (a *App) AddProxy(name, protocol, host string, port int, username, password string) (ipc.ProxyDTO, error) {
	var out ipc.ProxyDTO
	err := a.call(ipc.MethodProxiesAdd, ipc.ProxiesAddParams{
		Name: name, Protocol: protocol, Host: host, Port: port,
		Username: username, Password: password,
	}, &out)
	return out, err
}

// UpdateProxy updates the stored proxy. Pass an empty password string to
// keep the existing password unchanged.
func (a *App) UpdateProxy(id int64, name, protocol, host string, port int, username, password string) error {
	return a.call(ipc.MethodProxiesUpdate, ipc.ProxiesUpdateParams{
		ID: id, Name: name, Protocol: protocol, Host: host, Port: port,
		Username: username, Password: password,
	}, nil)
}

func (a *App) DeleteProxy(id int64) error {
	return a.call(ipc.MethodProxiesDelete, ipc.ProxiesDeleteParams{ID: id}, nil)
}

func (a *App) TestProxy(id int64) (ipc.ProxiesTestResult, error) {
	var out ipc.ProxiesTestResult
	err := a.call(ipc.MethodProxiesTest, ipc.ProxiesTestParams{ID: id}, &out)
	return out, err
}

// ---- Xray ----
//
// Each entry is a single xray-core outbound (the user writes the JSON
// directly). The supervisor in the daemon assembles all enabled
// entries into one composite config and runs xray as a subprocess; a
// rule with Interface = "xray:NAME" routes traffic through entry NAME.

func (a *App) XrayStatus() (ipc.XrayStatus, error) {
	var out ipc.XrayStatus
	err := a.call(ipc.MethodXrayStatus, nil, &out)
	return out, err
}

func (a *App) ListXray() ([]ipc.XrayDTO, error) {
	var out []ipc.XrayDTO
	err := a.call(ipc.MethodXrayList, nil, &out)
	return out, err
}

func (a *App) AddXray(name, outbound string, enabled bool) (ipc.XrayDTO, error) {
	var out ipc.XrayDTO
	err := a.call(ipc.MethodXrayAdd, ipc.XrayAddParams{
		Name: name, Outbound: outbound, Enabled: enabled,
	}, &out)
	return out, err
}

func (a *App) UpdateXray(id int64, name, outbound string, enabled bool) error {
	return a.call(ipc.MethodXrayUpdate, ipc.XrayUpdateParams{
		ID: id, Name: name, Outbound: outbound, Enabled: enabled,
	}, nil)
}

func (a *App) DeleteXray(id int64) error {
	return a.call(ipc.MethodXrayDelete, ipc.XrayDeleteParams{ID: id}, nil)
}

func (a *App) SetXrayEnabled(id int64, enabled bool) error {
	return a.call(ipc.MethodXraySetEnabled, ipc.XraySetEnabledParams{ID: id, Enabled: enabled}, nil)
}

// XrayRouting returns the user's saved global routing-rules JSON
// (a JSON array; empty string when never set).
func (a *App) XrayRouting() (ipc.XrayRoutingResult, error) {
	var out ipc.XrayRoutingResult
	err := a.call(ipc.MethodXrayGetRouting, nil, &out)
	return out, err
}

// SetXrayRouting validates and persists rules (a JSON array). Pass ""
// or "[]" to clear. Triggers an xray reconcile/restart on success.
func (a *App) SetXrayRouting(rules string) error {
	return a.call(ipc.MethodXraySetRouting, ipc.XraySetRoutingParams{Rules: rules}, nil)
}

// ParseXrayLink converts a vless://, vmess://, trojan://, or ss://
// share URI into a name + outbound JSON the user can drop into an
// entry. Errors are surfaced verbatim to the UI.
func (a *App) ParseXrayLink(link string) (ipc.XrayParseLinkResult, error) {
	var out ipc.XrayParseLinkResult
	err := a.call(ipc.MethodXrayParseLink, ipc.XrayParseLinkParams{Link: link}, &out)
	return out, err
}

// TestXray dials Target through the entry's local SOCKS5 inbound to
// check reachability. target is "host:port"; pass "" to use the
// daemon default (1.1.1.1:443).
func (a *App) TestXray(id int64, target string) (ipc.XrayTestResult, error) {
	var out ipc.XrayTestResult
	err := a.call(ipc.MethodXrayTest, ipc.XrayTestParams{ID: id, Target: target}, &out)
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
	// Drop all pooled connections — the daemon (and its socket) are gone
	// now, so any cached connection would block on the next call.
	a.drainPool()
	return nil
}

// UpdateInfo is returned by CheckForUpdate.
type UpdateInfo struct {
	HasUpdate   bool   `json:"hasUpdate"`
	Version     string `json:"version"`     // latest tag, e.g. "v1.2.3"
	URL         string `json:"url"`         // GitHub release page
	DownloadURL string `json:"downloadUrl"` // .dmg asset, "" if none published
}

// CheckForUpdate fetches the latest GitHub release and compares it with
// the running version. Returns HasUpdate=false when running a dev build,
// when the fetch fails, or when already on the latest version.
func (a *App) CheckForUpdate() UpdateInfo {
	current := version.Version
	if current == "" || current == "dev" {
		return UpdateInfo{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return UpdateInfo{}
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "em-wall/"+current)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return UpdateInfo{}
	}
	defer resp.Body.Close()
	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return UpdateInfo{}
	}
	if !semverNewer(release.TagName, current) {
		return UpdateInfo{}
	}
	dmg := ""
	for _, a := range release.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".dmg") {
			dmg = a.BrowserDownloadURL
			break
		}
	}
	return UpdateInfo{HasUpdate: true, Version: release.TagName, URL: release.HTMLURL, DownloadURL: dmg}
}

// DownloadAndApplyUpdate downloads the release .dmg from downloadURL,
// then hands off to a detached swap script and quits the app so the new
// bundle can replace the running one in place. On success the app exits
// (this method does not return to the frontend); the new app relaunches
// itself. The bundled daemon is updated separately — the relaunched app
// reports a newer version than the running daemon, which trips the
// existing "reinstall daemon" banner.
//
// Returns an error only when the download or mount fails before handoff;
// in that case the running app is untouched. Emits "update:progress"
// (0..1) events during the download for the UI's progress bar.
func (a *App) DownloadAndApplyUpdate(downloadURL string) error {
	if downloadURL == "" {
		return fmt.Errorf("no downloadable build for this release — use View release to update manually")
	}
	if !installer.IsPackaged() {
		return fmt.Errorf("self-update only works in a packaged build")
	}
	// Verify we can locate the bundle before spending time on the download.
	if _, err := installer.AppBundlePath(); err != nil {
		return err
	}

	dmgPath, err := a.downloadDMG(downloadURL)
	if err != nil {
		return err
	}

	if err := installer.ApplyAppUpdate(a.ctx, dmgPath); err != nil {
		_ = os.Remove(dmgPath)
		return err
	}
	// Hand-off succeeded: the swap script is waiting on our PID. Quit so it
	// can replace the bundle and relaunch.
	runtime.Quit(a.ctx)
	return nil
}

// downloadDMG streams downloadURL to a temp file, emitting fractional
// progress events. Returns the path to the downloaded .dmg.
func (a *App) downloadDMG(downloadURL string) (string, error) {
	req, err := http.NewRequestWithContext(a.ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("update: request: %w", err)
	}
	req.Header.Set("User-Agent", "em-wall/"+version.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("update: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update: download: HTTP %d", resp.StatusCode)
	}

	f, err := os.CreateTemp("", "em-wall-update-*.dmg")
	if err != nil {
		return "", fmt.Errorf("update: temp file: %w", err)
	}
	pw := &progressWriter{ctx: a.ctx, total: resp.ContentLength}
	if _, err := io.Copy(io.MultiWriter(f, pw), resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("update: write: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("update: close: %w", err)
	}
	runtime.EventsEmit(a.ctx, "update:progress", 1.0)
	return f.Name(), nil
}

// progressWriter emits "update:progress" (fraction 0..1) as bytes flow.
// Throttled to whole-percent changes so the event stream stays light.
type progressWriter struct {
	ctx     context.Context
	total   int64
	written int64
	lastPct int
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.written += int64(n)
	if p.total > 0 {
		pct := int(float64(p.written) / float64(p.total) * 100)
		if pct != p.lastPct {
			p.lastPct = pct
			runtime.EventsEmit(p.ctx, "update:progress", float64(pct)/100)
		}
	}
	return n, nil
}

// semverNewer reports whether latest is a higher version than current.
// Both strings may optionally start with "v". Non-numeric segments are
// treated as 0, so malformed tags lose gracefully instead of crashing.
func semverNewer(latest, current string) bool {
	trim := func(s string) string { return strings.TrimPrefix(s, "v") }
	lp := strings.SplitN(trim(latest), ".", 3)
	cp := strings.SplitN(trim(current), ".", 3)
	for i := 0; i < 3; i++ {
		lv, cv := 0, 0
		if i < len(lp) {
			lv, _ = strconv.Atoi(lp[i])
		}
		if i < len(cp) {
			cv, _ = strconv.Atoi(cp[i])
		}
		if lv > cv {
			return true
		}
		if lv < cv {
			return false
		}
	}
	return false
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
		// Flush stale connections from the previous daemon instance before
		// each probe so we don't reuse a half-dead socket.
		a.drainPool()
		var out ipc.StatusResult
		if err := a.call(ipc.MethodStatus, nil, &out); err == nil {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}
