package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/ehsan/em-wall/core/ipc"
)

// geoPollInterval / geoWaitLimit pace "geo update" while it waits for the
// daemon's background fetch. Vars so tests can shorten them.
var (
	geoPollInterval = time.Second
	geoWaitLimit    = 25 * time.Minute // daemon's own run timeout is 20 min
)

const geoUsage = `Usage: em-wall geo [status] [--json]
       em-wall geo update [FILE] [--no-wait] [--json]
       em-wall geo rollback [--json]

Offline geo data: geosite.dat / geoip.dat (xray rules, group suggestions)
and DB-IP Lite City / ASN (the world map).

  status           each file's source, version, age and next refresh
  update [FILE]    fetch now, due or not; FILE is geosite, geoip, city or asn
                   (default: all). Waits for the daemon to finish unless
                   --no-wait; exits 1 if any file failed.
  rollback         swap each file with its previous version (.bak)
`

func (a *app) cmdGeo(args []string) int {
	sub := "status"
	if len(args) > 0 && (args[0] == "status" || args[0] == "update" || args[0] == "rollback") {
		sub, args = args[0], args[1:]
	}
	fs := a.newFlagSet("geo " + sub)
	fs.Usage = func() { fmt.Fprint(a.errOut, geoUsage) }
	noWait := false
	if sub == "update" {
		fs.BoolVar(&noWait, "no-wait", false, "return once the update has started")
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}

	var st ipc.GeoStatusResult
	switch sub {
	case "status":
		if len(pos) > 0 {
			return a.usageErr("geo status takes no arguments (did you mean \"geo update %s\"?)", pos[0])
		}
		if err := a.call(ipc.MethodGeoStatus, nil, &st); err != nil {
			return a.fail(err)
		}
		return a.printGeo(st)

	case "update":
		if len(pos) > 1 {
			return a.usageErr("geo update takes at most one FILE")
		}
		file := ""
		if len(pos) == 1 {
			file = pos[0]
			switch file {
			case "geosite", "geoip", "city", "asn":
			default:
				return a.usageErr("unknown geo file %q (want geosite, geoip, city or asn)", file)
			}
		}
		if err := a.call(ipc.MethodGeoUpdate, ipc.GeoUpdateParams{File: file}, &st); err != nil {
			return a.fail(err)
		}
		if noWait {
			return a.printGeo(st)
		}
		deadline := time.Now().Add(geoWaitLimit)
		for st.Updating {
			if time.Now().After(deadline) {
				fmt.Fprintln(a.errOut, "em-wall: still updating; check later with \"em-wall geo status\"")
				return exitErr
			}
			time.Sleep(geoPollInterval)
			if err := a.call(ipc.MethodGeoStatus, nil, &st); err != nil {
				return a.fail(err)
			}
		}
		if code := a.printGeo(st); code != exitOK {
			return code
		}
		return geoUpdateExit(st, file)

	default: // rollback
		if len(pos) > 0 {
			return a.usageErr("geo rollback takes no arguments")
		}
		if err := a.call(ipc.MethodGeoRollback, nil, &st); err != nil {
			return a.fail(err)
		}
		return a.printGeo(st)
	}
}

// printGeo renders st: one row per file, then the settings.
func (a *app) printGeo(st ipc.GeoStatusResult) int {
	if a.json {
		if err := a.emitJSON(st); err != nil {
			return a.fail(err)
		}
	} else {
		rows := make([][]string, 0, len(st.Files))
		for _, f := range st.Files {
			present := "missing"
			if f.Present {
				present = geoBytes(f.SizeBytes)
			}
			rows = append(rows, []string{
				f.File, dash(f.Source), dash(f.Version), dash(geoShortTime(f.UpdatedAt)), present,
				geoEntries(f), yesNo(f.HasBackup), dash(geoShortTime(f.NextDue)), dash(f.LastError),
			})
		}
		a.table([]string{"FILE", "SOURCE", "VERSION", "UPDATED", "SIZE", "ENTRIES", "BACKUP", "NEXT", "ERROR"}, rows)
		fmt.Fprintln(a.out)
		via := st.FetchVia
		if via == "" {
			via = "direct only"
		}
		source := st.GeositeSource
		if source == "custom" && st.CustomURL != "" {
			source += " (" + st.CustomURL + ")"
		}
		a.table([]string{"SETTING", "VALUE"}, [][]string{
			{"source", dash(source)},
			{"auto-update", yesNo(st.AutoUpdate)},
			{"fetch via", via},
			{"updating", yesNo(st.Updating)},
		})
	}
	return exitOK
}

// geoUpdateExit is the exit code after an update: 1 when a file that was
// asked for (all of them when onlyFile is "") reports an error.
func geoUpdateExit(st ipc.GeoStatusResult, onlyFile string) int {
	for _, f := range st.Files {
		if (onlyFile == "" || f.File == onlyFile) && f.LastError != "" {
			return exitErr
		}
	}
	return exitOK
}

func geoEntries(f ipc.GeoFileDTO) string {
	if !f.Present || f.Entries == 0 {
		return "-"
	}
	unit := "tags"
	if f.File == "city" || f.File == "asn" {
		unit = "nodes"
	}
	return strconv.Itoa(f.Entries) + " " + unit
}

// geoShortTime renders an RFC 3339 timestamp as local "2006-01-02 15:04".
func geoShortTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

func geoBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}
