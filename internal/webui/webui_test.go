package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func fakeDist() fstest.MapFS {
	return fstest.MapFS{
		"200.html":               {Data: []byte("<!doctype html><title>spa</title>")},
		"index.html":             {Data: []byte("<!doctype html><title>index</title>")},
		"favicon.svg":            {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
		"_nuxt/entry.abc123.js":  {Data: []byte("console.log('hi')")},
		"_nuxt/entry.abc123.css": {Data: []byte("body{}")},
		"docs/index.html":        {Data: []byte("<p>docs</p>")},
	}
}

func get(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandlerServesFilesAndFallback(t *testing.T) {
	h := NewHandler(fakeDist())
	tests := []struct {
		target, wantBody, wantType, wantCache string
		wantStatus                            int
	}{
		{"/_gos3/ui/", "index", "text/html; charset=utf-8", "no-cache", 200},
		{"/_gos3/ui/index.html", "index", "text/html; charset=utf-8", "no-cache", 200},
		{"/_gos3/ui/servers/abc/buckets/photos", "spa", "text/html; charset=utf-8", "no-cache", 200},
		{"/_gos3/ui/servers/abc/buckets/b/objects/report.pdf", "spa", "text/html; charset=utf-8", "no-cache", 200},
		{"/_gos3/ui/favicon.svg", "<svg", "image/svg+xml", "no-cache", 200},
		{"/_gos3/ui/_nuxt/entry.abc123.js", "console.log", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 200},
		{"/_gos3/ui/_nuxt/entry.abc123.css", "body{}", "text/css; charset=utf-8", "public, max-age=31536000, immutable", 200},
		{"/_gos3/ui/_nuxt/missing.js", "404", "", "no-store", 404},
		{"/_gos3/ui/docs", "docs", "text/html; charset=utf-8", "no-cache", 200},
		{"/_gos3/ui/docs/", "docs", "text/html; charset=utf-8", "no-cache", 200},
		{"/_gos3/ui/../../etc/passwd", "spa", "text/html; charset=utf-8", "no-cache", 200},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			w := get(t, h, "GET", tt.target, nil)
			if w.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d (body %q)", w.Code, tt.wantStatus, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantBody) {
				t.Errorf("body %q lacks %q", w.Body.String(), tt.wantBody)
			}
			if tt.wantType != "" && w.Header().Get("Content-Type") != tt.wantType {
				t.Errorf("Content-Type %q, want %q", w.Header().Get("Content-Type"), tt.wantType)
			}
			if got := w.Header().Get("Cache-Control"); got != tt.wantCache {
				t.Errorf("Cache-Control %q, want %q", got, tt.wantCache)
			}
			for k, v := range map[string]string{
				"X-Content-Type-Options": "nosniff",
				"X-Frame-Options":        "DENY",
				"Referrer-Policy":        "no-referrer",
			} {
				if w.Header().Get(k) != v {
					t.Errorf("%s = %q, want %q", k, w.Header().Get(k), v)
				}
			}
			if csp := w.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'self'; script-src 'self';") || !strings.HasSuffix(csp, "; frame-ancestors 'none'") {
				t.Errorf("Content-Security-Policy = %q", csp)
			}
		})
	}
}

func TestHandlerRedirectMethodsAndConditional(t *testing.T) {
	h := NewHandler(fakeDist())

	w := get(t, h, "GET", "/_gos3/ui?x=1", nil)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/_gos3/ui/?x=1" {
		t.Fatalf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}

	w = get(t, h, "POST", "/_gos3/ui/", nil)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d allow=%q", w.Code, w.Header().Get("Allow"))
	}

	w = get(t, h, "HEAD", "/_gos3/ui/favicon.svg", nil)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d len=%d cl=%q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}

	w = get(t, h, "GET", "/_gos3/ui/_nuxt/entry.abc123.js", nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	w = get(t, h, "GET", "/_gos3/ui/_nuxt/entry.abc123.js", map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusNotModified {
		t.Fatalf("conditional GET: %d", w.Code)
	}

	w = get(t, h, "GET", "/other", nil)
	if w.Code != 404 {
		t.Fatalf("outside prefix: %d", w.Code)
	}
}

func TestFallbackToIndexWithout200(t *testing.T) {
	fsys := fakeDist()
	delete(fsys, "200.html")
	w := get(t, NewHandler(fsys), "GET", "/_gos3/ui/connect", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "index") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestEmbeddedPlaceholder(t *testing.T) {
	if FS() == nil {
		if Built() || get(t, Handler(), "GET", "/_gos3/ui/", nil).Code != 404 {
			t.Fatal("UI is available in a build without embedded assets")
		}
		return
	}
	// The committed tree holds the placeholder; a developer tree may hold a
	// real build. Either way the root must answer with HTML.
	w := get(t, Handler(), "GET", "/_gos3/ui/", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("embedded root: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body, _ := io.ReadAll(w.Body)
	if !Built() && !strings.Contains(string(body), "make ui") {
		t.Fatalf("placeholder body %q", body)
	}
}

func TestIsBuilt(t *testing.T) {
	if !IsBuilt(fakeDist()) {
		t.Error("fake dist not built")
	}
	placeholder := fstest.MapFS{
		"index.html": {Data: []byte(`<meta name="gos3-ui-placeholder" content="true">`)},
		".keep":      {},
	}
	if IsBuilt(placeholder) {
		t.Error("placeholder reported as built")
	}
	if !IsBuilt(fstest.MapFS{"index.html": {Data: []byte("<html>app</html>")}}) {
		t.Error("plain index.html build not detected")
	}
	if IsBuilt(nil) {
		t.Error("nil fs built")
	}
}
