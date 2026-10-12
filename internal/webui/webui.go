// Package webui embeds the static build of the web UI (web/, `pnpm
// generate:embed`) and serves it under /_gos3/ui/ as a single-page
// application.
//
// The repository commits only a placeholder dist/index.html ("UI not built —
// run make ui") and dist/.keep; `make ui` replaces the contents of dist/ with
// web/.output/public and `make ui-clean` restores the placeholder (from
// placeholder.html). Built reports whether a real UI build is embedded.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"
)

// Prefix is the URL path the UI is served under (the Nuxt app.baseURL of
// the embedded build).
const Prefix = "/_gos3/ui/"

// placeholderMarker identifies the committed placeholder index.html.
const placeholderMarker = `name="gos3-ui-placeholder"`

//go:embed all:dist
var embedded embed.FS

// FS returns the embedded UI files (the contents of dist/).
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil { // cannot happen: dist is embedded
		panic(err)
	}
	return sub
}

// Built reports whether the embedded files are a real UI build rather than
// the placeholder.
func Built() bool { return IsBuilt(FS()) }

// IsBuilt reports whether fsys holds a UI build: an SPA fallback page
// (200.html), Nuxt assets (_nuxt/) or an index.html that is not the
// placeholder.
func IsBuilt(fsys fs.FS) bool {
	if fsys == nil {
		return false
	}
	if st, err := fs.Stat(fsys, "200.html"); err == nil && !st.IsDir() {
		return true
	}
	if st, err := fs.Stat(fsys, "_nuxt"); err == nil && st.IsDir() {
		return true
	}
	data, err := fs.ReadFile(fsys, "index.html")
	return err == nil && !bytes.Contains(data, []byte(placeholderMarker))
}

// Handler returns the handler for the embedded UI (see NewHandler).
func Handler() http.Handler { return NewHandler(FS()) }

// NewHandler serves the static UI in fsys under Prefix:
//
//   - "/_gos3/ui" redirects to "/_gos3/ui/"; only GET and HEAD are allowed.
//   - Existing files are served with their content type; a directory serves
//     its index.html.
//   - Unknown paths are client-side routes of the SPA and get the fallback
//     page (200.html, else index.html) with status 200 — except under
//     _nuxt/ (hashed build assets), where a missing file is a 404 so a stale
//     page never receives HTML instead of JavaScript.
//   - _nuxt/ assets are immutable (Cache-Control: public, max-age=1y,
//     immutable); everything else, HTML in particular, is "no-cache" and
//     revalidated with a content ETag.
//   - Every response carries X-Content-Type-Options: nosniff,
//     Referrer-Policy: no-referrer, X-Frame-Options: DENY and
//     Content-Security-Policy: frame-ancestors 'none'.
//
// Requests whose path does not start with Prefix get a 404.
func NewHandler(fsys fs.FS) http.Handler {
	return &handler{fsys: fsys}
}

type handler struct {
	fsys  fs.FS
	etags sync.Map // file name -> quoted ETag (the files never change)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Content-Security-Policy", "frame-ancestors 'none'")

	p := r.URL.Path
	if p+"/" == Prefix {
		target := Prefix
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	if !strings.HasPrefix(p, Prefix) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hdr.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rel := strings.TrimPrefix(p, Prefix)
	isDir := rel == "" || strings.HasSuffix(rel, "/")
	name := strings.TrimPrefix(path.Clean("/"+rel), "/")
	if isDir {
		name = path.Join(name, "index.html")
	}
	if name == "" || name == "." {
		name = "index.html"
	}
	assetPath := name == "_nuxt" || strings.HasPrefix(name, "_nuxt/")

	if h.serveFile(w, r, name, assetPath) {
		return
	}
	// A directory requested without the trailing slash.
	if !isDir {
		if st, err := fs.Stat(h.fsys, name); err == nil && st.IsDir() && h.serveFile(w, r, path.Join(name, "index.html"), assetPath) {
			return
		}
	}
	if assetPath {
		hdr.Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	// SPA fallback for client-side routes.
	for _, fallback := range []string{"200.html", "index.html"} {
		if h.serveFile(w, r, fallback, false) {
			return
		}
	}
	http.NotFound(w, r)
}

// serveFile serves the regular file name, reporting false (without writing
// anything) when it does not exist or is a directory.
func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string, immutable bool) bool {
	if !fs.ValidPath(name) {
		return false
	}
	f, err := h.fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	var content io.ReadSeeker
	if rs, ok := f.(io.ReadSeeker); ok {
		content = rs
	} else {
		data, err := io.ReadAll(f)
		if err != nil {
			return false
		}
		content = bytes.NewReader(data)
	}
	etag, err := h.etag(name, content)
	if err != nil {
		return false
	}

	hdr := w.Header()
	hdr.Set("Content-Type", contentType(name))
	hdr.Set("ETag", etag)
	if immutable {
		hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		hdr.Set("Cache-Control", "no-cache")
	}
	// The embedded files have no modification time; the ETag drives
	// conditional requests.
	http.ServeContent(w, r, name, time.Time{}, content)
	return true
}

// etag returns the cached content ETag of name, hashing content (and
// rewinding it) on first use.
func (h *handler) etag(name string, content io.ReadSeeker) (string, error) {
	if v, ok := h.etags.Load(name); ok {
		return v.(string), nil
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, content); err != nil {
		return "", err
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tag := `"` + base64.RawURLEncoding.EncodeToString(sum.Sum(nil)[:18]) + `"`
	h.etags.Store(name, tag)
	return tag, nil
}

// contentTypes covers the file types of a Nuxt build independently of the
// host's mime.types.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".wasm":        "application/wasm",
	".txt":         "text/plain; charset=utf-8",
	".xml":         "application/xml",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
