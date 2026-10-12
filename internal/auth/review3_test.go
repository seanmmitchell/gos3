package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSignedHeaderUnicodeWhitespace: only spaces and tabs around a header
// value are outside the signature; appending other whitespace (U+00A0,
// U+0085) to a signed header breaks it.
func TestSignedHeaderUnicodeWhitespace(t *testing.T) {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	r := httptest.NewRequest("PUT", "/k", nil)
	r.Host = "bucket.example.com"
	r.Header.Set("X-Amz-Acl", "public-read")
	manualSign(t, r, "host;x-amz-acl;x-amz-content-sha256;x-amz-date", now)
	if _, err := v.Authenticate(r); err != nil {
		t.Fatalf("baseline request: %v", err)
	}
	for _, val := range []string{"public-read ", "\u0085public-read", "public-read　"} {
		r.Header.Set("X-Amz-Acl", val)
		if _, err := v.Authenticate(r); errCode(err) != "SignatureDoesNotMatch" {
			t.Errorf("x-amz-acl %q: got %v, want SignatureDoesNotMatch", val, err)
		}
	}
	r.Header.Set("X-Amz-Acl", " public-read\t")
	if _, err := v.Authenticate(r); err != nil {
		t.Errorf("spaces and tabs are not signed: %v", err)
	}
}

// TestQueryValuesMatchSignature: QueryValues sees every parameter the
// signature covers, decoded the same way, including values with ';' or a
// literal '%' that net/url would drop.
func TestQueryValuesMatchSignature(t *testing.T) {
	r := httptest.NewRequest("GET", "/b?list-type=2&prefix=team;a/&x=100%&y=a+b%2Fc&flag&=v", nil)
	q, err := QueryValues(r)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"list-type": "2", "prefix": "team;a/", "x": "100%", "y": "a b/c", "flag": "", "": "v"} {
		if got, ok := q[k]; !ok || len(got) != 1 || got[0] != want {
			t.Errorf("%q = %q, want %q", k, got, want)
		}
	}
	params, _ := parseRawQuery(requestRawQuery(r))
	if len(params) != 6 || len(q) != 6 {
		t.Errorf("params %v, values %v", params, q)
	}
	r = httptest.NewRequest("GET", "/b?"+strings.Repeat("a=1&", maxQueryParams+1), nil)
	if _, err := QueryValues(r); errCode(err) != "InvalidArgument" {
		t.Errorf("too many parameters: %v", err)
	}
}
