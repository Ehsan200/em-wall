// Package meta reads and writes geo.meta, the JSON record the geo updater
// keeps next to the files it manages (/usr/local/share/em-wall/geo.meta).
//
// It is its own stdlib-only package so the app's installer can read it —
// to avoid downgrading a downloaded file on reinstall — without pulling
// the readers' dependencies into the app module.
package meta

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// FileName is the meta file's name inside the geo data directory.
const FileName = "geo.meta"

// File keys, as used in Meta and in ipc.GeoFileDTO.File.
const (
	GeoSite = "geosite"
	GeoIP   = "geoip"
	City    = "city"
	ASN     = "asn"
)

// Files lists every key in display order.
var Files = []string{GeoSite, GeoIP, City, ASN}

// FileNames maps a key to its on-disk name.
var FileNames = map[string]string{
	GeoSite: "geosite.dat",
	GeoIP:   "geoip.dat",
	City:    "dbip-city-lite.mmdb",
	ASN:     "dbip-asn-lite.mmdb",
}

// Entry records one file's provenance.
type Entry struct {
	Source    string    `json:"source"`            // "loyalsoldier", "dbip", "bundled", …
	Version   string    `json:"version,omitempty"` // release tag / "2026-10"
	SHA256    string    `json:"sha256,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`           // when this content was installed
	CheckedAt time.Time `json:"checkedAt,omitempty"` // last successful check, changed or not
	// LastAttempt / LastError describe the most recent failed attempt;
	// both are cleared by a success.
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	// Prev is the entry of the content now in <file>.bak, so a rollback
	// restores provenance along with the bytes.
	Prev *Entry `json:"prev,omitempty"`
}

// Meta is the whole file: key → entry.
type Meta map[string]Entry

// Read loads dir/geo.meta. A missing file is an empty Meta, not an error.
func Read(dir string) (Meta, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := Meta{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Write saves m atomically (temp file + rename).
func Write(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, FileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, FileName))
}
