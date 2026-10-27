package updates

import (
	"net/url"
	"regexp"
	"strings"
)

// Only stable SemVer builds can be compared. Development/prerelease builds
// intentionally produce no notice. Build metadata does not affect precedence.
var stableVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

func components(v string) []string { return stableVersion.FindStringSubmatch(v) }

func compare(a, b []string) int {
	for i := 1; i <= 3; i++ {
		// SemVer integers have no size limit or leading zeroes.
		if len(a[i]) != len(b[i]) {
			if len(a[i]) > len(b[i]) {
				return 1
			}
			return -1
		}
		if n := strings.Compare(a[i], b[i]); n != 0 {
			return n
		}
	}
	return 0
}

// Release is a stable, published release. Notes are linked, never fetched.
type Release struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Type    string `json:"type"`
}

func newest(installed string, tags, types []string) *Release {
	base := components(installed)
	if base == nil {
		return nil
	}
	var best []string
	var out *Release
	for _, tag := range tags {
		v := components(tag)
		if v == nil || compare(v, base) <= 0 {
			continue
		}
		kind := "patch"
		if v[1] != base[1] {
			kind = "major"
		} else if v[2] != base[2] {
			kind = "minor"
		}
		allowed := false
		for _, t := range types {
			if kind == t {
				allowed = true
			}
		}
		if !allowed || (best != nil && compare(v, best) <= 0) {
			continue
		}
		best = v
		out = &Release{Version: tag, Type: kind, URL: "https://github.com/seanmmitchell/gos3/releases/tag/" + url.PathEscape(tag)}
	}
	return out
}
