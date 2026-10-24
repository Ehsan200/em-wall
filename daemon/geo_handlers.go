package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ehsan/em-wall/core/geo"
	"github.com/ehsan/em-wall/core/geo/meta"
	"github.com/ehsan/em-wall/core/ipc"
)

// registerGeoHandlers registers the geo.* IPC methods. Contract: docs/geo-map-suggest.md.
func registerGeoHandlers(s *ipc.Server, d *handlerDeps) {
	s.Handle(ipc.MethodGeoStatus, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if d.geoDB == nil {
			return nil, errors.New("geo data is not configured")
		}
		return d.geoStatus(ctx), nil
	})

	// geo.update starts the fetch and returns at once with Updating set:
	// the city mmdb alone is ~130 MB, longer than any caller should hold
	// an IPC call. Callers poll geo.status until Updating is false; each
	// file's LastError then says how it went.
	s.Handle(ipc.MethodGeoUpdate, func(ctx context.Context, raw json.RawMessage) (any, error) {
		if d.geoDB == nil {
			return nil, errors.New("geo data is not configured")
		}
		var p ipc.GeoUpdateParams
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
		}
		keys := meta.Files
		if p.File != "" {
			if _, ok := meta.FileNames[p.File]; !ok {
				return nil, fmt.Errorf("unknown geo file %q (want geosite, geoip, city or asn)", p.File)
			}
			keys = []string{p.File}
		}
		st := d.geoUpd()
		rctx, cancel := context.WithTimeout(d.bgContext(), geoRunTimeout)
		if err := st.u.Start(rctx, keys, func(errs map[string]error) {
			cancel()
			if len(errs) > 0 {
				log.Printf("geo: update finished with %d error(s)", len(errs))
			}
		}); err != nil {
			cancel()
			if errors.Is(err, geo.ErrBusy) {
				return d.geoStatus(ctx), nil // already running: report it
			}
			return nil, err
		}
		return d.geoStatus(ctx), nil
	})

	s.Handle(ipc.MethodGeoRollback, func(ctx context.Context, _ json.RawMessage) (any, error) {
		if d.geoDB == nil {
			return nil, errors.New("geo data is not configured")
		}
		if err := d.geoUpd().u.Rollback(ctx); err != nil {
			return nil, err
		}
		return d.geoStatus(ctx), nil
	})
}

// geoStatus is geo.status: every file's state plus the settings.
func (d *handlerDeps) geoStatus(ctx context.Context) ipc.GeoStatusResult {
	st := d.geoUpd()
	srcKey, _ := d.store.GetSetting(ctx, settingGeoSource, geo.SourceLoyalsoldier)
	custom, _ := d.store.GetSetting(ctx, settingGeoCustomURL, "")
	via, _ := d.store.GetSetting(ctx, settingGeoFetchVia, "")
	auto := d.geoAutoUpdate(ctx)
	res := ipc.GeoStatusResult{
		Files:         []ipc.GeoFileDTO{},
		GeositeSource: srcKey,
		CustomURL:     custom,
		AutoUpdate:    auto,
		FetchVia:      via,
		Updating:      st.u.Busy(),
	}
	stats := d.geoDB.Stats()
	note := st.xrayNote.Load()
	for _, fs := range st.u.Status() {
		dto := ipc.GeoFileDTO{
			File:      fs.Key,
			Source:    fs.Entry.Source,
			Present:   fs.Present,
			Version:   fs.Entry.Version,
			UpdatedAt: geoTime(fs.Entry.UpdatedAt),
			SizeBytes: fs.Size,
			HasBackup: fs.HasBackup,
			LastError: fs.Entry.LastError,
		}
		if auto && !fs.NextDue.IsZero() {
			dto.NextDue = geoTime(fs.NextDue)
		}
		switch fs.Key {
		case meta.GeoSite:
			dto.Entries = stats.GeoSiteTags
		case meta.GeoIP:
			dto.Entries = stats.GeoIPTags
		case meta.City:
			dto.Entries = stats.CityNodes
		case meta.ASN:
			dto.Entries = stats.ASNNodes
		}
		if note != nil && dto.LastError == "" && (fs.Key == meta.GeoSite || fs.Key == meta.GeoIP) {
			dto.LastError = *note
		}
		res.Files = append(res.Files, dto)
	}
	return res
}

func geoTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
