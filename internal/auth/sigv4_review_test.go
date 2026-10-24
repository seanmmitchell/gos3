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
// amplify into a huge error body. See also
// TestSignatureMismatchRedactsHeaderValues.
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
	// x-amz-* values are the client's own and stay in the echo (only
	// headers a browser or proxy may add on its own are redacted).
	if !strings.Contains(e.Extra["CanonicalRequest"], "x-amz-meta-a:BBBB") {
		t.Errorf("CanonicalRequest lost the x-amz-meta-a value")
	}
}

// TestSignatureMismatchRedactsHeaderValues: a client may list any header in
// SignedHeaders, including ones a browser or proxy adds without the page
// seeing them (Cookie, proxy-injected tokens). The CanonicalRequest echoed in
// SignatureDoesNotMatch must not hand those values back to the caller, for
// header auth and presigned URLs alike; host stays readable.
func TestSignatureMismatchRedactsHeaderValues(t *testing.T) {
	const (
		cookieVal = "session=COOKIE-SECRET-4f1d"
		tokenVal  = "PROXY-TOKEN-SECRET-9a2c"
		host      = "bucket.example.com"
	)
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	zero := strings.Repeat("0", 64)
	signed := "cookie;host;x-forwarded-access-token"

	withSecrets := func(r *http.Request) *http.Request {
		r.Host = host
		r.Header.Set("Cookie", cookieVal)
		r.Header.Set("X-Forwarded-Access-Token", tokenVal)
		return r
	}

	// The real signature still covers the real values: a correctly signed
	// request verifies, and one with a changed cookie does not.
	good := withSecrets(httptest.NewRequest("GET", "/k", nil))
	manualSign(t, good, signed, now)
	if _, err := v.Authenticate(good); err != nil {
		t.Fatalf("correctly signed request: %v", err)
	}
	good.Header.Set("Cookie", "session=other")
	if _, err := v.Authenticate(good); errCode(err) != "SignatureDoesNotMatch" {
		t.Fatalf("changed cookie: got %v, want SignatureDoesNotMatch", err)
	}

	hdr := withSecrets(httptest.NewRequest("GET", "/k", nil))
	hdr.Header.Set("X-Amz-Date", now.Format(iso8601Format))
	hdr.Header.Set("X-Amz-Content-Sha256", PayloadUnsigned)
	hdr.Header.Set("Authorization", signV4Algorithm+" Credential="+testAK+"/20240301/us-east-1/s3/aws4_request, SignedHeaders="+
		signed+", Signature="+zero)

	pre := withSecrets(httptest.NewRequest("GET", "/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential="+testAK+
		"%2F20240301%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20240301T120000Z&X-Amz-Expires=60"+
		"&X-Amz-SignedHeaders="+strings.ReplaceAll(signed, ";", "%3B")+"&X-Amz-Signature="+zero, nil))

	for name, r := range map[string]*http.Request{"header": hdr, "presigned": pre} {
		_, err := v.Authenticate(r)
		var e *s3err.Error
		if !errors.As(err, &e) || e.Code != "SignatureDoesNotMatch" {
			t.Fatalf("%s: got %v, want SignatureDoesNotMatch", name, err)
		}
		for k, val := range e.Extra {
			for _, secret := range []string{cookieVal, tokenVal, "COOKIE-SECRET", "PROXY-TOKEN-SECRET"} {
				if strings.Contains(val, secret) {
					t.Errorf("%s: %s echoes %q:\n%s", name, k, secret, val)
				}
			}
		}
		creq := e.Extra["CanonicalRequest"]
		for _, want := range []string{"host:" + host + "\n",
			"cookie:" + redactedHeaderValue + "\n", "x-forwarded-access-token:" + redactedHeaderValue + "\n"} {
			if !strings.Contains(creq, want) {
				t.Errorf("%s: CanonicalRequest lacks %q:\n%s", name, want, creq)
			}
		}
		if e.Extra["StringToSign"] == "" {
			t.Errorf("%s: StringToSign not echoed", name)
		}
	}
}

// TestEchoedHeaderValueRedaction pins which signed headers keep their value
// in the echoed canonical request.
func TestEchoedHeaderValueRedaction(t *testing.T) {
	for name, keep := range map[string]bool{
		"host": true, "Host": true, "content-type": true, "content-md5": true, "x-amz-meta-a": true,
		"X-Amz-Date": true, "range": true, "date": true, "expect": true,
		"cookie": false, "authorization": false, "x-forwarded-access-token": false,
		"x-gos3-foo": false, "transfer-encoding": false, "contentx": false, "x-amzfoo": false,
	} {
		if got := echoHeaderValue(name); got != keep {
			t.Errorf("echoHeaderValue(%q) = %v, want %v", name, got, keep)
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
