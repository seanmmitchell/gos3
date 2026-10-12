package auth

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// manualSign signs r (a server-side request) over exactly the given
// SignedHeaders list, which lets tests build requests real signers would not.
func manualSign(t *testing.T, r *http.Request, signedHeaders string, at time.Time) {
	t.Helper()
	at = at.UTC()
	amzDate := at.Format(iso8601Format)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", PayloadUnsigned)
	params, _ := parseRawQuery(r.URL.RawQuery)
	names := strings.Split(signedHeaders, ";")
	creq := buildCanonicalRequest(r.Method, canonicalURIPath(r.URL.EscapedPath()), canonicalQueryString(params, ""),
		canonicalHeaders(r, names, r.Host), signedHeaders, PayloadUnsigned)
	scope := at.Format(yyyymmdd) + "/us-east-1/s3/aws4_request"
	sig := hex.EncodeToString(hmacSHA256(SigningKey(testSK, at.Format(yyyymmdd), "us-east-1", "s3"), buildStringToSign(amzDate, scope, creq)))
	r.Header.Set("Authorization", signV4Algorithm+" Credential="+testAK+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+sig)
}

// TestRepeatedSignedHeadersRejected: listing a header many times in
// SignedHeaders must not multiply its value into the canonical request (and
// the error body); the request is rejected as malformed before any work.
func TestRepeatedSignedHeadersRejected(t *testing.T) {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	r := httptest.NewRequest("GET", "/k", nil)
	r.Host = "bucket.example.com"
	r.Header.Set("X-Amz-Meta-A", strings.Repeat("A", 100<<10))
	manualSign(t, r, "host;x-amz-content-sha256;x-amz-date;x-amz-meta-a", now)
	if _, err := v.Authenticate(r); err != nil {
		t.Fatalf("baseline request: %v", err)
	}
	signed := "host;x-amz-content-sha256;x-amz-date" + strings.Repeat(";x-amz-meta-a", 200)
	r.Header.Set("Authorization", signV4Algorithm+" Credential="+testAK+"/20240301/us-east-1/s3/aws4_request, SignedHeaders="+
		signed+", Signature="+strings.Repeat("0", 64))
	_, err := v.Authenticate(r)
	if errCode(err) != "AuthorizationHeaderMalformed" {
		t.Fatalf("repeated SignedHeaders: got %v, want AuthorizationHeaderMalformed", err)
	}
	// Case-insensitive duplicates too.
	r.Header.Set("Authorization", signV4Algorithm+" Credential="+testAK+"/20240301/us-east-1/s3/aws4_request, SignedHeaders="+
		"host;x-amz-date;X-Amz-Date, Signature="+strings.Repeat("0", 64))
	if _, err := v.Authenticate(r); errCode(err) != "AuthorizationHeaderMalformed" {
		t.Fatalf("case-insensitive duplicate: got %v", err)
	}

	// Presigned: AuthorizationQueryParametersError.
	q := httptest.NewRequest("GET", "/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential="+testAK+
		"%2F20240301%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20240301T120000Z&X-Amz-Expires=60"+
		"&X-Amz-SignedHeaders=host%3Bhost&X-Amz-Signature="+strings.Repeat("0", 64), nil)
	q.Host = "bucket.example.com"
	if _, err := v.Authenticate(q); errCode(err) != "AuthorizationQueryParametersError" {
		t.Fatalf("presigned repeated SignedHeaders: got %v", err)
	}
}

// TestSignatureMismatchEchoIsBounded: the canonical request echoed in a
// SignatureDoesNotMatch error is capped so large signed headers cannot
// amplify into a huge error body.
func TestSignatureMismatchEchoIsBounded(t *testing.T) {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	r := httptest.NewRequest("GET", "/k", nil)
	r.Host = "bucket.example.com"
	r.Header.Set("X-Amz-Meta-A", strings.Repeat("A", 200<<10))
	manualSign(t, r, "host;x-amz-content-sha256;x-amz-date;x-amz-meta-a", now)
	r.Header.Set("X-Amz-Meta-A", strings.Repeat("B", 200<<10)) // tamper
	_, err := v.Authenticate(r)
	var e *s3err.Error
	if !errors.As(err, &e) || e.Code != "SignatureDoesNotMatch" {
		t.Fatalf("got %v, want SignatureDoesNotMatch", err)
	}
	for _, k := range []string{"CanonicalRequest", "StringToSign"} {
		if n := len(e.Extra[k]); n == 0 || n > maxEchoedCanonical+3 {
			t.Errorf("%s echoed with %d bytes", k, n)
		}
	}
}

// TestHTTP2ExpectHeaderSigned: Go's HTTP/2 server strips
// "Expect: 100-continue" from r.Header; a signature covering it must still
// verify.
func TestHTTP2ExpectHeaderSigned(t *testing.T) {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	r := httptest.NewRequest("PUT", "/k", strings.NewReader("data"))
	r.Host = "bucket.example.com"
	r.Header.Set("Expect", "100-continue")
	manualSign(t, r, "expect;host;x-amz-content-sha256;x-amz-date", now)
	if _, err := v.Authenticate(r); err != nil {
		t.Fatalf("HTTP/1.1: %v", err)
	}
	// What the handler sees over HTTP/2.
	r.Header.Del("Expect")
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
	if _, err := v.Authenticate(r); err != nil {
		t.Fatalf("HTTP/2 without Expect in r.Header: %v", err)
	}
	// HTTP/1.1 keeps the header, so a missing one stays a mismatch there.
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/1.1", 1, 1
	if _, err := v.Authenticate(r); errCode(err) != "SignatureDoesNotMatch" {
		t.Fatalf("HTTP/1.1 without Expect: got %v", err)
	}
}
