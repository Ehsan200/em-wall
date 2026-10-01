package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
)

// poolStripWidth is the most columns a strip takes; a longer window is
// bucketed, each column showing the worst state in its bucket.
const poolStripWidth = 60

func (a *app) cmdHealthPools(args []string) int {
	fs := a.newFlagSet("health pools")
	window := fs.Duration("window", 10*time.Minute, "how far back to draw (the daemon keeps 30m)")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, `Usage: em-wall health pools [MASTER] [--window DUR] [--json]

Per-node health timeline of each master's subscription pool, sampled
every 10s. One strip per node, oldest left:

  █ up     ▓ some pings lost     X down     P parked     ? no pings yet
  (lighter glyphs ▄ ▒ _ = the node was ranked out, not carrying traffic)

The "uplink" row marks rounds where no node answered at all — the local
link, not the pool; those are left out of every number.
`)
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) > 1 {
		return a.usageErr("health pools takes at most one master name")
	}
	params := ipc.HealthPoolsParams{WindowSec: int(window.Seconds())}
	if len(pos) == 1 {
		params.Master = pos[0]
	}

	var pools []ipc.PoolTimelineDTO
	if err := a.call(ipc.MethodHealthPools, params, &pools); err != nil {
		return a.fail(err)
	}
	if a.json {
		if err := a.emitJSON(pools); err != nil {
			return a.fail(err)
		}
		return exitOK
	}
	if len(pools) == 0 {
		if params.Master != "" {
			fmt.Fprintf(a.out, "no pool for master %q (not a master, or xray not running)\n", params.Master)
		} else {
			fmt.Fprintln(a.out, "no master pools loaded")
		}
		return exitOK
	}
	color := a.colorOK()
	for i, p := range pools {
		if i > 0 {
			fmt.Fprintln(a.out)
		}
		a.drawPool(p, color)
	}
	return exitOK
}

func (a *app) drawPool(p ipc.PoolTimelineDTO, color bool) {
	n := len(p.Times)
	span := "-"
	if n > 0 {
		span = (time.Duration(p.Times[n-1]-p.Times[0]+int64(p.IntervalSec)) * time.Second).Round(time.Second).String()
	}
	fmt.Fprintf(a.out, "pool %s", p.Master)
	if len(p.Masters) > 1 {
		fmt.Fprintf(a.out, " (shared by %s)", strings.Join(p.Masters, ", "))
	}
	fmt.Fprintf(a.out, " · %s · pick losses %d · flapping %d/%d · churn %.0f%%\n",
		span, p.PickLosses, p.Flappers, len(p.Nodes), p.ChurnPct)
	if n == 0 {
		fmt.Fprintln(a.out, "  no samples yet")
		return
	}

	per := (n + poolStripWidth - 1) / poolStripWidth
	buckets := (n + per - 1) / per

	uplink := make([]byte, 0, buckets)
	lost := 0
	for b := 0; b < buckets; b++ {
		c := byte('.')
		for i := b * per; i < min((b+1)*per, n); i++ {
			if p.UplinkDown[i] {
				c = '!'
				lost++
			}
		}
		uplink = append(uplink, c)
	}

	nameW := len("uplink")
	for _, nd := range p.Nodes {
		nameW = max(nameW, len([]rune(nd.Name)))
	}
	nameW = min(nameW, 32)
	pad := strings.Repeat(" ", max(len("STRIP")-buckets, 0)) // keeps a short strip under its header

	fmt.Fprintf(a.out, "  %-*s  %s%s  %s\n", nameW, "uplink", string(uplink), pad, dimIf(color, fmt.Sprintf("%d of %d rounds dead", lost, n)))
	fmt.Fprintf(a.out, "  %-*s  %-*s  %5s %5s %6s %8s %8s\n", nameW, "NODE", buckets, "STRIP", "UP%", "FLIPS", "RTT", "SENT", "RECV")
	for _, nd := range p.Nodes {
		var strip strings.Builder
		for b := 0; b < buckets; b++ {
			state, carrying := bucketCell(nd, b*per, min((b+1)*per, n))
			strip.WriteString(cellGlyph(state, carrying, color))
		}
		rtt := "-"
		if nd.AvgRTTMs > 0 {
			rtt = strconv.Itoa(nd.AvgRTTMs) + "ms"
		}
		name := []rune(nd.Name)
		if len(name) > nameW {
			name = append(name[:nameW-1], '…')
		}
		fmt.Fprintf(a.out, "  %-*s  %s%s  %5.0f %5d %6s %8s %8s  %s\n", nameW, string(name), strip.String(), pad,
			nd.UptimePct, nd.Flips, rtt, humanBytes(nd.TotalUp), humanBytes(nd.TotalDown), roleLabel(nd.Role))
	}
}

// bucketCell folds samples [from,to) of one node into a column: the worst
// state seen, carrying if it carried traffic in any of them.
func bucketCell(nd ipc.PoolNodeTimelineDTO, from, to int) (byte, bool) {
	rank := map[byte]int{
		ipc.PoolCellAbsent: 0, ipc.PoolCellAlive: 1, ipc.PoolCellUnknown: 2,
		ipc.PoolCellFlaky: 3, ipc.PoolCellParked: 4, ipc.PoolCellDead: 5,
	}
	state, carrying := byte(ipc.PoolCellAbsent), false
	for i := from; i < to && i < len(nd.States); i++ {
		if s := nd.States[i]; rank[s] > rank[state] {
			state = s
		}
		if r := nd.Roles[i]; r == ipc.PoolRoleFallback || r == ipc.PoolRoleActive {
			carrying = true
		}
	}
	return state, carrying
}

func cellGlyph(state byte, carrying, color bool) string {
	var g, c string
	switch state {
	case ipc.PoolCellAlive:
		g, c = ifElse(carrying, "█", "▄"), "32"
	case ipc.PoolCellFlaky:
		g, c = ifElse(carrying, "▓", "▒"), "33"
	case ipc.PoolCellDead:
		g, c = ifElse(carrying, "X", "_"), "31"
	case ipc.PoolCellParked:
		g, c = "P", "90"
	case ipc.PoolCellUnknown:
		g, c = "?", "90"
	default:
		return " "
	}
	if !color {
		return g
	}
	return "\x1b[" + c + "m" + g + "\x1b[0m"
}

func ifElse(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func roleLabel(r string) string {
	switch r {
	case string(ipc.PoolRoleFallback):
		return "fallback"
	case string(ipc.PoolRoleActive):
		return "active"
	case string(ipc.PoolRoleIdle):
		return "idle"
	case string(ipc.PoolRoleUnranked):
		return "unranked"
	}
	return "gone"
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1<<10:
		return strconv.FormatInt(n, 10) + "B"
	case n < 1<<20:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
}

func dimIf(color bool, s string) string {
	if !color {
		return s
	}
	return "\x1b[90m" + s + "\x1b[0m"
}

// colorOK reports whether output is a terminal that wants colour.
func (a *app) colorOK() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := a.out.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
