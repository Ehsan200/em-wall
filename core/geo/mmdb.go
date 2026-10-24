package geo

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// DB-IP Lite City and ASN are GeoIP2-compatible mmdb files. Only the
// fields the map shows are decoded.

type cityRecord struct {
	City struct {
		Names struct {
			En string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"city"`
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
		Names   struct {
			En string `maxminddb:"en"`
		} `maxminddb:"names"`
	} `maxminddb:"country"`
	Location struct {
		Latitude  *float64 `maxminddb:"latitude"`
		Longitude *float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

type asnRecord struct {
	Number uint32 `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
}

// openMMDB opens path; a missing file is (nil, nil).
func openMMDB(path string) (*maxminddb.Reader, error) {
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// locateCity answers from the city mmdb. A record without coordinates
// falls back to the country centroid but still counts as the mmdb's answer.
func (d *data) locateCity(ip netip.Addr) (Location, bool) {
	if d.city == nil {
		return Location{}, false
	}
	var rec cityRecord
	res := d.city.Lookup(ip)
	if !res.Found() || res.Decode(&rec) != nil || rec.Country.ISOCode == "" {
		return Location{}, false
	}
	cc := rec.Country.ISOCode
	c := centroids()[cc]
	loc := Location{
		Country:     cc,
		CountryName: rec.Country.Names.En,
		City:        rec.City.Names.En,
		Precision:   PrecisionCity,
	}
	if loc.CountryName == "" {
		loc.CountryName = c.name
	}
	if rec.Location.Latitude != nil && rec.Location.Longitude != nil {
		loc.Lat, loc.Lon = *rec.Location.Latitude, *rec.Location.Longitude
	} else {
		loc.Lat, loc.Lon, loc.City, loc.Precision = c.lat, c.lon, "", PrecisionCountry
	}
	if loc.City == "" {
		loc.Precision = PrecisionCountry
	}
	return loc, true
}

func (d *data) lookupASN(ip netip.Addr) (uint32, string, bool) {
	if d.asn == nil {
		return 0, "", false
	}
	var rec asnRecord
	res := d.asn.Lookup(ip)
	if !res.Found() || res.Decode(&rec) != nil || rec.Number == 0 {
		return 0, "", false
	}
	return rec.Number, rec.Org, true
}

// closeGrace is how long a replaced snapshot's mmdb readers stay mapped:
// lookups take microseconds, so this is generous.
const closeGrace = 30 * time.Second

func closeLater(d *data) {
	time.Sleep(closeGrace)
	if d.city != nil {
		d.city.Close()
	}
	if d.asn != nil {
		d.asn.Close()
	}
}

// CheckMMDB opens path and looks up probe addresses; ok when the file
// parses and at least one probe answers with the field kind expects
// ("city": a country code, "asn": an AS number). Used by the updater to
// verify a download before it replaces the live file.
func CheckMMDB(path, kind string) (nodes int, err error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	probes := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "2606:4700:4700::1111"}
	for _, p := range probes {
		res := r.Lookup(netip.MustParseAddr(p))
		if !res.Found() {
			continue
		}
		switch kind {
		case "city":
			var rec cityRecord
			if res.Decode(&rec) == nil && rec.Country.ISOCode != "" {
				return int(r.Metadata.NodeCount), nil
			}
		case "asn":
			var rec asnRecord
			if res.Decode(&rec) == nil && rec.Number != 0 {
				return int(r.Metadata.NodeCount), nil
			}
		}
	}
	return 0, fmt.Errorf("geo: %s answers none of the probe addresses", kind)
}
