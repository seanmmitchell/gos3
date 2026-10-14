package storage

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// EncryptionProvidersBackend is implemented by backends that can tell
// which SSE-S3 implementations (EncryptionAES256, EncryptionRestic) a
// bucket can use right now: restic needs the restic executable on the
// server and filesystem storage for every copy of the bucket. The admin
// API reports the list so the web UI offers only usable providers, and
// refuses to select another one.
type EncryptionProvidersBackend interface {
	// EncryptionProviders returns the usable providers of bucket, in
	// preference order (EncryptionAES256 first).
	EncryptionProviders(ctx context.Context, bucket string) ([]string, error)
}

// ValidEncryptionProvider reports whether p names a known SSE-S3
// implementation.
func ValidEncryptionProvider(p string) bool {
	return p == EncryptionAES256 || p == EncryptionRestic
}

// IntersectEncryptionProviders returns the providers present in every
// list, in the order of the first. With no lists it returns nil.
func IntersectEncryptionProviders(lists ...[]string) []string {
	if len(lists) == 0 {
		return nil
	}
	out := slices.Clone(lists[0])
	for _, l := range lists[1:] {
		out = slices.DeleteFunc(out, func(p string) bool { return !slices.Contains(l, p) })
	}
	return out
}

// maxResticEndpointLen bounds ResticS3.Endpoint (config.MaxURLLen).
const maxResticEndpointLen = 2048

// resticBucketRe is an S3 bucket name as S3-compatible services accept
// it (AWS's rules, plus upper case and underscores of older buckets).
var resticBucketRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,61}[A-Za-z0-9]$`)

// resticRegionRe is config.ValidRegion's syntax.
var resticRegionRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

func validResticBucket(s string) bool { return resticBucketRe.MatchString(s) }

func validResticRegion(s string) bool { return resticRegionRe.MatchString(s) }

// hasControl reports whether s contains an ASCII control character.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// validResticEndpoint makes config.ValidateBaseURL's checks (allowPath
// false) of an S3 endpoint URL. The error completes the sentence
// "<endpoint> ...".
func validResticEndpoint(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must be an absolute http:// or https:// URL")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("has no host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("must not contain credentials, a query or a fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return errors.New("must not contain a path (set the bucket and prefix separately)")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("has an invalid port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return errors.New("has an empty port")
	}
	if h := u.Hostname(); !validHostname(h) {
		if _, err := netip.ParseAddr(h); err != nil {
			return errors.New("has an invalid host")
		}
	}
	return nil
}

// validHostname is config.ValidHostname: 1-253 characters of
// dot-separated labels of 1-63 letters, digits, hyphens or underscores,
// not starting or ending with a hyphen.
func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}
