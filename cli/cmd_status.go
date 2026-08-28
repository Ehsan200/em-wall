package main

import (
	"fmt"
	"strconv"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/version"
)

func (a *app) cmdStatus(args []string) int {
	fs := a.newFlagSet("status")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall status [--json]\n\nDaemon health, version, upstream DNS and rule count.\n")
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) > 0 {
		return a.usageErr("status takes no arguments")
	}

	var st ipc.StatusResult
	if err := a.call(ipc.MethodStatus, nil, &st); err != nil {
		return a.fail(err)
	}
	if a.json {
		if err := a.emitJSON(st); err != nil {
			return a.fail(err)
		}
		return exitOK
	}

	a.table([]string{"FIELD", "VALUE"}, [][]string{
		{"version", st.Version},
		{"uptime", st.Uptime},
		{"listen", st.ListenAddr},
		{"upstream", st.UpstreamDNS},
		{"rules", strconv.Itoa(st.RuleCount)},
		{"block encrypted dns", yesNo(st.BlockEncryptedDNS)},
	})
	if w := versionSkew(st.Version); w != "" {
		fmt.Fprintln(a.errOut, w)
	}
	return exitOK
}

// versionSkew warns when the CLI and the daemon were built from
// different releases — the IPC surface can differ, so a mismatch is
// what "method not found" errors usually trace back to. A "dev" build
// on either side is a local build, where mismatch is expected.
func versionSkew(daemonVer string) string {
	if version.Version == "dev" || daemonVer == "dev" || daemonVer == version.Version {
		return ""
	}
	return fmt.Sprintf("warning: cli is %s but the daemon is %s — reinstall from the app to match",
		version.Version, daemonVer)
}
