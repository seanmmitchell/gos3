package s3

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

func TestCheckConditions(t *testing.T) {
	lm := time.Date(2024, 5, 6, 7, 8, 9, 500e6, time.UTC) // sub-second part is ignored
	const etag = "abc123"
	before := lm.Add(-time.Hour).Format(http.TimeFormat)
	same := lm.Format(http.TimeFormat)
	after := lm.Add(time.Hour).Format(http.TimeFormat)
	for _, tc := range []struct {
		name string
		hdr  map[string]string
		want string // "", "PreconditionFailed", "NotModified"
	}{
		{"none", nil, ""},
		{"if-match ok", map[string]string{"If-Match": `"abc123"`}, ""},
		{"if-match unquoted", map[string]string{"If-Match": `abc123`}, ""},
		{"if-match list", map[string]string{"If-Match": `"x", "abc123"`}, ""},
		{"if-match star", map[string]string{"If-Match": `*`}, ""},
		{"if-match weak", map[string]string{"If-Match": `W/"abc123"`}, ""},
		{"if-match fail", map[string]string{"If-Match": `"zzz"`}, "PreconditionFailed"},
		{"if-unmodified-since ok (same second)", map[string]string{"If-Unmodified-Since": same}, ""},
		{"if-unmodified-since fail", map[string]string{"If-Unmodified-Since": before}, "PreconditionFailed"},
		{"if-unmodified-since garbage ignored", map[string]string{"If-Unmodified-Since": "yesterday"}, ""},
		{"if-match true + unmodified false => 200", map[string]string{"If-Match": `"abc123"`, "If-Unmodified-Since": before}, ""},
		{"if-none-match match", map[string]string{"If-None-Match": `"abc123"`}, "NotModified"},
		{"if-none-match star", map[string]string{"If-None-Match": `*`}, "NotModified"},
		{"if-none-match other", map[string]string{"If-None-Match": `"other"`}, ""},
		{"if-modified-since not modified", map[string]string{"If-Modified-Since": same}, "NotModified"},
		{"if-modified-since after", map[string]string{"If-Modified-Since": after}, "NotModified"},
		{"if-modified-since modified", map[string]string{"If-Modified-Since": before}, ""},
		{"none-match false + modified-since true => 304", map[string]string{"If-None-Match": `"abc123"`, "If-Modified-Since": before}, "NotModified"},
		{"none-match true + modified-since false => 200", map[string]string{"If-None-Match": `"zzz"`, "If-Modified-Since": after}, ""},
		{"if-match fail beats none-match", map[string]string{"If-Match": `"zzz"`, "If-None-Match": `"abc123"`}, "PreconditionFailed"},
		{"rfc850 date", map[string]string{"If-Modified-Since": lm.Add(-time.Hour).Format(time.RFC850)}, ""},
	} {
		h := http.Header{}
		for k, v := range tc.hdr {
			h.Set(k, v)
		}
		err := checkConditions(h, etag, lm)
		if got := codeOf(err); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		// Copy-source variant: same logic, every failure is 412.
		ch := http.Header{}
		for k, v := range tc.hdr {
			ch.Set("X-Amz-Copy-Source-"+k, v)
		}
		want := tc.want
		if want == "NotModified" {
			want = "PreconditionFailed"
		}
		if got := codeOf(checkCopySourceConditions(ch, etag, lm)); got != want {
			t.Errorf("copy %s: %q, want %q", tc.name, got, want)
		}
	}
	if err := checkConditions(http.Header{"If-Match": {`"x"`}}, etag, lm); s3err.As(err).Extra["Condition"] != "If-Match" {
		t.Fatalf("condition element: %v", s3err.As(err).Extra)
	}
}

