// Package decision evaluates a domain name against the rule set and
// returns what the DNS proxy should do with the query.
package decision

import (
	"context"
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

// Engine decides outcomes for DNS queries. It caches the rule list
// in memory and is hot-path safe; call Reload after rule changes.
type Engine struct {
	src   RuleSource
	cache atomic.Pointer[[]rules.Rule]
}

func New(src RuleSource) *Engine {
	e := &Engine{src: src}
	empty := []rules.Rule{}
	e.cache.Store(&empty)
	return e
}

func (e *Engine) Reload(ctx context.Context) error {
	list, err := e.src.List(ctx)
	if err != nil {
		return err
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
		if r.Interface == "" {
			return Decision{Outcome: OutcomeAllow, RuleID: r.ID, Pattern: r.Pattern}
		}
		return Decision{Outcome: OutcomeRoute, Interface: r.Interface, RuleID: r.ID, Pattern: r.Pattern}
	}
	return Decision{Outcome: OutcomeAllow}
}
