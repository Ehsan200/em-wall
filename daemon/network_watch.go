package main

import (
	"bufio"
	"context"
	"log"
	"os/exec"
	"time"
)

// networkChangeKeys are the SCDynamicStore keys whose mutation means the
// host's network posture changed: the default route (Global/IPv4) and the
// resolver configuration (Global/DNS). A Wi-Fi switch, VPN toggle, or
// wake-from-sleep all rewrite at least one of these. scutil in n.watch mode
// prints a line whenever a watched key fires and is otherwise completely
// silent, so any output at all is our "network changed" signal — we never
// parse the line content, which keeps us independent of scutil's exact
// notification format.
var networkChangeKeys = []string{
	"State:/Network/Global/IPv4",
	"State:/Network/Global/DNS",
}

// watchNetworkChanges parks a scutil subprocess in notification-watch mode
// and invokes onChange (debounced) whenever a watched key mutates. It is
// event-driven: the kernel only wakes the process on an actual change, so it
// costs ~zero CPU while the network is idle. This is the primary trigger for
// re-picking upstream DNS after the user moves networks — instant, unlike
// the 10s poll in main which stays only as a safety net in case scutil dies
// or a change somehow slips past it.
//
// A single association emits a burst of key changes; we coalesce them with a
// short debounce so onChange fires once per settle rather than once per key.
// If scutil exits (its own crash, a configd restart) we relaunch it with a
// fixed backoff. Blocks until ctx is cancelled.
func watchNetworkChanges(ctx context.Context, onChange func()) {
	raw := make(chan struct{}, 16)

	// Producer: (re)launch scutil, forwarding each notification line as a
	// tick on raw. CommandContext ties the subprocess lifetime to ctx.
	go func() {
		for ctx.Err() == nil {
			if err := runScutilWatch(ctx, raw); err != nil && ctx.Err() == nil {
				log.Printf("em-walld: network watch scutil exited: %v (restarting)", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second): // backoff before relaunch
			}
		}
	}()

	// Consumer: debounce bursts, then fire onChange once per settle.
	const debounce = 1500 * time.Millisecond
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-raw:
			if timer == nil {
				timer = time.NewTimer(debounce)
				timerC = timer.C
			} else {
				timer.Reset(debounce)
			}
		case <-timerC:
			timer = nil
			timerC = nil
			onChange()
		}
	}
}

// runScutilWatch runs one scutil session that watches networkChangeKeys and
// pushes an empty struct onto ticks for every line scutil prints. It blocks
// until scutil exits or ctx is cancelled (which kills the subprocess).
func runScutilWatch(ctx context.Context, ticks chan<- struct{}) error {
	cmd := exec.CommandContext(ctx, "/usr/sbin/scutil")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	// Register the watches, then park. scutil reads these from stdin and
	// stays alive in n.watch until the process is killed (ctx cancel).
	for _, k := range networkChangeKeys {
		if _, err := stdin.Write([]byte("n.add " + k + "\n")); err != nil {
			break
		}
	}
	_, _ = stdin.Write([]byte("n.watch\n"))

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		select {
		case ticks <- struct{}{}:
		default: // consumer already mid-debounce; coalesce, drop the extra
		}
	}
	return cmd.Wait()
}