func codeOf(err error) string {
	if err == nil {
		return ""
	}
	return s3err.As(err).Code
}

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		v             string
		size          int64
		start, length int64
		none          bool
		err           bool
	}{
		{v: "", size: 10, none: true},
		{v: "bytes=0-4", size: 10, start: 0, length: 5},
		{v: "bytes=2-", size: 10, start: 2, length: 8},
		{v: "bytes=-3", size: 10, start: 7, length: 3},
		{v: "bytes=-30", size: 10, start: 0, length: 10},
		{v: "bytes=5-100", size: 10, start: 5, length: 5},
		{v: "bytes=9-9", size: 10, start: 9, length: 1},
		{v: "Bytes = 1-2", size: 10, start: 1, length: 2},
		{v: "bytes=0-99999999999999999999999", size: 10, start: 0, length: 10},
		{v: "bytes=10-", size: 10, err: true},
		{v: "bytes=10-20", size: 10, err: true},
		{v: "bytes=99999999999999999999999-", size: 10, err: true},
		{v: "bytes=-0", size: 10, err: true},
		{v: "bytes=0-", size: 0, err: true},
		{v: "bytes=-5", size: 0, err: true},
		{v: "bytes=0-1,3-4", size: 10, none: true},
		{v: "bytes=4-2", size: 10, none: true},
		{v: "bytes=a-b", size: 10, none: true},
		{v: "bytes=-", size: 10, none: true},
		{v: "bytes=1", size: 10, none: true},
		{v: "items=0-1", size: 10, none: true},
		{v: "bytes=+1-2", size: 10, none: true},
	} {
		br, err := parseRange(tc.v, tc.size)
		switch {
		case tc.err:
			if e := s3err.As(err); err == nil || e.Code != "InvalidRange" || e.HTTPStatus != 416 || e.Extra["ActualObjectSize"] == "" {
				t.Errorf("%q/%d: %v %v, want InvalidRange", tc.v, tc.size, br, err)
			}
		case tc.none:
			if br != nil || err != nil {
				t.Errorf("%q: %+v %v, want ignored", tc.v, br, err)
			}
		default:
			if err != nil || br == nil || br.Start != tc.start || br.Length != tc.length {
				t.Errorf("%q/%d: %+v %v, want %d+%d", tc.v, tc.size, br, err, tc.start, tc.length)
			}
		}
	}
	br, _ := parseRange("bytes=2-5", 10)
	if br.End() != 5 || br.ContentRange(10) != "bytes 2-5/10" {
		t.Fatalf("%d %s", br.End(), br.ContentRange(10))
	}
}

func TestParseCopySource(t *testing.T) {
	for _, tc := range []struct {
		v                    string
		bucket, key, version string
		err                  bool
	}{
		{v: "/src/key", bucket: "src", key: "key"},
		{v: "src/key", bucket: "src", key: "key"},
		{v: "src/dir/sub%20dir/f%2Bplus+x.txt", bucket: "src", key: "dir/sub dir/f+plus+x.txt"},
		{v: "src/k?versionId=abc", bucket: "src", key: "k", version: "abc"},
		{v: "src/what%3F?versionId=null", bucket: "src", key: "what?", version: "null"},
		{v: "src/%E6%97%A5", bucket: "src", key: "日"},
		{v: "src//double", bucket: "src", key: "/double"},
		{v: "src/k?other=1", bucket: "src", key: "k"},
		{v: "", err: true},
		{v: "src", err: true},
		{v: "src/", err: true},
		{v: "/key-only", err: true},
		{v: "src/%zz", err: true},
		{v: "src/k?versionId=", err: true},
		{v: "src/" + strings.Repeat("k", 1025), err: true},
	} {
		cs, err := parseCopySource(tc.v)
		if tc.err {
			if err == nil {
				t.Errorf("%q: %+v, want error", tc.v, cs)
			}
			continue
		}
		if err != nil || cs.Bucket != tc.bucket || cs.Key != tc.key || cs.VersionID != tc.version {
			t.Errorf("%q: %+v %v", tc.v, cs, err)
		}
	}
}

func TestParseCopySourceRange(t *testing.T) {
	br, err := parseCopySourceRange("bytes=0-9", 100)
	if err != nil || br.Start != 0 || br.Length != 10 {
		t.Fatalf("%+v %v", br, err)
	}
	for _, v := range []string{"bytes=0-100", "bytes=5-4", "bytes=-5", "bytes=5-", "0-5", "bytes=a-b", ""} {
		if _, err := parseCopySourceRange(v, 100); codeOf(err) != "InvalidArgument" {
			t.Errorf("%q: %v", v, err)
		}
	}
}

func TestETagHelpers(t *testing.T) {
	if quoteETag("abc") != `"abc"` || quoteETag(`"abc"`) != `"abc"` || trimETag(` W/"x" `) != "x" {
		t.Fatal("etag helpers")
	}
	if !etagListMatches(`"a", "b"`, "b") || etagListMatches(`"a"`, "b") || etagListMatches(``, "b") {
		t.Fatal("etagListMatches")
	}
}
