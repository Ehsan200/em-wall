package main

import (
	"context"
	"time"

	"github.com/ehsan/em-wall/core/selfuse"
	"github.com/ehsan/em-wall/core/xray"
)

// selfHostsEvery is how often the suggester rebuilds em-wall's own
// hostnames from the stores (proxy / xray entries, subscriptions and their
// pool nodes change without telling it).
const selfHostsEvery = time.Minute

// loadSelfHosts is every hostname the daemon itself resolves: the static
// table in core/selfuse plus the custom geo source, subscription URL
// hosts, proxy and xray server hosts, and pool node hosts. A store that
// fails to read is skipped — a partial set only means a few more names
// count as the user's.
func (d *handlerDeps) loadSelfHosts(ctx context.Context) selfuse.Set {
	s := selfuse.NewSet(selfuse.Static())
	if d.store != nil {
		if v, err := d.store.GetSetting(ctx, settingGeoCustomURL, ""); err == nil {
			s.Add(v, selfuse.PurposeGeoData)
		}
	}
	if d.proxyStore != nil {
		if list, err := d.proxyStore.List(ctx); err == nil {
			for _, p := range list {
				s.Add(p.Host, selfuse.PurposeProxyServer)
			}
		}
	}
	if d.xrayStore == nil {
		return s
	}
	if list, err := d.xrayStore.List(ctx); err == nil {
		for _, e := range list {
			s.Add(xray.OutboundServerHost(e.Outbound), selfuse.PurposeProxyServer)
		}
	}
	subs, err := d.xrayStore.ListSubs(ctx)
	if err != nil {
		return s
	}
	for _, sub := range subs {
		s.Add(sub.URL, selfuse.PurposeSubscription)
		nodes, err := d.xrayStore.ListNodes(ctx, sub.ID)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			s.Add(xray.OutboundServerHost(n.Outbound), selfuse.PurposeProxyServer)
		}
	}
	return s
}
