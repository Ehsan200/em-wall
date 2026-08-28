package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/rules"
)

const groupUsage = `Usage: em-wall group <subcommand> [flags]

Subcommands:
  list                      list curated + custom groups
  add <name>                create a custom group
  edit <key>                edit a custom group
  rm <key>                  delete a custom group definition
  apply <key>               create rules from the group's patterns
  sync <key>                add only the patterns no rule covers yet
  enable <key>              enable every rule created from the group
  disable <key>             disable every rule created from the group

Keys of custom groups carry a "custom:" prefix; it is added for you when
you pass the bare name.
`

func (a *app) cmdGroup(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.errOut, groupUsage)
		return exitUsage
	}
	switch args[0] {
	case "list", "ls":
		return a.groupList(args[1:])
	case "add", "create":
		return a.groupAdd(args[1:])
	case "edit", "update":
		return a.groupEdit(args[1:])
	case "rm", "delete", "del":
		return a.groupDelete(args[1:])
	case "apply":
		return a.groupApply(args[1:])
	case "sync":
		return a.groupSync(args[1:])
	case "enable":
		return a.groupSetEnabled(args[1:], true)
	case "disable":
		return a.groupSetEnabled(args[1:], false)
	case "help", "-h", "--help":
		fmt.Fprint(a.errOut, groupUsage)
		return exitOK
	default:
		fmt.Fprintf(a.errOut, "em-wall: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(a.errOut, groupUsage)
		return exitUsage
	}
}

func (a *app) groupList(args []string) int {
	fs := a.newFlagSet("group list")
	customOnly := fs.Bool("custom", false, "only show user-created groups")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall group list [--custom] [--json]\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) > 0 {
		return a.usageErr("group list takes no arguments")
	}

	list, err := a.listGroups()
	if err != nil {
		return a.fail(err)
	}
	out := make([]ipc.GroupDTO, 0, len(list))
	for _, g := range list {
		if *customOnly && !g.Custom {
			continue
		}
		out = append(out, g)
	}

	if a.json {
		if err := a.emitJSON(out); err != nil {
			return a.fail(err)
		}
		return exitOK
	}
	rows := make([][]string, 0, len(out))
	for _, g := range out {
		missing := "-"
		if n := len(g.MissingPatterns); n > 0 {
			missing = "+" + strconv.Itoa(n)
		}
		rows = append(rows, []string{
			g.Key, g.DisplayName, g.Category, strconv.Itoa(len(g.Patterns)),
			strconv.Itoa(g.RuleCount), missing, yesNo(g.Custom),
		})
	}
	a.table([]string{"KEY", "NAME", "CATEGORY", "PATTERNS", "RULES", "NEW", "CUSTOM"}, rows)
	return exitOK
}

func (a *app) groupAdd(args []string) int {
	fs := a.newFlagSet("group add")
	var pats stringList
	fs.Var(&pats, "pattern", "pattern to include; repeatable, comma-separated allowed")
	fs.Var(&pats, "p", "shorthand for --pattern")
	fromFile := fs.String("from-file", "", "read patterns from a file, one per line (# comments allowed)")
	desc := fs.String("desc", "", "group description")
	color := fs.String("color", "", "accent colour, hex (e.g. #7c5cff)")
	key := fs.String("key", "", "explicit group key; derived from the name when omitted")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall group add <name> [-p pattern]... [--from-file F] [--desc D] [--color #hex] [--key K]\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) == 0 {
		return a.usageErr("group add requires a display name")
	}
	// Unquoted multi-word names are joined back together, so
	// `group add Work VPN` and `group add "Work VPN"` agree.
	name := strings.Join(pos, " ")

	if *fromFile != "" {
		filePats, err := readPatternFile(*fromFile)
		if err != nil {
			return a.fail(err)
		}
		pats = append(pats, filePats...)
	}
	if len(pats) == 0 {
		return a.usageErr("group add requires at least one pattern (-p or --from-file)")
	}

	var created ipc.GroupDTO
	err := a.call(ipc.MethodGroupsAdd, ipc.GroupsAddParams{
		Key:         *key,
		DisplayName: name,
		Description: *desc,
		Patterns:    pats,
		Color:       *color,
	}, &created)
	if err != nil {
		return a.fail(err)
	}
	if a.json {
		if err := a.emitJSON(created); err != nil {
			return a.fail(err)
		}
		return exitOK
	}
	fmt.Fprintf(a.out, "created group %s (%s) with %d pattern(s)\n",
		created.Key, created.DisplayName, len(created.Patterns))
	fmt.Fprintf(a.out, "apply it with: em-wall group apply %s --action block\n", created.Key)
	return exitOK
}

