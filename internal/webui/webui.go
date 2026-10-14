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
	"encoding/base64"
	"io"
	"io/fs"
	"log/slog"
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
func Handler() http.Handler {
	if FS() == nil {
		return http.NotFoundHandler()
	}
	return NewHandler(FS())
}

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
//     Referrer-Policy: no-referrer, X-Frame-Options: DENY and a
//     Content-Security-Policy that only runs the UI's own scripts: its files
//     and the inline scripts of its HTML pages, whose hashes NewHandler
//     computes once from fsys (see contentSecurityPolicy).
//   - servers.json (preset connections) is served without secret keys: every
//     UI file is public, so NewHandler strips them and logs a warning; an
//     invalid servers.json is not served.
//
// Requests whose path does not start with Prefix get a 404. Warnings go to
// slog.Default().
func NewHandler(fsys fs.FS) http.Handler {
	log := slog.Default()
	h := &handler{fsys: fsys, csp: contentSecurityPolicy(fsys, log)}
	if data, ok := sanitizedPresets(fsys, log); ok {
		h.overrides = map[string][]byte{presetsFile: data}
	}
	return h
}

type handler struct {
	fsys fs.FS
	csp  string // Content-Security-Policy of every response
	// overrides replaces the content of files of fsys (a nil slice hides
	// the file). Keys are lower case and matched case-insensitively, so a
	// case-insensitive fsys (os.DirFS on Windows or macOS) cannot serve
	// the original under another spelling.
	overrides map[string][]byte
	etags     sync.Map // file name -> quoted ETag (the files never change)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Content-Security-Policy", h.csp)

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
	var content io.ReadSeeker
	if data, ok := h.overrides[strings.ToLower(name)]; ok {
		if data == nil {
			return false
		}
		content = bytes.NewReader(data)
	} else if f, err := h.fsys.Open(name); err != nil {
		return false
	} else {
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			return false
		}
		content, err = readSeeker(f)
		if err != nil {
			return false
		}
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

// readSeeker returns f itself when it can seek, else its content in memory.
func readSeeker(f fs.File) (io.ReadSeeker, error) {
	if rs, ok := f.(io.ReadSeeker); ok {
		return rs, nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
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
