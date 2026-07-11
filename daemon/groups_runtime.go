package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ehsan/em-wall/core/groups"
	"github.com/ehsan/em-wall/core/ipc"
	"github.com/ehsan/em-wall/core/proxy"
	"github.com/ehsan/em-wall/core/rules"
	"github.com/ehsan/em-wall/core/xray"
)

// resolveGroupPatterns returns the pattern list for a group key, looking in
// the curated registry (core/groups) for bare keys and the DB-backed custom
// store for "custom:"-prefixed keys. Returns an error if the key is unknown.
func (d *handlerDeps) resolveGroupPatterns(ctx context.Context, key string) ([]string, error) {
	if strings.HasPrefix(key, rules.CustomGroupPrefix) {
		g, err := d.store.GetCustomGroup(ctx, key)
		if err != nil {
			return nil, err
		}
		return g.Patterns, nil
	}
	g := groups.FindByKey(key)
	if g == nil {
		return nil, fmt.Errorf("unknown group: %s", key)
	}
	return g.Patterns, nil
}

// normalizeGroupPattern lowercases and trims a pattern so comparisons
// don't depend on user-typed whitespace or trailing dots.
func normalizeGroupPattern(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimSuffix(s, ".")))
}

// ruleCoveredByGroupPattern reports whether rulePat sits inside the
// scope of groupPat. Mirrors core/rules.Match's wildcard semantics so
// the same rules that *would* be hit by a group's wildcard at query
// time are also the ones treated as group members for delete-all /
// enable-all bulk actions.
//
//	group "*.openai.com" covers rules: openai.com, *.openai.com,
//	                                    api.openai.com, *.api.openai.com
//	group "openai.com"   covers only:  openai.com (exact match)
//
// This is what the user expects: "chatgpt.com" should be considered
// part of the OpenAI group because OpenAI lists "*.chatgpt.com".
func ruleCoveredByGroupPattern(rulePat, groupPat string) bool {
	rp := normalizeGroupPattern(rulePat)
	gp := normalizeGroupPattern(groupPat)
	if rp == "" || gp == "" {
		return false
	}
	if rp == gp {
		return true
	}
	if !strings.HasPrefix(gp, "*.") {
		return false // exact-only group pattern
	}
	suffix := gp[2:]
	body := rp
	if strings.HasPrefix(rp, "*.") {
		body = rp[2:]
	}
	return body == suffix || strings.HasSuffix(body, "."+suffix)
}

// ruleBelongsToGroup is the union over a group's pattern list.
func ruleBelongsToGroup(rulePat string, groupPats []string) bool {
	for _, gp := range groupPats {
		if ruleCoveredByGroupPattern(rulePat, gp) {
			return true
		}
	}
	return false
}

// matchingRuleIDs returns IDs of rules covered by any of patterns. Used
// by the bulk delete handler. Order is unspecified.
func (d *handlerDeps) matchingRuleIDs(ctx context.Context, patterns []string) ([]int64, error) {
	all, err := d.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range all {
		if ruleBelongsToGroup(r.Pattern, patterns) {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

func ruleToDTO(r rules.Rule) ipc.RuleDTO {
	return ipc.RuleDTO{
		ID:        r.ID,
		Pattern:   r.Pattern,
		Action:    string(r.Action),
		Interface: r.Interface,
		Enabled:   r.Enabled,
		CreatedAt: r.CreatedAt.Format(time.RFC3339),
		UpdatedAt: r.UpdatedAt.Format(time.RFC3339),
	}
}

func proxyToDTO(p proxy.Proxy) ipc.ProxyDTO {
	return ipc.ProxyDTO{
		ID:          p.ID,
		Name:        p.Name,
		Protocol:    string(p.Protocol),
		Host:        p.Host,
		Port:        p.Port,
		Username:    p.Username,
		HasPassword: p.Password != "",
		CreatedAt:   p.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   p.UpdatedAt.Format(time.RFC3339),
	}
}

func xrayToDTO(c xray.Config) ipc.XrayDTO {
	return ipc.XrayDTO{
		ID:        c.ID,
		Name:      c.Name,
		Outbound:  c.Outbound,
		SocksPort: c.SocksPort,
		Enabled:   c.Enabled,
		Dialer:    c.Dialer,
		CreatedAt: c.CreatedAt.Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.Format(time.RFC3339),
	}
}
