package main

import (
	"context"
	"encoding/json"
	"regexp"
)

// geoRoutingRe matches a geosite:/geoip: reference in routing JSON.
var geoRoutingRe = regexp.MustCompile(`"(geosite|geoip):`)

// repushGeoRouting makes a running xray use a just-replaced geosite.dat /
// geoip.dat. xray reads those files only while building routing rules, so
// it would otherwise keep the old lists until a restart. `xray api
// adrules` compiles the rules in the CLI process — which reads the
// current files from XRAY_LOCATION_ASSET (see runAPI) — and swaps the
// running routing for the result, exactly as a routing edit does; no
// connection is touched. TestGeoRepushRereadsDat checks this against the
// real binary.
//
// pushed is false (with no error) when there is nothing to do: xray not
// running (its next start reads the new files) or no rule references geo
// data. On an error the old lists stay until xray's next restart for any
// ordinary reason — a geo update never restarts it.
func (s *xraySupervisor) repushGeoRouting(ctx context.Context) (pushed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.cmd == nil || s.running == nil {
		return false, nil
	}
	var top struct {
		Routing json.RawMessage `json:"routing"`
	}
	if err := json.Unmarshal(s.running, &top); err != nil {
		return false, err
	}
	if !geoRoutingRe.Match(top.Routing) {
		return false, nil
	}
	if err := s.apiWithDoc(ctx, "adrules", map[string]any{"routing": top.Routing}); err != nil {
		return false, err
	}
	return true, nil
}
