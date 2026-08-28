package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ehsan/em-wall/core/ipc"
)

const rulesUsage = `Usage: em-wall rules <subcommand> [flags]

Subcommands:
  list                      list stored rules
  add <pattern>             add a rule
  rm <id>...                delete rules
  enable <id>...            enable rules
  disable <id>...           disable rules
`

func (a *app) cmdRules(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.errOut, rulesUsage)
		return exitUsage
	}
	switch args[0] {
	case "list", "ls":
		return a.rulesList(args[1:])
	case "add":
		return a.rulesAdd(args[1:])
	case "rm", "delete", "del":
		return a.rulesDelete(args[1:])
	case "enable":
		return a.rulesSetEnabled(args[1:], true)
	case "disable":
		return a.rulesSetEnabled(args[1:], false)
	case "help", "-h", "--help":
		fmt.Fprint(a.errOut, rulesUsage)
		return exitOK
	default:
		fmt.Fprintf(a.errOut, "em-wall: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(a.errOut, rulesUsage)
		return exitUsage
	}
}

func (a *app) rulesList(args []string) int {
	fs := a.newFlagSet("rules list")
	action := fs.String("action", "", "only show rules with this action (block|allow|route)")
	match := fs.String("match", "", "only show rules whose pattern contains this substring")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall rules list [--action A] [--match SUBSTR] [--json]\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) > 0 {
		return a.usageErr("rules list takes no arguments")
	}

	list, err := a.listRules()
	if err != nil {
		return a.fail(err)
	}
	out := make([]ipc.RuleDTO, 0, len(list))
	for _, r := range list {
		if *action != "" && !strings.EqualFold(r.Action, *action) {
			continue
		}
		if *match != "" && !strings.Contains(r.Pattern, *match) {
			continue
		}
		out = append(out, r)
	}

	if a.json {
		if err := a.emitJSON(out); err != nil {
			return a.fail(err)
		}
		return exitOK
	}
	rows := make([][]string, 0, len(out))
	for _, r := range out {
		rows = append(rows, []string{
			strconv.FormatInt(r.ID, 10), r.Pattern, r.Action, dash(r.Interface), yesNo(r.Enabled),
		})
	}
	a.table([]string{"ID", "PATTERN", "ACTION", "INTERFACE", "ENABLED"}, rows)
	return exitOK
}

func (a *app) rulesAdd(args []string) int {
	fs := a.newFlagSet("rules add")
	action := fs.String("action", "", "block, allow, or route (required)")
	iface := fs.String("iface", "", "route target: utunN, proxy:NAME, xray:NAME, or xrayset:NAME")
	disabled := fs.Bool("disabled", false, "create the rule disabled")
	fs.Usage = func() {
		fmt.Fprint(a.errOut, "Usage: em-wall rules add <pattern> --action block|allow|route [--iface X] [--disabled]\n")
		fs.PrintDefaults()
	}
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	if len(pos) != 1 {
		return a.usageErr("rules add takes exactly one pattern")
	}
	if *action == "" {
		return a.usageErr("rules add requires --action")
	}

	var added ipc.RuleDTO
	err := a.call(ipc.MethodRulesAdd, ipc.RulesAddParams{
		Pattern:   pos[0],
		Action:    *action,
		Interface: *iface,
		Enabled:   !*disabled,
	}, &added)
	if err != nil {
		return a.fail(err)
	}
	if a.json {
		if err := a.emitJSON(added); err != nil {
			return a.fail(err)
		}
		return exitOK
	}
	fmt.Fprintf(a.out, "added rule %d: %s → %s%s\n", added.ID, added.Pattern, added.Action, ifaceSuffix(added.Interface))
	return exitOK
}

func (a *app) rulesDelete(args []string) int {
	fs := a.newFlagSet("rules rm")
	fs.Usage = func() { fmt.Fprint(a.errOut, "Usage: em-wall rules rm <id>...\n") }
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	ids, code := a.parseIDs(pos)
	if code != exitOK {
		return code
	}
	for _, id := range ids {
		if err := a.call(ipc.MethodRulesDelete, ipc.RulesDeleteParams{ID: id}, nil); err != nil {
			return a.fail(fmt.Errorf("delete rule %d: %w", id, err))
		}
		fmt.Fprintf(a.out, "deleted rule %d\n", id)
	}
	return exitOK
}

// rulesSetEnabled flips the enabled flag. rules.update replaces the
// whole row, so the current rule is read back first to preserve its
// pattern, action, and interface.
func (a *app) rulesSetEnabled(args []string, enabled bool) int {
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	fs := a.newFlagSet("rules " + verb)
	fs.Usage = func() { fmt.Fprintf(a.errOut, "Usage: em-wall rules %s <id>...\n", verb) }
	pos, code, done := parseFlags(fs, args)
	if done {
		return code
	}
	ids, code := a.parseIDs(pos)
	if code != exitOK {
		return code
	}

	list, err := a.listRules()
	if err != nil {
		return a.fail(err)
	}
	byID := make(map[int64]ipc.RuleDTO, len(list))
	for _, r := range list {
		byID[r.ID] = r
	}
	for _, id := range ids {
		r, ok := byID[id]
		if !ok {
			return a.fail(fmt.Errorf("no rule with id %d", id))
		}
		if r.Enabled == enabled {
			fmt.Fprintf(a.out, "rule %d already %sd\n", id, verb)
			continue
		}
		err := a.call(ipc.MethodRulesUpdate, ipc.RulesUpdateParams{
			ID:        r.ID,
			Pattern:   r.Pattern,
			Action:    r.Action,
			Interface: r.Interface,
			Enabled:   enabled,
		}, nil)
		if err != nil {
			return a.fail(fmt.Errorf("%s rule %d: %w", verb, id, err))
		}
		fmt.Fprintf(a.out, "%sd rule %d (%s)\n", verb, id, r.Pattern)
	}
	return exitOK
}

func (a *app) listRules() ([]ipc.RuleDTO, error) {
	var list []ipc.RuleDTO
	if err := a.call(ipc.MethodRulesList, nil, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (a *app) parseIDs(args []string) ([]int64, int) {
	if len(args) == 0 {
		return nil, a.usageErr("expected at least one rule id")
	}
	ids := make([]int64, 0, len(args))
	for _, s := range args {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, a.usageErr("%q is not a rule id", s)
		}
		ids = append(ids, id)
	}
	return ids, exitOK
}

func ifaceSuffix(iface string) string {
	if iface == "" {
		return ""
	}
	return " via " + iface
}
