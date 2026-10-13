package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// lanPolicy grants anonymous reads and writes to callers on the local
// network only (here: loopback, where the test client is).
const lanPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
	"Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::lan/*",
	"Condition":{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}}]}`

// TestAnonymousCrossSiteWritesRefused: a website cannot make a visitor's
// browser blindly write to or delete from a bucket that is anonymously
// writable only from the visitor's network. Browsers send HTML form posts
// (PostObject) and no-cors fetches with a text/plain body without a
// preflight, so CORS alone (which only hides the response) does not stop
// them: anonymous cross-origin writes need a CORS rule for the origin.
// Non-browser clients, same-origin pages and signed requests are not
// affected.
func TestAnonymousCrossSiteWritesRefused(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("lan")
	e.putBucketConfig("lan", storage.ConfigPolicy, lanPolicy)
	e.putObject("lan", "keep", []byte("data"), storage.ObjectMeta{})
	exists := func(key string) bool {
		_, err := e.backend.GetObjectInfo(context.Background(), "lan", key, "")
		return err == nil
	}
	crossSite := objHdr("Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site", "Content-Type", "text/plain")

	// PutObject and DeleteObject (no-cors fetch).
	if r := e.anon("PUT", "/lan/planted", []byte("x"), crossSite); r.StatusCode != 403 || exists("planted") {
		t.Errorf("cross-site anonymous PUT: %d %s", r.StatusCode, r.Data)
	}
	if r := e.anon("DELETE", "/lan/keep", nil, crossSite); r.StatusCode != 403 || !exists("keep") {
		t.Errorf("cross-site anonymous DELETE: %d %s", r.StatusCode, r.Data)
	}
	// DeleteObjects through an enctype=text/plain form.
	form := []byte(`<Delete><Object><Key>keep</Key></Object><X a="b"/></Delete>` + "\r\n")
	if r := e.anon("POST", "/lan?delete", form, objHdr("Origin", "null", "Content-Type", "text/plain")); r.StatusCode == 200 || !exists("keep") {
		t.Errorf("cross-site anonymous DeleteObjects: %d %s", r.StatusCode, r.Data)
	}
	// PostObject through an HTML form, detected by Origin alone (no
	// Sec-Fetch-Site).
	body, ct := objPostBody(t, []objPostField{{"key", "static/app.js"}}, "app.js", []byte("alert(1)"))
	if r := e.anon("POST", "/lan", body, objHdr("Content-Type", ct, "Origin", "http://attacker.example")); r.StatusCode != 403 || exists("static/app.js") {
		t.Errorf("cross-site anonymous PostObject: %d %s", r.StatusCode, r.Data)
	}

	// Not refused: a non-browser client, a same-origin page, a CORS rule
	// for the origin, a signed request.
	if r := e.anon("PUT", "/lan/cli", []byte("x"), nil); r.StatusCode != 200 {
		t.Errorf("anonymous PUT without Origin: %d %s", r.StatusCode, r.Data)
	}
	host := e.srv.Listener.Addr().String()
	if r := e.anon("PUT", "/lan/same", []byte("x"), objHdr("Origin", "http://"+host)); r.StatusCode != 200 {
		t.Errorf("same-origin anonymous PUT: %d %s", r.StatusCode, r.Data)
	}
	if r := e.anon("PUT", "/lan/same2", []byte("x"), objHdr("Origin", "https://evil.example", "Sec-Fetch-Site", "same-origin")); r.StatusCode != 200 {
		t.Errorf("Sec-Fetch-Site same-origin anonymous PUT: %d %s", r.StatusCode, r.Data)
	}
	if r := e.signed("PUT", "/lan/signed", []byte("x"), crossSite); r.StatusCode != 200 {
		t.Errorf("signed cross-site PUT: %d %s", r.StatusCode, r.Data)
	}
	e.putBucketConfig("lan", storage.ConfigCORS, `<CORSConfiguration><CORSRule><AllowedOrigin>https://app.example</AllowedOrigin>`+
		`<AllowedMethod>PUT</AllowedMethod><AllowedMethod>POST</AllowedMethod></CORSRule></CORSConfiguration>`)
	allowed := objHdr("Origin", "https://app.example", "Sec-Fetch-Site", "cross-site")
	if r := e.anon("PUT", "/lan/cors", []byte("x"), allowed); r.StatusCode != 200 {
		t.Errorf("anonymous PUT from a CORS-allowed origin: %d %s", r.StatusCode, r.Data)
	}
	if r := e.anon("POST", "/lan", body, objHdr("Content-Type", ct, "Origin", "https://app.example")); r.StatusCode/100 != 2 {
		t.Errorf("anonymous PostObject from a CORS-allowed origin: %d %s", r.StatusCode, r.Data)
	}
	if r := e.anon("PUT", "/lan/other", []byte("x"), crossSite); r.StatusCode != 403 {
		t.Errorf("anonymous PUT from an origin the bucket CORS does not allow: %d %s", r.StatusCode, r.Data)
	}
}

