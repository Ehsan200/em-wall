// Command em-wall is the command-line client for the em-wall daemon.
//
// Every subcommand is a thin wrapper around one JSON-RPC call on the
// daemon's Unix socket — the same wire contract the Wails app speaks
// (core/ipc/protocol.go). No firewall logic lives here: the CLI never
// touches the SQLite store, routes, or pf directly.
//
// The socket is mode 0660 owned by group "staff", so a regular user
// can drive the CLI without sudo.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/version"
)

const (
	exitOK          = 0
	exitErr         = 1 // the daemon answered with an error
	exitUsage       = 2 // bad flags / arguments
	exitUnreachable = 3 // could not connect to the daemon socket
)

const usage = `em-wall — command-line client for the em-wall daemon

Usage:
  em-wall [global flags] <command> [args]

Commands:
  status                    daemon health, version, upstream, rule count
  rules list                list stored rules
  rules add <pattern>       add a rule
  rules rm <id>...          delete rules
  rules enable <id>...      enable rules
  rules disable <id>...     disable rules
  group list                list curated + custom groups
  group add <name>          create a custom group
  group edit <key>          edit a custom group
  group rm <key>            delete a custom group
  group apply <key>         create rules from a group's patterns
  group sync <key>          add only the patterns no rule covers yet
  group enable <key>        enable every rule created from a group
  group disable <key>       disable every rule created from a group
  version                   print the CLI version

Global flags (accepted before or after the command):
  --socket PATH   daemon IPC socket (default ` + ipc.DefaultSocketPath + `)
  --json          emit raw JSON instead of a table

Run "em-wall <command> -h" for command-specific flags.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// app carries the global flags and the output sinks. Tests construct
// one directly with buffers, which is why run() takes writers.
type app struct {
	socket string
	json   bool
	out    io.Writer
	errOut io.Writer
}

func run(args []string, out, errOut io.Writer) int {
	a := &app{socket: ipc.DefaultSocketPath, out: out, errOut: errOut}

	root := flag.NewFlagSet("em-wall", flag.ContinueOnError)
	root.SetOutput(errOut)
	root.Usage = func() { fmt.Fprint(errOut, usage) }
	a.bindGlobals(root)
	// Deliberately a plain Parse, not parseFlags: parsing must stop at
	// the command word so "rules add --action block" keeps --action for
	// the subcommand instead of the root FlagSet rejecting it.
	if code, done := classify(root.Parse(args)); done {
		return code
	}

	rest := root.Args()
	if len(rest) == 0 {
		root.Usage()
		return exitUsage
	}

	switch rest[0] {
	case "status":
		return a.cmdStatus(rest[1:])
	case "rules", "rule":
		return a.cmdRules(rest[1:])
	case "group", "groups":
		return a.cmdGroup(rest[1:])
	case "version":
		fmt.Fprintln(a.out, version.Version)
		return exitOK
	case "help", "-h", "--help":
		root.Usage()
		return exitOK
	default:
		fmt.Fprintf(errOut, "em-wall: unknown command %q\n\n", rest[0])
		root.Usage()
		return exitUsage
	}
}

// bindGlobals registers the global flags on fs. They are bound to every
// subcommand's FlagSet as well, so both "em-wall --json group list" and
// "em-wall group list --json" work.
func (a *app) bindGlobals(fs *flag.FlagSet) {
	fs.StringVar(&a.socket, "socket", a.socket, "path to the daemon IPC socket")
	fs.BoolVar(&a.json, "json", a.json, "emit raw JSON instead of a table")
}

// newFlagSet builds a subcommand FlagSet with the globals already bound.
func (a *app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	a.bindGlobals(fs)
	return fs
}

// parseFlags parses a subcommand's flags, allowing flags and positional
// arguments to be interspersed. Go's flag package stops at the first
// non-flag word, so "rules add x.com --action block" would otherwise
// leave --action sitting in the positional list; re-parsing what's left
// after each positional gets the ordering people expect from a CLI.
//
// done is true when the caller should return code immediately.
func parseFlags(fs *flag.FlagSet, args []string) (pos []string, code int, done bool) {
	for {
		if code, done := classify(fs.Parse(args)); done {
			return nil, code, true
		}
		if fs.NArg() == 0 {
			return pos, exitOK, false
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// classify maps a flag parse error onto an exit code. done is true when
// the caller should stop: -h printed the usage (success), anything else
// is a bad invocation.
func classify(err error) (int, bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, flag.ErrHelp):
		return exitOK, true
	default:
		return exitUsage, true
	}
}

// fail prints err and picks the matching exit code.
func (a *app) fail(err error) int {
	fmt.Fprintf(a.errOut, "em-wall: %v\n", err)
	if errors.Is(err, errUnreachable) {
		return exitUnreachable
	}
	return exitErr
}

// usageErr reports a bad invocation without the daemon being involved.
func (a *app) usageErr(format string, args ...any) int {
	fmt.Fprintf(a.errOut, "em-wall: "+format+"\n", args...)
	return exitUsage
}

// stringList is a repeatable string flag. Values are also comma-split,
// so -p a.com -p b.com and -p a.com,b.com are equivalent.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}
