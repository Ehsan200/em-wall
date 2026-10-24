package geo

import (
	_ "embed"
	"strconv"
	"strings"
	"sync"
)

// centroids.tsv is generated once from IANA's zone.tab / iso3166.tab: the
// first (main) timezone city of each country, or the geographic centroid
// for countries spanning several zones (US, RU, CA, BR, AU, CN, …). It is
// only what a country-precision dot is drawn at, so a representative
// point is all it needs to be.
//
//go:embed centroids.tsv
var centroidsTSV string

type centroid struct {
	name     string
	lat, lon float64
}

var (
	centroidOnce sync.Once
	centroidMap  map[string]centroid
)

func centroids() map[string]centroid {
	centroidOnce.Do(func() {
		centroidMap = make(map[string]centroid, 256)
		for _, line := range strings.Split(centroidsTSV, "\n") {
			if line == "" || line[0] == '#' {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) != 4 {
				continue
			}
			lat, err1 := strconv.ParseFloat(f[2], 64)
			lon, err2 := strconv.ParseFloat(f[3], 64)
			if err1 != nil || err2 != nil {
				continue
			}
			centroidMap[f[0]] = centroid{name: f[1], lat: lat, lon: lon}
		}
	})
	return centroidMap
}

// CountryName returns the English name for an ISO code, "" if unknown.
func CountryName(code string) string {
	return centroids()[strings.ToUpper(code)].name
}

// anycastASNs are networks that announce the same addresses from many
// edges, so a database's single city for one of their IPs is only one of
// many places it is served from. Cloud providers whose ASN also carries
// regional (unicast) hosting are listed when their edge network shares it;
// the map then says "anycast" for those too, which errs toward caution.
var anycastASNs = map[uint32]bool{
	13335:  true, // Cloudflare
	209242: true, // Cloudflare Spectrum / WARP
	15169:  true, // Google
	396982: true, // Google Cloud
	36040:  true, // YouTube (Google)
	20940:  true, // Akamai
	16625:  true, // Akamai
	32787:  true, // Akamai Prolexic
	54113:  true, // Fastly
	16509:  true, // Amazon (CloudFront edges)
	8075:   true, // Microsoft (Azure Front Door / edges)
	32934:  true, // Meta
	15133:  true, // Edgio / Edgecast
	22822:  true, // Edgio / Limelight
	60068:  true, // CDN77 / DataCamp
	19551:  true, // Imperva Incapsula
	30148:  true, // Sucuri
	19281:  true, // Quad9
	36692:  true, // Cisco OpenDNS / Umbrella
	199524: true, // G-Core Labs
	62597:  true, // NSONE
	12008:  true, // Neustar UltraDNS
}

// IsAnycast reports whether asn is a known anycast network.
func IsAnycast(asn uint32) bool { return anycastASNs[asn] }
