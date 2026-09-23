package main

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/ehsan/em-wall/core/ipc"
)

func (a *app) cmdHealth(args []string) int {
	fs := a.newFlagSet("health")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall health [--json]\n\nProxied-connection health over the daemon's rolling window:\nfailures by cause, setup time, per-upstream standing, xray restarts,\nparked pool nodes.\n")
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) > 0 {
		return a.usageErr("health takes no arguments")
	}

	var h ipc.HealthStatsDTO
	if err := a.call(ipc.MethodHealthStats, nil, &h); err != nil {
		return a.fail(err)
	}
	if a.json {
		if err := a.emitJSON(h); err != nil {
			return a.fail(err)
		}
		return exitOK
	}

	failed := 0
	causes := make([]string, 0, len(h.Failed))
	for c, n := range h.Failed {
		failed += n
		if n > 0 {
			causes = append(causes, c)
		}
	}
	sort.Slice(causes, func(i, j int) bool { return h.Failed[causes[i]] > h.Failed[causes[j]] })

	rows := [][]string{
		{"window", fmt.Sprintf("%d min", h.WindowSec/60)},
		{"connections", strconv.Itoa(h.Connections)},
		{"failed", fmt.Sprintf("%d (%s)", failed, percent(failed, h.Connections))},
	}
	for _, c := range causes {
		rows = append(rows, []string{"  " + c, strconv.Itoa(h.Failed[c])})
	}
	rows = append(rows,
		[]string{"setup p50 / p95", setupMs(h.SetupP50Ms) + " / " + setupMs(h.SetupP95Ms)},
		[]string{"extra attempts", strconv.Itoa(h.ExtraAttempts)},
		[]string{"udp flows silent", fmt.Sprintf("%d of %d (%s)", h.UDPSilent, h.UDPFlows, percent(h.UDPSilent, h.UDPFlows))},
		[]string{"xray restarts / live", fmt.Sprintf("%d / %d", h.XrayRestarts, h.XrayLiveApplies)},
		[]string{"parked nodes", strconv.Itoa(len(h.ParkedNodes))},
	)
	a.table([]string{"FIELD", "VALUE"}, rows)

	if len(h.Upstreams) > 0 {
		fmt.Fprintln(a.out)
		var ur [][]string
		for _, u := range h.Upstreams {
			state := "ok"
			if u.BreakerOpen {
				state = "demoted"
			}
			rtt := "-"
			if u.RTTMs > 0 {
				rtt = strconv.FormatInt(u.RTTMs, 10) + "ms"
			}
			ur = append(ur, []string{u.Name, strconv.Itoa(u.Connections), strconv.Itoa(u.Blamed),
				strconv.Itoa(u.NoData), setupMs(u.SetupP50Ms), rtt, state})
		}
		a.table([]string{"UPSTREAM", "CARRIED", "BLAMED", "NO-REPLY", "SETUP-P50", "RTT", "STATE"}, ur)
	}
	if len(h.ParkedNodes) > 0 {
		fmt.Fprintln(a.out)
		var pr [][]string
		for _, p := range h.ParkedNodes {
			pr = append(pr, []string{p.Name, p.Until})
		}
		a.table([]string{"PARKED NODE", "TRIAL AT"}, pr)
	}
	return exitOK
}

func percent(n, of int) string {
	if of == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(n)/float64(of))
}

// setupMs renders a histogram bin bound; -1 means no samples.
func setupMs(v int64) string {
	switch {
	case v < 0:
		return "-"
	case v > 20000:
		return ">20s"
	default:
		return "≤" + strconv.FormatInt(v, 10) + "ms"
	}
}
