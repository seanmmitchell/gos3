package s3

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

const bucketCORS = `<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <CORSRule>
    <ID>app</ID>
    <AllowedOrigin>https://*.example.com</AllowedOrigin>
    <AllowedMethod>GET</AllowedMethod>
    <AllowedMethod>PUT</AllowedMethod>
    <AllowedHeader>x-amz-*</AllowedHeader>
    <AllowedHeader>Content-Type</AllowedHeader>
    <ExposeHeader>ETag</ExposeHeader>
    <MaxAgeSeconds>600</MaxAgeSeconds>
  </CORSRule>
  <CORSRule>
    <AllowedOrigin>*</AllowedOrigin>
    <AllowedMethod>HEAD</AllowedMethod>
  </CORSRule>
</CORSConfiguration>`

func TestParseCORSConfiguration(t *testing.T) {
	c, err := parseCORSConfiguration([]byte(bucketCORS))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Rules) != 2 || c.Rules[0].ID != "app" || *c.Rules[0].MaxAgeSeconds != 600 || len(c.Rules[0].AllowedHeaders) != 2 {
		t.Fatalf("%+v", c)
	}
	// Without namespace too.
	if _, err := parseCORSConfiguration([]byte(`<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`)); err != nil {
		t.Fatal(err)
	}
	rule := func(inner string) string {
		return `<CORSConfiguration><CORSRule>` + inner + `</CORSRule></CORSConfiguration>`
	}
	for name, tc := range map[string]struct{ doc, code string }{
		"empty":            {``, "MissingRequestBodyError"},
		"garbage":          {`<nope`, "MalformedXML"},
		"wrong root":       {`<Foo/>`, "MalformedXML"},
		"no rules":         {`<CORSConfiguration></CORSConfiguration>`, "MalformedXML"},
		"no method":        {rule(`<AllowedOrigin>*</AllowedOrigin>`), "MalformedXML"},
		"no origin":        {rule(`<AllowedMethod>GET</AllowedMethod>`), "MalformedXML"},
		"bad method":       {rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>PATCH</AllowedMethod>`), "InvalidRequest"},
		"lowercase method": {rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>get</AllowedMethod>`), "InvalidRequest"},
		"two wildcards":    {rule(`<AllowedOrigin>*.*</AllowedOrigin><AllowedMethod>GET</AllowedMethod>`), "InvalidRequest"},
		"header wildcards": {rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><AllowedHeader>**</AllowedHeader>`), "InvalidRequest"},
		"expose wildcard":  {rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><ExposeHeader>x-*</ExposeHeader>`), "InvalidRequest"},
		"negative max age": {rule(`<AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod><MaxAgeSeconds>-1</MaxAgeSeconds>`), "MalformedXML"},
		"long id":          {rule(`<ID>` + strings.Repeat("i", 256) + `</ID><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod>`), "InvalidArgument"},
		"too big":          {strings.Repeat(" ", maxCORSConfigSize+1), "MaxMessageLengthExceeded"},
		"too many rules":   {`<CORSConfiguration>` + strings.Repeat(`<CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule>`, 101) + `</CORSConfiguration>`, "MalformedXML"},
	} {
		_, err := parseCORSConfiguration([]byte(tc.doc))
		if got := s3err.As(err); err == nil || got.Code != tc.code {
			t.Errorf("%s: %v, want %s", name, err, tc.code)
		}
	}
	// Round trip with namespace.
	c.XMLNS = s3Namespace
	out, err := xml.Marshal(c)
	if err != nil || !strings.HasPrefix(string(out), `<CORSConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><CORSRule><ID>app</ID>`) {
		t.Fatalf("%s %v", out, err)
	}
}

func TestWildcardMatchFold(t *testing.T) {
	for _, tc := range []struct {
		p, s string
		want bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"https://*.example.com", "https://app.example.com", true},
		{"https://*.example.com", "https://APP.Example.com", true},
		{"https://*.example.com", "https://example.com", false},
		{"https://*.example.com", "http://app.example.com", false},
		{"https://*.example.com", "https://app.example.com.evil", false},
		{"x-amz-*", "x-amz-date", true},
		{"x-amz-*", "x-am", false},
		{"content-type", "Content-Type", true},
		{"a*a", "a", false},
	} {
		if got := wildcardMatchFold(tc.p, tc.s); got != tc.want {
			t.Errorf("%q ~ %q = %v", tc.p, tc.s, got)
		}
	}
}

func preflight(e *testEnv, path, origin, method, headers string) *testResponse {
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	if method != "" {
		h.Set("Access-Control-Request-Method", method)
	}
	if headers != "" {
		h.Set("Access-Control-Request-Headers", headers)
	}
	return e.anon("OPTIONS", path, nil, h)
}

