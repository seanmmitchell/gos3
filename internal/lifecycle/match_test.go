package lifecycle

import (
	"testing"
	"time"
)

func utc(y int, m time.Month, d, h, min, s int) time.Time {
	return time.Date(y, m, d, h, min, s, 0, time.UTC)
}

func TestExpiry(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	tests := []struct {
		name string
		t    time.Time
		days int
		want time.Time
	}{
		// The AWS documentation example: created 2014-01-15 10:30 UTC,
		// 3 days -> 2014-01-19 00:00 UTC.
		{"aws example", utc(2014, 1, 15, 10, 30, 0), 3, utc(2014, 1, 19, 0, 0, 0)},
		{"one day late evening", utc(2024, 1, 1, 23, 59, 59), 1, utc(2024, 1, 3, 0, 0, 0)},
		{"one day just after midnight", utc(2024, 1, 1, 0, 0, 1), 1, utc(2024, 1, 3, 0, 0, 0)},
		{"exact midnight rounds to the next one", utc(2024, 1, 1, 0, 0, 0), 1, utc(2024, 1, 3, 0, 0, 0)},
		{"month end", utc(2024, 1, 31, 12, 0, 0), 1, utc(2024, 2, 2, 0, 0, 0)},
		{"leap day", utc(2024, 2, 28, 12, 0, 0), 1, utc(2024, 3, 1, 0, 0, 0)},
		{"non leap year", utc(2023, 2, 28, 12, 0, 0), 1, utc(2023, 3, 2, 0, 0, 0)},
		{"year end", utc(2023, 12, 30, 8, 0, 0), 2, utc(2024, 1, 2, 0, 0, 0)},
		{"zero days", utc(2024, 5, 5, 5, 5, 5), 0, utc(2024, 5, 6, 0, 0, 0)},
		{"negative treated as zero", utc(2024, 5, 5, 5, 5, 5), -4, utc(2024, 5, 6, 0, 0, 0)},
		// 2024-01-01 00:30 CET is 2023-12-31 23:30 UTC.
		{"non-UTC input", time.Date(2024, 1, 1, 0, 30, 0, 0, cet), 1, utc(2024, 1, 2, 0, 0, 0)},
		{"large", utc(2024, 1, 1, 1, 0, 0), 365, utc(2025, 1, 1, 0, 0, 0)},
		{"max int32 does not panic", utc(2024, 1, 1, 1, 0, 0), 1<<31 - 1, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1<<31)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Expiry(tc.t, tc.days)
			if !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("Expiry(%v, %d) = %v, want %v", tc.t, tc.days, got, tc.want)
			}
		})
	}
}

