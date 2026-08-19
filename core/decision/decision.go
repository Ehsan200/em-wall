// Package decision evaluates a domain name against the rule set and
// returns what the DNS proxy should do with the query.
package decision

import (
	"context"
	"net"
	"sync/atomic"

	"github.com/ehsan/em-wall/core/rules"
)

type Outcome int

const (
	OutcomeAllow Outcome = iota // resolve normally, default route
	OutcomeBlock                // return NXDOMAIN
	OutcomeRoute                // resolve, install per-host routes via Interface
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAllow:
		return "allow"
	case OutcomeBlock:
		return "block"
	case OutcomeRoute:
		return "route"
	}
	return "unknown"
}

type Decision struct {
	Outcome   Outcome
	Interface string // only when OutcomeRoute
	RuleID    int64  // 0 if no rule matched
	Pattern   string // empty if no rule matched
}

// RuleSource is satisfied by *rules.Store. The engine takes the
// interface so tests can supply a static slice.
type RuleSource interface {
	List(ctx context.Context) ([]rules.Rule, error)
}

// InterfaceExpander resolves indirect Rule.Interface bindings into the
// literal form the routing layer understands. It is satisfied by
// *xray.Store (outbound sets), but the engine stays OS- and
// xray-agnostic: it only ever sees opaque strings.
//
// Expansions returns a rewrite map keyed by the FULL stored interface
// value; an interface absent from the map passes through unchanged.
//
// A binding that no longer resolves (deleted or disabled set) is simply
// left out of the map, so the raw reference survives into the routing
// layer. Nothing there recognizes it, so the rule fails closed
// (NXDOMAIN) instead of leaking its traffic out of the default route,
// and the unresolved name still shows up verbatim in the query log.
type InterfaceExpander interface {
	Expansions(ctx context.Context) (map[string]string, error)
}

// Engine decides outcomes for DNS queries. It caches the rule list
// in memory and is hot-path safe; call Reload after rule changes.
type Engine struct {
	src   RuleSource
	exp   InterfaceExpander
	cache atomic.Pointer[[]rules.Rule]
}

func New(src RuleSource) *Engine {
	e := &Engine{src: src}
	empty := []rules.Rule{}
	e.cache.Store(&empty)
	return e
}

// SetExpander installs an InterfaceExpander applied to every rule at
// Reload time. Call before the first Reload; passing nil disables
// expansion. Not safe to call concurrently with Reload.
func (e *Engine) SetExpander(exp InterfaceExpander) { e.exp = exp }

// Reload refreshes the cached rule list. Indirect interface bindings are
// resolved ONCE here rather than per query, so Decide stays a pure
// in-memory lookup and every downstream consumer of Decision.Interface
// (dnsproxy, proxytun, routing) only ever sees literal bindings.
func (e *Engine) Reload(ctx context.Context) error {
	list, err := e.src.List(ctx)
	if err != nil {
		return err
	}
	if e.exp != nil {
		rewrite, err := e.exp.Expansions(ctx)
		if err != nil {
			return err
		}
		for i := range list {
			if v, ok := rewrite[list[i].Interface]; ok {
				list[i].Interface = v
			}
		}
	}
	e.cache.Store(&list)
	return nil
}

func (e *Engine) Decide(name string) Decision {
	list := *e.cache.Load()
	r := rules.MostSpecific(list, name)
	if r == nil {
		return Decision{Outcome: OutcomeAllow}
	}
	switch r.Action {
	case rules.ActionBlock:
		return Decision{Outcome: OutcomeBlock, RuleID: r.ID, Pattern: r.Pattern}
	case rules.ActionAllow:
		return Decision{Outcome: OutcomeAllow, RuleID: r.ID, Pattern: r.Pattern}
	case rules.ActionRoute:
		return Decision{Outcome: OutcomeRoute, Interface: r.Interface, RuleID: r.ID, Pattern: r.Pattern}
	}
	return Decision{Outcome: OutcomeAllow}
}

// DecideIP evaluates a destination IP against the IP/CIDR rule set. It is
// the IP-layer counterpart to Decide: the netstack TCP/UDP handler calls
// it when a connection arrives for an IP that has no DNS-time mapping, to
// learn whether that IP should be routed through a proxy. Domain rules are
// ignored here (see rules.MostSpecificIP).
func (e *Engine) DecideIP(ip net.IP) Decision {
	list := *e.cache.Load()
	r := rules.MostSpecificIP(list, ip)
	if r == nil {
		return Decision{Outcome: OutcomeAllow}
	}
	switch r.Action {
	case rules.ActionBlock:
		return Decision{Outcome: OutcomeBlock, RuleID: r.ID, Pattern: r.Pattern}
	case rules.ActionAllow:
		return Decision{Outcome: OutcomeAllow, RuleID: r.ID, Pattern: r.Pattern}
	case rules.ActionRoute:
		return Decision{Outcome: OutcomeRoute, Interface: r.Interface, RuleID: r.ID, Pattern: r.Pattern}
	}
	return Decision{Outcome: OutcomeAllow}
}