func TestPreflightBucketConfig(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.putBucketConfig("bkt", storage.ConfigCORS, bucketCORS)

	resp := preflight(e, "/bkt/some/key", "https://app.example.com", "PUT", "X-Amz-Date, content-type")
	checkCommonHeaders(t, resp.Header)
	h := resp.Header
	if resp.StatusCode != 200 || h.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		h.Get("Access-Control-Allow-Credentials") != "true" || h.Get("Access-Control-Allow-Methods") != "GET, PUT" ||
		h.Get("Access-Control-Allow-Headers") != "x-amz-date, content-type" || h.Get("Access-Control-Max-Age") != "600" ||
		h.Get("Access-Control-Expose-Headers") != "ETag" || !strings.Contains(h.Get("Vary"), "Origin") {
		t.Fatalf("%d %v %s", resp.StatusCode, h, resp.Data)
	}
	// Wildcard rule: "*" origin, no credentials.
	resp = preflight(e, "/bkt", "https://other.org", "HEAD", "")
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("wildcard: %d %v", resp.StatusCode, resp.Header)
	}
	// No matching rule (method / header / origin) => 403 AccessForbidden.
	for _, tc := range [][3]string{
		{"https://app.example.com", "DELETE", ""},
		{"https://app.example.com", "PUT", "authorization-x"},
		{"https://other.org", "GET", ""},
	} {
		resp = preflight(e, "/bkt/k", tc[0], tc[1], tc[2])
		if resp.StatusCode != 403 || resp.code() != "AccessForbidden" || !strings.Contains(string(resp.Data), "CORSResponse") {
			t.Errorf("%v: %d %s", tc, resp.StatusCode, resp.Data)
		}
	}
	// Missing Origin / request method => 400.
	if resp = preflight(e, "/bkt/k", "", "GET", ""); resp.StatusCode != 400 || resp.code() != "BadRequest" {
		t.Fatalf("no origin: %d %s", resp.StatusCode, resp.Data)
	}
	if resp = preflight(e, "/bkt/k", "https://app.example.com", "", ""); resp.StatusCode != 400 {
		t.Fatalf("no method: %d", resp.StatusCode)
	}
	if resp = preflight(e, "/bkt/k", "https://app.example.com", "TRACE", ""); resp.StatusCode != 400 {
		t.Fatalf("bad method: %d", resp.StatusCode)
	}
	// Preflights are recorded in metrics.
	found := false
	for _, o := range e.metrics.Snapshot().Ops {
		found = found || o.Op == opPreflight
	}
	if !found {
		t.Fatal("preflight not in metrics")
	}
}

func TestPreflightGlobalFallback(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) {
		s.CORS = config.CORSConfig{AllowedOrigins: []string{"http://ui.local:3000"}}
	}))
	e.createBucket("nocors")
	for _, path := range []string{"/", "/nocors", "/nocors/k", "/doesnotexist"} {
		resp := preflight(e, path, "http://ui.local:3000", "PUT", "authorization,x-amz-content-sha256")
		h := resp.Header
		if resp.StatusCode != 200 || h.Get("Access-Control-Allow-Origin") != "http://ui.local:3000" ||
			h.Get("Access-Control-Allow-Methods") != "GET, PUT, HEAD, POST, DELETE" || h.Get("Access-Control-Max-Age") != "3600" ||
			!strings.Contains(h.Get("Access-Control-Expose-Headers"), "x-amz-version-id") {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, h)
		}
	}
	if resp := preflight(e, "/nocors", "http://evil.local", "GET", ""); resp.StatusCode != 403 {
		t.Fatalf("other origin: %d", resp.StatusCode)
	}
	// A bucket configuration overrides the global one.
	e.putBucketConfig("nocors", storage.ConfigCORS, bucketCORS)
	if resp := preflight(e, "/nocors/k", "http://ui.local:3000", "GET", ""); resp.StatusCode != 403 {
		t.Fatalf("bucket overrides global: %d", resp.StatusCode)
	}
	// Settings changes apply live.
	if _, err := e.settings.Update(func(s *config.Settings) error { s.CORS.AllowedOrigins = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if resp := preflight(e, "/", "http://ui.local:3000", "GET", ""); resp.StatusCode != 403 {
		t.Fatalf("CORS disabled: %d", resp.StatusCode)
	}
}

func TestCORSActualRequests(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) {
		s.CORS = config.CORSConfig{AllowedOrigins: []string{"*"}, ExposeHeaders: []string{"ETag"}}
	}))
	e.createBucket("bkt")
	e.putBucketConfig("bkt", storage.ConfigCORS, bucketCORS)
	e.override("GetObject", okHandler)

	h := http.Header{"Origin": {"https://app.example.com"}}
	resp := e.signed("GET", "/bkt/k", nil, h)
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" ||
		resp.Header.Get("Access-Control-Expose-Headers") != "ETag" || resp.Header.Get("Vary") != "Origin" ||
		resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	// Errors carry CORS headers too (so browsers can read them).
	resp = e.anon("GET", "/bkt/k", nil, h)
	if resp.StatusCode != 403 || resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("error without CORS: %d %v", resp.StatusCode, resp.Header)
	}
	// Method not allowed by any rule: no Allow-Origin, but Vary.
	resp = e.signed("DELETE", "/bkt/k", nil, h)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Vary") != "Origin" {
		t.Fatalf("delete: %v", resp.Header)
	}
	// Service-level request uses the global "*" rule.
	resp = e.signed("GET", "/", nil, http.Header{"Origin": {"http://x"}})
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("global: %v", resp.Header)
	}
	// No Origin header => no CORS headers.
	resp = e.signed("GET", "/bkt/k", nil, nil)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Vary") != "" {
		t.Fatalf("no origin: %v", resp.Header)
	}
}