// groupEdit reads the group back first: groups.update replaces every
// field, so anything the user did not pass has to be re-sent as-is.
func (a *app) groupEdit(args []string) int {
	fs := a.newFlagSet("group edit")
	var set, add, del stringList
	fs.Var(&set, "pattern", "replace the pattern list with these; repeatable")
	fs.Var(&set, "p", "shorthand for --pattern")
	fs.Var(&add, "add-pattern", "add a pattern, keeping the existing ones; repeatable")
	fs.Var(&del, "rm-pattern", "remove a pattern; repeatable")
	fromFile := fs.String("from-file", "", "replace the pattern list with a file's contents, one per line")
	name := fs.String("name", "", "new display name")
	desc := fs.String("desc", "", "new description")
	color := fs.String("color", "", "new accent colour")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall group edit <key> [--name N] [--desc D] [--color C] [-p pat | --add-pattern pat | --rm-pattern pat]...\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) != 1 {
		return a.usageErr("group edit takes exactly one group key")
	}

	g, err := a.findGroup(pos[0])
	if err != nil {
		return a.fail(err)
	}
	if !g.Custom {
		return a.fail(fmt.Errorf("group %s is curated, not custom — it cannot be edited", g.Key))
	}

	patterns := append([]string(nil), g.Patterns...)
	if *fromFile != "" {
		filePats, err := readPatternFile(*fromFile)
		if err != nil {
			return a.fail(err)
		}
		set = append(set, filePats...)
	}
	if len(set) > 0 {
		patterns = set
	}
	patterns = mergePatterns(patterns, add, del)
	if len(patterns) == 0 {
		return a.usageErr("refusing to leave group %s with no patterns; use \"group rm\" instead", g.Key)
	}

	updated := ipc.GroupsUpdateParams{
		Key:         g.Key,
		DisplayName: pick(*name, g.DisplayName),
		Description: pick(*desc, g.Description),
		Patterns:    patterns,
		Color:       pick(*color, g.Color),
	}
	if err := a.call(ipc.MethodGroupsUpdate, updated, nil); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.out, "updated group %s: %d pattern(s)\n", g.Key, len(patterns))
	if g.RuleCount > 0 {
		fmt.Fprintf(a.out, "run \"em-wall group sync %s\" to create rules for the new patterns\n", g.Key)
	}
	return exitOK
}

func (a *app) groupDelete(args []string) int {
	fs := a.newFlagSet("group rm")
	deleteRules := fs.Bool("delete-rules", false, "also delete the rules created from this group")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall group rm <key> [--delete-rules]\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) != 1 {
		return a.usageErr("group rm takes exactly one group key")
	}
	g, err := a.findGroup(pos[0])
	if err != nil {
		return a.fail(err)
	}

	var res ipc.GroupsBulkResult
	if err := a.call(ipc.MethodGroupsDelete, ipc.GroupsDeleteParams{
		Key: g.Key, DeleteRules: *deleteRules,
	}, &res); err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.out, "deleted group %s", g.Key)
	if *deleteRules {
		fmt.Fprintf(a.out, " and %d rule(s)", res.Affected)
	}
	fmt.Fprintln(a.out)
	return exitOK
}

func (a *app) groupApply(args []string) int {
	fs := a.newFlagSet("group apply")
	action := fs.String("action", "block", "block, allow, or route")
	iface := fs.String("iface", "", "route target: utunN, proxy:NAME, xray:NAME, or xrayset:NAME")
	disabled := fs.Bool("disabled", false, "create the rules disabled")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall group apply <key> [--action block|allow|route] [--iface X] [--disabled]\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) != 1 {
		return a.usageErr("group apply takes exactly one group key")
	}
	g, err := a.findGroup(pos[0])
	if err != nil {
		return a.fail(err)
	}

	var res ipc.GroupsApplyResult
	err = a.call(ipc.MethodGroupsApply, ipc.GroupsApplyParams{
		Key:       g.Key,
		Action:    *action,
		Interface: *iface,
		Enabled:   !*disabled,
	}, &res)
	if err != nil {
		return a.fail(err)
	}
	return a.reportApply(g.Key, res)
}