func TestRuleMatches(t *testing.T) {
	tags := map[string]string{"a": "1", "b": "2"}
	tests := []struct {
		name string
		rule Rule
		key  string
		tags map[string]string
		size int64
		want bool
	}{
		{"legacy prefix match", Rule{Prefix: ptr("logs/")}, "logs/x", nil, 1, true},
		{"legacy prefix miss", Rule{Prefix: ptr("logs/")}, "log", nil, 1, false},
		{"empty filter", Rule{Filter: &Filter{}}, "anything", nil, 0, true},
		{"filter prefix", Rule{Filter: &Filter{Prefix: ptr("a/")}}, "a/b", nil, 0, true},
		{"filter prefix miss", Rule{Filter: &Filter{Prefix: ptr("a/")}}, "b/a/", nil, 0, false},
		{"tag match", Rule{Filter: &Filter{Tag: &Tag{"a", "1"}}}, "k", tags, 0, true},
		{"tag wrong value", Rule{Filter: &Filter{Tag: &Tag{"a", "2"}}}, "k", tags, 0, false},
		{"tag missing", Rule{Filter: &Filter{Tag: &Tag{"c", ""}}}, "k", tags, 0, false},
		{"tag empty value present", Rule{Filter: &Filter{Tag: &Tag{"c", ""}}}, "k", map[string]string{"c": ""}, 0, true},
		{"tag nil object tags", Rule{Filter: &Filter{Tag: &Tag{"a", "1"}}}, "k", nil, 0, false},
		{"gt strict", Rule{Filter: &Filter{ObjectSizeGreaterThan: ptr[int64](10)}}, "k", nil, 10, false},
		{"gt ok", Rule{Filter: &Filter{ObjectSizeGreaterThan: ptr[int64](10)}}, "k", nil, 11, true},
		{"gt zero excludes empty", Rule{Filter: &Filter{ObjectSizeGreaterThan: ptr[int64](0)}}, "k", nil, 0, false},
		{"lt strict", Rule{Filter: &Filter{ObjectSizeLessThan: ptr[int64](10)}}, "k", nil, 10, false},
		{"lt ok", Rule{Filter: &Filter{ObjectSizeLessThan: ptr[int64](10)}}, "k", nil, 9, true},
		{"and all", Rule{Filter: &Filter{And: &And{Prefix: ptr("p"), Tags: []Tag{{"a", "1"}, {"b", "2"}},
			ObjectSizeGreaterThan: ptr[int64](1), ObjectSizeLessThan: ptr[int64](5)}}}, "px", tags, 3, true},
		{"and object has extra tags", Rule{Filter: &Filter{And: &And{Tags: []Tag{{"a", "1"}}}}}, "px", tags, 3, true},
		{"and missing one tag", Rule{Filter: &Filter{And: &And{Prefix: ptr("p"), Tags: []Tag{{"a", "1"}, {"z", "2"}}}}}, "px", tags, 3, false},
		{"and prefix miss", Rule{Filter: &Filter{And: &And{Prefix: ptr("q"), Tags: []Tag{{"a", "1"}}}}}, "px", tags, 3, false},
		{"and too big", Rule{Filter: &Filter{And: &And{Prefix: ptr("p"), ObjectSizeLessThan: ptr[int64](3)}}}, "px", tags, 3, false},
		{"and too small", Rule{Filter: &Filter{And: &And{Prefix: ptr("p"), ObjectSizeGreaterThan: ptr[int64](3)}}}, "px", tags, 3, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Matches(tc.key, tc.tags, tc.size); got != tc.want {
				t.Fatalf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKeyPrefix(t *testing.T) {
	cases := map[string]Rule{
		"":   {Filter: &Filter{Tag: &Tag{"a", "b"}}},
		"l":  {Prefix: ptr("l")},
		"f":  {Filter: &Filter{Prefix: ptr("f")}},
		"a":  {Filter: &Filter{And: &And{Prefix: ptr("a")}}},
		"nf": {Prefix: ptr("nf")},
	}
	for want, r := range cases {
		if got := r.KeyPrefix(); got != want {
			t.Errorf("KeyPrefix(%+v) = %q, want %q", r, got, want)
		}
	}
	if (&Rule{}).KeyPrefix() != "" {
		t.Error("nil filter")
	}
}

func TestCurrentExpiration(t *testing.T) {
	c := &Configuration{Rules: []Rule{
		{ID: "disabled", Filter: &Filter{}, Status: StatusDisabled, Expiration: &Expiration{Days: ptr(1)}},
		{ID: "long", Filter: &Filter{}, Status: StatusEnabled, Expiration: &Expiration{Days: ptr(30)}},
		{ID: "short-tmp", Filter: &Filter{Prefix: ptr("tmp/")}, Status: StatusEnabled, Expiration: &Expiration{Days: ptr(2)}},
		{ID: "date-tag", Filter: &Filter{Tag: &Tag{"x", "y"}}, Status: StatusEnabled,
			Expiration: &Expiration{Date: &Date{utc(2024, 1, 5, 0, 0, 0)}}},
		{ID: "marker", Filter: &Filter{}, Status: StatusEnabled, Expiration: &Expiration{ExpiredObjectDeleteMarker: ptr(true)}},
		{ID: "nc", Filter: &Filter{}, Status: StatusEnabled, NoncurrentVersionExpiration: &NoncurrentVersionExpiration{NoncurrentDays: ptr(1)}},
	}}
	lm := utc(2024, 1, 1, 12, 0, 0)
	tests := []struct {
		key    string
		tags   map[string]string
		want   time.Time
		ruleID string
	}{
		{"a", nil, utc(2024, 2, 1, 0, 0, 0), "long"},
		{"tmp/a", nil, utc(2024, 1, 4, 0, 0, 0), "short-tmp"},
		{"a", map[string]string{"x": "y"}, utc(2024, 1, 5, 0, 0, 0), "date-tag"},
		{"tmp/a", map[string]string{"x": "y"}, utc(2024, 1, 4, 0, 0, 0), "short-tmp"},
	}
	for _, tc := range tests {
		got, id, ok := c.CurrentExpiration(tc.key, tc.tags, 1, lm)
		if !ok || !got.Equal(tc.want) || id != tc.ruleID {
			t.Errorf("CurrentExpiration(%q, %v) = %v %q %v, want %v %q", tc.key, tc.tags, got, id, ok, tc.want, tc.ruleID)
		}
	}
	none := &Configuration{Rules: c.Rules[4:]}
	if _, _, ok := none.CurrentExpiration("a", nil, 1, lm); ok {
		t.Error("rules without Days/Date must not produce an expiration")
	}
	var nilCfg *Configuration
	if _, _, ok := nilCfg.CurrentExpiration("a", nil, 1, lm); ok {
		t.Error("nil configuration")
	}
}

func TestAbortDate(t *testing.T) {
	c := &Configuration{Rules: []Rule{
		{ID: "off", Filter: &Filter{}, Status: StatusDisabled, AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: ptr(1)}},
		{ID: "all", Filter: &Filter{}, Status: StatusEnabled, AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: ptr(7)}},
		{ID: "up", Filter: &Filter{Prefix: ptr("up/")}, Status: StatusEnabled, AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: ptr(1)}},
		{ID: "tagged", Filter: &Filter{Tag: &Tag{"a", "b"}}, Status: StatusEnabled, AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: ptr(1)}},
	}}
	init := utc(2024, 3, 1, 6, 0, 0)
	if got, id, ok := c.AbortDate("x", init); !ok || id != "all" || !got.Equal(utc(2024, 3, 9, 0, 0, 0)) {
		t.Errorf("x: %v %q %v", got, id, ok)
	}
	if got, id, ok := c.AbortDate("up/x", init); !ok || id != "up" || !got.Equal(utc(2024, 3, 3, 0, 0, 0)) {
		t.Errorf("up/x: %v %q %v", got, id, ok)
	}
	if _, _, ok := (&Configuration{Rules: c.Rules[3:]}).AbortDate("x", init); ok {
		t.Error("tag rule must not apply to uploads")
	}
}

func TestExpirationHeader(t *testing.T) {
	got := ExpirationHeader(utc(2012, 12, 23, 0, 0, 0), "picture-deletion-rule")
	want := `expiry-date="Sun, 23 Dec 2012 00:00:00 GMT", rule-id="picture-deletion-rule"`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	if got := ExpirationHeader(utc(2012, 12, 23, 0, 0, 0), `a "b"`); got != `expiry-date="Sun, 23 Dec 2012 00:00:00 GMT", rule-id="a%20%22b%22"` {
		t.Fatalf("escaping: %s", got)
	}
}
