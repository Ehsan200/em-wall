package xray

import (
	"regexp"
	"strings"
)

var slotOutRe = regexp.MustCompile(`slot\d+-out-(\S+)`)

// ParseBalancerWinners extracts each balancer's current "Selects" winner
// from `xray api bi` output. That CLI returns human TEXT (not JSON) shaped
// like, per balancer:
//
//   - Selecting Override:
//     ...
//   - Selects:
//     1   slot0-out-<fingerprint>
//
// The member tag suffix after "-out-" is the node fingerprint (or an
// "xray-"/"proxy-" key for non-subscription members). Returned deduped,
// in first-seen order. Per-node RTT is NOT present in bi output, so this
// is winner-only.
func ParseBalancerWinners(biText string) []string {
	var out []string
	seen := make(map[string]bool)
	inSelects := false
	for _, ln := range strings.Split(biText, "\n") {
		if strings.Contains(ln, "Selects:") {
			inSelects = true
			continue
		}
		// Any other "- <Header>:" line starts a new section.
		if strings.HasPrefix(strings.TrimSpace(ln), "- ") {
			inSelects = false
		}
		if !inSelects {
			continue
		}
		if m := slotOutRe.FindStringSubmatch(ln); m != nil {
			fp := m[1]
			if !seen[fp] {
				seen[fp] = true
				out = append(out, fp)
			}
		}
	}
	return out
}