func (a *app) groupSync(args []string) int {
	fs := a.newFlagSet("group sync")
	all := fs.Bool("all", false, "sync every applied group that has drifted")
	action := fs.String("action", "", "override the action of the created rules")
	iface := fs.String("iface", "", "override the interface of the created rules")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall group sync <key> | --all [--action A] [--iface X]\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}

	var keys []string
	switch {
	case *all && len(pos) > 0:
		return a.usageErr("pass either --all or a group key, not both")
	case *all:
		list, err := a.listGroups()
		if err != nil {
			return a.fail(err)
		}
		// Same selector as the UI's "Sync all": a non-empty
		// MissingPatterns already implies the daemon considers the group
		// applied. Note that "applied" means some stored rule matches one
		// of the group's patterns — a hand-written rule can therefore pull
		// an overlapping curated group into scope. The target list is
		// printed first so that is visible before rules appear.
		for _, g := range list {
			if len(g.MissingPatterns) > 0 {
				keys = append(keys, g.Key)
			}
		}
		if len(keys) == 0 {
			fmt.Fprintln(a.out, "nothing to sync")
			return exitOK
		}
		fmt.Fprintf(a.out, "syncing %d group(s): %s\n", len(keys), strings.Join(keys, ", "))
	case len(pos) == 1:
		g, err := a.findGroup(pos[0])
		if err != nil {
			return a.fail(err)
		}
		keys = []string{g.Key}
	default:
		return a.usageErr("group sync takes one group key, or --all")
	}

	for _, k := range keys {
		var res ipc.GroupsApplyResult
		err := a.call(ipc.MethodGroupsSync, ipc.GroupsSyncParams{
			Key: k, Action: *action, Interface: *iface,
		}, &res)
		if err != nil {
			return a.fail(fmt.Errorf("sync %s: %w", k, err))
		}
		if code := a.reportApply(k, res); code != exitOK {
			return code
		}
	}
	return exitOK
}

func (a *app) groupSetEnabled(args []string, enabled bool) int {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	fs := a.newFlagSet("group " + verb)
	fs.Usage = func() { fmt.Fprintf(a.errOut, "Usage: em-wall group %s <key>\n", verb) }
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) != 1 {
		return a.usageErr("group %s takes exactly one group key", verb)
	}
	g, err := a.findGroup(pos[0])
	if err != nil {
		return a.fail(err)
	}

	var res ipc.GroupsBulkResult
	if err := a.call(ipc.MethodGroupsSetEnabled, ipc.GroupsSetEnabledParams{
		Key: g.Key, Enabled: enabled,
	}, &res); err != nil {
		return a.fail(err)
	}
	if a.json {
		if err := a.emitJSON(res); err != nil {
			return a.fail(err)
		}
		return exitOK
	}
	fmt.Fprintf(a.out, "%sd %d rule(s) of group %s\n", verb, res.Affected, g.Key)
	return exitOK
}

// reportApply renders the shared result of groups.apply / groups.sync.
func (a *app) reportApply(key string, res ipc.GroupsApplyResult) int {
	if a.json {
		if err := a.emitJSON(res); err != nil {
			return a.fail(err)
		}
		return exitOK
	}
	fmt.Fprintf(a.out, "%s: created %d rule(s), skipped %d already covered\n",
		key, len(res.Created), len(res.Skipped))
	for _, r := range res.Created {
		fmt.Fprintf(a.out, "  %d  %s → %s%s\n", r.ID, r.Pattern, r.Action, ifaceSuffix(r.Interface))
	}
	return exitOK
}

func (a *app) listGroups() ([]ipc.GroupDTO, error) {
	var list []ipc.GroupDTO
	if err := a.call(ipc.MethodGroupsList, nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// findGroup resolves a user-typed key against the daemon's list. A bare
// name also matches a custom group, whose stored key carries the
// "custom:" prefix.
func (a *app) findGroup(key string) (ipc.GroupDTO, error) {
	list, err := a.listGroups()
	if err != nil {
		return ipc.GroupDTO{}, err
	}
	prefixed := rules.CustomGroupPrefix + key
	for _, g := range list {
		if strings.EqualFold(g.Key, key) || strings.EqualFold(g.Key, prefixed) {
			return g, nil
		}
	}
	return ipc.GroupDTO{}, fmt.Errorf("no group with key %q (try \"em-wall group list\")", key)
}

// mergePatterns applies the add/remove deltas, preserving order and
// dropping duplicates. Comparison is case-insensitive to match the
// daemon's pattern normalisation.
func mergePatterns(base, add, del []string) []string {
	removed := make(map[string]bool, len(del))
	for _, d := range del {
		removed[strings.ToLower(strings.TrimSpace(d))] = true
	}
	seen := make(map[string]bool, len(base)+len(add))
	out := make([]string, 0, len(base)+len(add))
	for _, p := range append(append([]string(nil), base...), add...) {
		k := strings.ToLower(strings.TrimSpace(p))
		if k == "" || removed[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	return out
}

// readPatternFile reads one pattern per line, ignoring blanks and
// #-comments so a group list can be kept in version control.
func readPatternFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read patterns: %w", err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read patterns: %w", err)
	}
	return out, nil
}

func pick(override, current string) string {
	if override != "" {
		return override
	}
	return current
}
