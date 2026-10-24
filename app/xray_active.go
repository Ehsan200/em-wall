package main

import "github.com/ehsan/em-wall/core/ipc"

// XrayActive reports which pool node each master's slot balancer — and
// which outbound each entry's routing — xray picked over the last
// windowSec seconds (≤ 0 = 60), read by the daemon from xray's access log.
// history adds per-10s pick counts for the pool timeline.
func (a *App) XrayActive(windowSec int, history bool) (ipc.XrayActiveResult, error) {
	var out ipc.XrayActiveResult
	err := a.call(ipc.MethodXrayActive, ipc.XrayActiveParams{WindowSec: windowSec, History: history}, &out)
	return out, err
}
