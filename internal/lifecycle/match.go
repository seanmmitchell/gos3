package lifecycle

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Expiry returns when an action configured with a number of days
// becomes due for something created (or made noncurrent, or initiated) at t:
// S3 adds the days to t and rounds the result up to the following midnight
// UTC. For example an object created 2014-01-15 10:30 UTC with a 3-day
// expiration expires at 2014-01-19 00:00 UTC. Like S3 (and MinIO) the result
// is always the midnight strictly after t+days, even when t+days is itself a
// midnight. Negative days are treated as 0.
func Expiry(t time.Time, days int) time.Time {
	if days < 0 {
		days = 0
	}
	y, m, d := t.UTC().AddDate(0, 0, days).Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// Enabled reports whether the rule's Status is Enabled.
func (r *Rule) Enabled() bool { return r.Status == StatusEnabled }

// KeyPrefix returns the key prefix the rule's filter (or legacy Prefix)
// requires; "" when the rule has no prefix condition.
func (r *Rule) KeyPrefix() string {
	switch {
	case r.Prefix != nil:
		return *r.Prefix
	case r.Filter == nil:
		return ""
	case r.Filter.Prefix != nil:
		return *r.Filter.Prefix
	case r.Filter.And != nil && r.Filter.And.Prefix != nil:
		return *r.Filter.And.Prefix
	}
	return ""
}

// predicate is the normalized form of a rule's filter.
type predicate struct {
	prefix       string
	tags         []Tag
	gt, lt       int64
	hasGT, hasLT bool
}

func (r *Rule) predicate() predicate {
	p := predicate{prefix: r.KeyPrefix()}
	f := r.Filter
	if f == nil {
		return p
	}
	gt, lt := f.ObjectSizeGreaterThan, f.ObjectSizeLessThan
	if f.Tag != nil {
		p.tags = []Tag{*f.Tag}
	}
	if a := f.And; a != nil {
		p.tags = append(p.tags, a.Tags...)
		if a.ObjectSizeGreaterThan != nil {
			gt = a.ObjectSizeGreaterThan
		}
		if a.ObjectSizeLessThan != nil {
			lt = a.ObjectSizeLessThan
		}
	}
	if gt != nil {
		p.gt, p.hasGT = *gt, true
	}
	if lt != nil {
		p.lt, p.hasLT = *lt, true
	}
	return p
}

// matches reports whether an object version with the given key, tags and
// size satisfies the predicate: the key has the prefix, the object carries
// every filter tag with the same value, and the size is strictly within the
// configured bounds.
func (p *predicate) matches(key string, tags map[string]string, size int64) bool {
	if !strings.HasPrefix(key, p.prefix) {
		return false
	}
	for _, t := range p.tags {
		if v, ok := tags[t.Key]; !ok || v != t.Value {
			return false
		}
	}
	if p.hasGT && size <= p.gt {
		return false
	}
	if p.hasLT && size >= p.lt {
		return false
	}
	return true
}

// Matches reports whether the rule's filter selects an object version with
// the given key, tags and size (delete markers have no tags and size 0). It
// does not look at Status.
func (r *Rule) Matches(key string, tags map[string]string, size int64) bool {
	p := r.predicate()
	return p.matches(key, tags, size)
}

// CurrentExpiration returns the earliest time at which an enabled Days- or
// Date-based Expiration rule expires the current version of an object with
// the given key, tags, size and last-modified time, together with that
// rule's ID. ok is false when no rule applies. It backs the
// x-amz-expiration response header (see ExpirationHeader).
func (c *Configuration) CurrentExpiration(key string, tags map[string]string, size int64, lastModified time.Time) (expiry time.Time, ruleID string, ok bool) {
	if c == nil {
		return time.Time{}, "", false
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		e := r.Expiration
		if !r.Enabled() || e == nil || (e.Days == nil && e.Date == nil) || !r.Matches(key, tags, size) {
			continue
		}
		var t time.Time
		if e.Days != nil {
			t = Expiry(lastModified, *e.Days)
		} else {
			t = e.Date.UTC()
		}
		if !ok || t.Before(expiry) {
			expiry, ruleID, ok = t, r.ID, true
		}
	}
	return expiry, ruleID, ok
}

// AbortDate returns when an enabled AbortIncompleteMultipartUpload rule
// aborts a multipart upload of key initiated at initiated, and the rule's ID
// (the x-amz-abort-date and x-amz-abort-rule-id headers of
// CreateMultipartUpload and ListParts). Only the prefix part of a rule's
// filter is considered (uploads have neither tags nor a final size); rules
// with tag conditions never apply. ok is false when no rule applies.
func (c *Configuration) AbortDate(key string, initiated time.Time) (abort time.Time, ruleID string, ok bool) {
	if c == nil {
		return time.Time{}, "", false
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		a := r.AbortIncompleteMultipartUpload
		if !r.Enabled() || a == nil || a.DaysAfterInitiation == nil || r.hasTagFilter() ||
			!strings.HasPrefix(key, r.KeyPrefix()) {
			continue
		}
		t := Expiry(initiated, *a.DaysAfterInitiation)
		if !ok || t.Before(abort) {
			abort, ruleID, ok = t, r.ID, true
		}
	}
	return abort, ruleID, ok
}

// ExpirationHeader formats the x-amz-expiration response header value:
//
//	expiry-date="Fri, 23 Dec 2012 00:00:00 GMT", rule-id="picture-deletion-rule"
//
// The rule ID is URL-encoded (as S3 does) so that it cannot break the
// quoting.
func ExpirationHeader(expiry time.Time, ruleID string) string {
	return fmt.Sprintf(`expiry-date="%s", rule-id="%s"`, expiry.UTC().Format(timeFormatHTTP), url.PathEscape(ruleID))
}

// timeFormatHTTP is the RFC 7231 IMF-fixdate layout (http.TimeFormat).
const timeFormatHTTP = "Mon, 02 Jan 2006 15:04:05 GMT"