// TestAnonymousReadsResourcePolicy: anonymous reads carry
// Cross-Origin-Resource-Policy: same-site, so other websites cannot load
// LAN-only objects as scripts, styles or images (XSSI, existence probing)
// in a visitor's browser. Signed reads and buckets whose CORS
// configuration allows GET (the opt-in for cross-origin use) do not.
func TestAnonymousReadsResourcePolicy(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("lan")
	e.putBucketConfig("lan", storage.ConfigPolicy, lanPolicy)
	e.putObject("lan", "config.js", []byte("var CFG={token:'x'}"), storage.ObjectMeta{ContentType: "application/javascript"})

	for _, m := range []string{"GET", "HEAD"} {
		r := e.anon(m, "/lan/config.js", nil, objHdr("Sec-Fetch-Site", "cross-site", "Sec-Fetch-Dest", "script"))
		if r.StatusCode != 200 || r.Header.Get("Cross-Origin-Resource-Policy") != "same-site" {
			t.Errorf("anonymous %s: %d CORP %q", m, r.StatusCode, r.Header.Get("Cross-Origin-Resource-Policy"))
		}
	}
	// Denied reads carry it too (no load/error oracle).
	if r := e.anon("GET", "/lan/missing.js", nil, nil); r.StatusCode != 403 || r.Header.Get("Cross-Origin-Resource-Policy") != "same-site" {
		t.Errorf("anonymous denied GET: %d CORP %q", r.StatusCode, r.Header.Get("Cross-Origin-Resource-Policy"))
	}
	if r := e.signed("GET", "/lan/config.js", nil, nil); r.StatusCode != 200 || r.Header.Get("Cross-Origin-Resource-Policy") != "" {
		t.Errorf("signed GET: %d CORP %q", r.StatusCode, r.Header.Get("Cross-Origin-Resource-Policy"))
	}
	e.putBucketConfig("lan", storage.ConfigCORS, `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`)
	if r := e.anon("GET", "/lan/config.js", nil, nil); r.StatusCode != 200 || r.Header.Get("Cross-Origin-Resource-Policy") != "" {
		t.Errorf("anonymous GET, bucket CORS allows GET: %d CORP %q", r.StatusCode, r.Header.Get("Cross-Origin-Resource-Policy"))
	}
}

// TestNosniffOnEveryResponse: S3 responses are served from the web UI's
// origin; none (errors, listings, stored policies) may be content-sniffed.
func TestNosniffOnEveryResponse(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("b")
	e.putBucketConfig("b", storage.ConfigPolicy, lanPolicy)
	for _, r := range []*testResponse{
		e.signed("GET", "/b?policy", nil, nil),
		e.signed("GET", "/b?list-type=2", nil, nil),
		e.anon("GET", "/nobucket/x", nil, nil),
		e.signed("GET", "/", nil, nil),
	} {
		if r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: %d without nosniff", r.Request.Method, r.Request.URL, r.StatusCode)
		}
	}
}

// TestUnknownHostAnonymousRefused: requests marked by WithUnknownHost (a
// Host header that is not one of the server's names, as after DNS
// rebinding) get no anonymous access; signed requests are served.
func TestUnknownHostAnonymousRefused(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("lan")
	e.putBucketConfig("lan", storage.ConfigPolicy, lanPolicy)
	e.putObject("lan", "secret.txt", []byte("lan-only"), storage.ObjectMeta{})
	serve := func(signed bool) int {
		req := httptest.NewRequest("GET", "http://rebind.attacker.example:9000/lan/secret.txt", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		if signed {
			if err := signRoot(req); err != nil {
				t.Fatal(err)
			}
		}
		req = req.WithContext(WithUnknownHost(req.Context()))
		rec := httptest.NewRecorder()
		e.server.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := serve(false); code != http.StatusForbidden {
		t.Errorf("anonymous request for an unknown host: %d, want 403", code)
	}
	if code := serve(true); code != http.StatusOK {
		t.Errorf("signed request for an unknown host: %d, want 200", code)
	}
}
