package server

import (
	"strings"
	"testing"
)

// TestObjectsSandboxedOnUIOrigin checks, through the full server, that an
// object served on the embedded UI's origin cannot run script there: an
// uploaded text/html object (or one requested with response-content-type)
// is delivered with "Content-Security-Policy: sandbox" and nosniff, so it
// cannot read the console's stored connections and credentials.
func TestObjectsSandboxedOnUIOrigin(t *testing.T) {
	cfg := testConfig()
	cfg.UI.Enabled = true
	srv := start(t, cfg, WithUIFS(fakeUI()))
	if r := do(t, "PUT", srv.url+"/pages", nil, &rootCreds, nil); r.status != 200 {
		t.Fatalf("create bucket: %d %s", r.status, r.body)
	}
	page := []byte("<script>alert(localStorage.length)</script>")
	if r := do(t, "PUT", srv.url+"/pages/evil.html", page, &rootCreds, map[string]string{"Content-Type": "text/html"}); r.status != 200 {
		t.Fatalf("put: %d %s", r.status, r.body)
	}
	for _, target := range []string{"/pages/evil.html", "/pages/evil.html?response-content-type=image/svg%2Bxml"} {
		for _, method := range []string{"GET", "HEAD"} {
			r := do(t, method, srv.url+target, nil, &rootCreds, nil)
			if r.status != 200 {
				t.Fatalf("%s %s: %d", method, target, r.status)
			}
			if csp := r.header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
				t.Errorf("%s %s: Content-Security-Policy = %q", method, target, csp)
			}
			if r.header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s %s: no nosniff", method, target)
			}
		}
	}
}
