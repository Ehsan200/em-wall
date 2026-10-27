package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ehsan/em-wall/core/update"
	"github.com/ehsan/em-wall/core/version"
)

// The app tells the user about a new release only while its window is
// open. The daemon is always running, so it checks too and, when the app
// is closed, asks the logged-in user in a dialog whose "Open em-wall"
// button starts the app — whose own banner then offers the install. A
// notification can't do that: `display notification` has no click action
// and would be attributed to Script Editor.
const (
	updateFirstCheckDelay = 2 * time.Minute
	updateCheckInterval   = 6 * time.Hour
	// "Later" (or no answer) asks again after this long; "Open" never
	// asks again for that version.
	updatePromptSnooze = 24 * time.Hour
	// An unanswered dialog closes by itself after this long.
	updatePromptGiveUp = time.Hour

	settingUpdateNotify = "update_notify"
	settingUpdatePrompt = "update_prompt"

	appBundleName = "Em Wall.app"
)

// updateNotifier holds what the last check found, for Status.
type updateNotifier struct {
	latest atomic.Pointer[string]
}

func (u *updateNotifier) Latest() string {
	if p := u.latest.Load(); p != nil {
		return *p
	}
	return ""
}

// updatePromptState is the user's last answer, so one release is asked
// about once (or once per snooze).
type updatePromptState struct {
	Version string    `json:"version"`
	Answer  string    `json:"answer"` // "open" | "later" | "timeout"
	At      time.Time `json:"at"`
}

// safeTag guards what goes into the AppleScript source: a release tag
// comes from the network.
var safeTag = regexp.MustCompile(`^v?[0-9A-Za-z.+-]{1,32}$`)

func (d *handlerDeps) runUpdateNotifier(ctx context.Context) {
	if version.Version == "" || version.Version == "dev" {
		return // nothing to compare an unversioned build against
	}
	t := time.NewTimer(updateFirstCheckDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.checkForUpdate(ctx)
		t.Reset(updateCheckInterval)
	}
}

func (d *handlerDeps) checkForUpdate(ctx context.Context) {
	rel, err := update.Latest(ctx, version.Version)
	if err != nil {
		return // offline or rate-limited; try again next round
	}
	if !update.Newer(rel.Tag, version.Version) || !safeTag.MatchString(rel.Tag) {
		empty := ""
		d.updates.latest.Store(&empty)
		return
	}
	tag := rel.Tag
	d.updates.latest.Store(&tag)

	if on, _ := d.store.GetSetting(ctx, settingUpdateNotify, "true"); on == "false" {
		return
	}
	var st updatePromptState
	if raw, _ := d.store.GetSetting(ctx, settingUpdatePrompt, ""); raw != "" {
		_ = json.Unmarshal([]byte(raw), &st)
	}
	if st.Version == tag && (st.Answer == "open" || time.Since(st.At) < updatePromptSnooze) {
		return
	}
	if appRunning(ctx) {
		return // the app's own banner has it
	}
	u, err := consoleUser()
	if err != nil {
		return // nobody logged in
	}
	answer, err := promptUpdate(ctx, u, tag)
	if err != nil {
		log.Printf("em-walld: update prompt: %v", err)
		return
	}
	st = updatePromptState{Version: tag, Answer: answer, At: time.Now()}
	if raw, err := json.Marshal(st); err == nil {
		_ = d.store.SetSetting(ctx, settingUpdatePrompt, string(raw))
	}
	log.Printf("em-walld: update %s available — user answered %q", tag, answer)
	if answer == "open" {
		if err := openApp(ctx, u); err != nil {
			log.Printf("em-walld: open app for update: %v", err)
		}
	}
}

// appRunning reports whether the em-wall app is open for anyone.
func appRunning(ctx context.Context) bool {
	return exec.CommandContext(ctx, "/usr/bin/pgrep", "-f", appBundleName+"/Contents/MacOS/").Run() == nil
}

// consoleUser is the user at the screen. The owner of /dev/console is
// root at the login window, which means there is nobody to ask.
func consoleUser() (*user.User, error) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("stat /dev/console: no owner")
	}
	uid := fmt.Sprint(st.Uid)
	if uid == "0" {
		return nil, errors.New("no console user")
	}
	return user.LookupId(uid)
}

// asUser runs a command in u's GUI session: launchctl asuser puts it in
// their bootstrap namespace (so it can draw on screen), sudo drops root.
func asUser(ctx context.Context, u *user.User, name string, args ...string) *exec.Cmd {
	full := append([]string{"asuser", u.Uid, "/usr/bin/sudo", "-u", u.Username, name}, args...)
	return exec.CommandContext(ctx, "/bin/launchctl", full...)
}

// appPath finds the installed app, system-wide first.
func appPath(u *user.User) string {
	for _, p := range []string{
		filepath.Join("/Applications", appBundleName),
		filepath.Join(u.HomeDir, "Applications", appBundleName),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func promptUpdate(ctx context.Context, u *user.User, tag string) (string, error) {
	icon := "note"
	if p := appPath(u); p != "" {
		if icns := filepath.Join(p, "Contents", "Resources", "iconfile.icns"); fileExists(icns) {
			icon = fmt.Sprintf("(POSIX file %q)", icns)
		}
	}
	script := fmt.Sprintf(`set r to display dialog "em-wall %s is available." & return & return & "Open em-wall to download and install it." with title "em-wall" buttons {"Later", "Open em-wall"} default button "Open em-wall" with icon %s giving up after %d
if gave up of r then return "timeout"
if button returned of r is "Open em-wall" then return "open"
return "later"`, tag, icon, int(updatePromptGiveUp/time.Second))

	ctx, cancel := context.WithTimeout(ctx, updatePromptGiveUp+time.Minute)
	defer cancel()
	out, err := asUser(ctx, u, "/usr/bin/osascript", "-e", script).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("osascript: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	switch a := strings.TrimSpace(string(out)); a {
	case "open", "later", "timeout":
		return a, nil
	default:
		return "", fmt.Errorf("osascript: unexpected answer %q", a)
	}
}

func openApp(ctx context.Context, u *user.User) error {
	args := []string{"-a", strings.TrimSuffix(appBundleName, ".app")}
	if p := appPath(u); p != "" {
		args = []string{p}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := asUser(ctx, u, "/usr/bin/open", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