// TestGlobalWildcardCORSSignedOnly: the global "*" rule serves only signed
// requests (Authorization header or presigned URL), so a website cannot
// make a visitor's browser read or write anonymously accessible data (for
// example a bucket allowed by aws:SourceIp) and see the response. Bucket
// CORS configurations and explicit global origins still apply to
// anonymous requests.
func TestGlobalWildcardCORSSignedOnly(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) {
		s.CORS = config.CORSConfig{AllowedOrigins: []string{"*", "https://ui.example"}}
	}))
	requireAuthz(t, e)
	e.createBucket("lan")
	e.putObject("lan", "k", []byte("intranet"), storage.ObjectMeta{})
	e.putBucketConfig("lan", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
		"Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::lan/*","Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}}]}`)
	evil := http.Header{"Origin": {"https://evil.example"}}

	// Unsigned actual requests: served, but without Access-Control-*.
	for _, m := range []string{"GET", "PUT"} {
		var body []byte
		if m == "PUT" {
			body = []byte("x")
		}
		resp := e.anon(m, "/lan/k", body, evil)
		if resp.StatusCode != 200 {
			t.Fatalf("anonymous %s: %d %s", m, resp.StatusCode, resp.Data)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("anonymous %s from any origin: Access-Control-Allow-Origin %q", m, got)
		}
		if resp.Header.Get("Vary") != "Origin" {
			t.Errorf("anonymous %s: Vary %q", m, resp.Header.Get("Vary"))
		}
	}
	// Preflights for unsigned requests are refused; for signed ones
	// (Authorization header requested, or presigned URL) approved.
	if resp := preflight(e, "/lan/k", "https://evil.example", "PUT", "content-type"); resp.StatusCode != 403 {
		t.Errorf("preflight unsigned PUT: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := preflight(e, "/lan/k", "https://evil.example", "DELETE", ""); resp.StatusCode != 403 {
		t.Errorf("preflight unsigned DELETE: %d", resp.StatusCode)
	}
	if resp := preflight(e, "/lan/k", "https://evil.example", "PUT", "authorization,x-amz-date,x-amz-content-sha256"); resp.StatusCode != 200 ||
		resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight signed PUT: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := preflight(e, "/lan/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=x&X-Amz-Signature=y", "https://evil.example", "PUT", "content-type"); resp.StatusCode != 200 {
		t.Errorf("preflight presigned PUT: %d", resp.StatusCode)
	}
	// Signed requests (header and presigned) keep the CORS headers, errors
	// included.
	if resp := e.signed("GET", "/lan/k", nil, evil); resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("signed GET: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := e.signed("GET", "/lan/missing", nil, evil); resp.StatusCode != 404 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("signed GET error: %d %v", resp.StatusCode, resp.Header)
	}
	u, err := auth.PresignURL("GET", e.url("/lan/k"), testRoot, "us-east-1", time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req := e.newRequestURL("GET", u, nil)
	req.Header.Set("Origin", "https://evil.example")
	if resp := e.do(req); resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("presigned GET: %d %v", resp.StatusCode, resp.Header)
	}
	// An explicitly listed global origin keeps working for anonymous
	// requests (the operator opted in).
	ui := http.Header{"Origin": {"https://ui.example"}}
	if resp := e.anon("GET", "/lan/k", nil, ui); resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Errorf("explicit origin anonymous: %v", resp.Header)
	}
	if resp := preflight(e, "/lan/k", "https://ui.example", "PUT", ""); resp.StatusCode != 200 {
		t.Errorf("explicit origin preflight: %d", resp.StatusCode)
	}
	// A bucket CORS configuration with "*" applies to anonymous requests.
	e.putBucketConfig("lan", storage.ConfigCORS, `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`)
	if resp := e.anon("GET", "/lan/k", nil, evil); resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("bucket CORS * anonymous: %v", resp.Header)
	}
	if resp := preflight(e, "/lan/k", "https://evil.example", "GET", ""); resp.StatusCode != 200 {
		t.Errorf("bucket CORS * preflight: %d", resp.StatusCode)
	}
}
