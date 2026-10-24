package selfuse

import (
	"reflect"
	"testing"
)

func TestStaticFromRealURLs(t *testing.T) {
	s := NewSet(Static())
	for host, want := range map[string][]string{
		"ip-api.com":                           {PurposeExitIP},
		"download.db-ip.com":                   {PurposeGeoData},
		"github.com":                           {PurposeAppUpdate, PurposeGeoData},
		"api.github.com":                       {PurposeAppUpdate},
		"release-assets.githubusercontent.com": {PurposeAppUpdate, PurposeGeoData},
		"raw.githubusercontent.com":            {PurposeGeoData},
		"cp.cloudflare.com":                    {PurposeProbe},
		"www.gstatic.com":                      {PurposeProbe},
	} {
		if got := s.Purposes(host); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", host, got, want)
		}
	}
	// Exact names only: the rest of the service is the user's.
	if s.Has("avatars.githubusercontent.com") || s.Has("gist.github.com") {
		t.Error("subdomain counted as em-wall's own")
	}
	if !s.Has("GitHub.com.") {
		t.Error("case / trailing dot")
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://Sub.Example.com:8443/x?y": "sub.example.com",
		"example.com:443":                  "example.com",
		"example.com":                      "example.com",
		"1.2.3.4":                          "",
		"[2001:db8::1]:443":                "",
		"2001:db8::1":                      "",
		"":                                 "",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
