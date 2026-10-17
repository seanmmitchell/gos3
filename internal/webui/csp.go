package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The Content-Security-Policy of every UI response. The UI keeps S3 secret
// keys in Web Storage on the same origin, so the policy is what contains a
// script-injection bug (for example in the Markdown preview) instead of
// letting it read those keys:
//
//   - script-src allows the UI's own files plus the SHA-256 hashes of the
//     inline scripts of the embedded build (Nuxt's import map, its runtime
//     config and the color-mode bootstrap), computed from the HTML at startup
//     so a rebuilt UI keeps working without a manifest. No 'unsafe-inline',
//     no 'unsafe-eval'.
//   - connect-src must stay open to any http(s) origin: the UI talks to every
//     S3 endpoint the user adds (other gos3 servers, AWS, …), cross-origin.
//     Exfiltration through fetch is therefore not blocked; the policy's job
//     is to keep injected markup from running script at all.
//   - img-src / media-src allow any http(s) origin and blob:/data: for the
//     object previews (presigned URLs of any endpoint).
//   - style-src needs 'unsafe-inline': the loading screen and Nuxt UI use
//     inline styles, and styles cannot run script.
//   - object-src, base-uri and form-action are 'none' (the UI never submits a
//     form natively); frame-ancestors 'none' forbids framing.
const (
	cspBefore = "default-src 'self'; script-src 'self'"
	cspAfter  = "; style-src 'self' 'unsafe-inline'" +
		"; img-src 'self' data: blob: http: https:" +
		"; media-src 'self' blob: http: https:" +
		"; font-src 'self' data:" +
		"; connect-src 'self' http: https:" +
		"; worker-src 'self'" +
		"; manifest-src 'self'" +
		"; object-src 'none'" +
		"; base-uri 'none'" +
		"; form-action 'none'" +
		"; frame-ancestors 'none'"
)

// contentSecurityPolicy returns the policy allowing the inline scripts of
// every HTML file in fsys. It warns when an inline script carries a preset
// secret key (presets baked in through NUXT_PUBLIC_SERVERS at build time):
// unlike servers.json, that cannot be removed without breaking the page.
func contentSecurityPolicy(fsys fs.FS, log *slog.Logger) string {
	hashes, secretPages := inlineScriptHashes(fsys)
	for _, page := range secretPages {
		log.Warn("web UI: an inline script of the embedded build contains a secret key field (NUXT_PUBLIC_SERVERS at build time?); the UI files are public, rebuild without secrets", "page", page)
	}
	var b strings.Builder
	b.WriteString(cspBefore)
	for _, h := range hashes {
		b.WriteString(" '")
		b.WriteString(h)
		b.WriteString("'")
	}
	b.WriteString(cspAfter)
	return b.String()
}

// inlineScriptHashes returns the sorted, de-duplicated CSP hash sources
// ("sha256-<base64>") of the inline scripts in all HTML files of fsys.
// Unreadable files are skipped: a page that cannot be read cannot be served
// either. secretPages lists the pages with an inline script that mentions a
// preset secret field.
func inlineScriptHashes(fsys fs.FS) (hashes, secretPages []string) {
	if fsys == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	_ = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // keep walking the rest of the tree
		}
		if d.IsDir() {
			// Hashed build assets are JavaScript/CSS, never HTML pages.
			if name == "_nuxt" {
				return fs.SkipDir
			}
			return nil
		}
		if ext := strings.ToLower(path.Ext(name)); ext != ".html" && ext != ".htm" {
			return nil
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil
		}
		secret := false
		for _, script := range inlineScripts(data) {
			seen[scriptHash(script)] = true
			for _, field := range presetSecretFields {
				secret = secret || bytes.Contains(script, []byte(field))
			}
		}
		if secret {
			secretPages = append(secretPages, name)
		}
		return nil
	})
	hashes = make([]string, 0, len(seen))
	for h := range seen {
		hashes = append(hashes, h)
	}
	slices.Sort(hashes)
	return hashes, secretPages
}

// scriptHash is the CSP hash source of an inline script's text.
func scriptHash(script []byte) string {
	sum := sha256.Sum256(script)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// inlineScripts returns the text of the <script> elements in page that
// script-src governs: elements without src whose type is classic or module
// JavaScript or an import map. Data blocks (type="application/json", e.g.
// Nuxt's __NUXT_DATA__) are never executed and need no hash. The text is
// what browsers hash: the element content as the HTML parser sees it, i.e.
// after input-stream preprocessing (CRLF and lone CR become LF) and with
// NUL replaced by U+FFFD (script data state), but without entity decoding.
func inlineScripts(page []byte) [][]byte {
	var out [][]byte
	z := html.NewTokenizer(bytes.NewReader(page))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken:
			name, hasAttr := z.TagName()
			if atom.Lookup(name) != atom.Script {
				continue
			}
			external, typ := false, ""
			for hasAttr {
				var key, val []byte
				key, val, hasAttr = z.TagAttr()
				switch string(key) {
				case "src":
					external = true
				case "type":
					typ = string(val)
				}
			}
			// The tokenizer reads the element's content as one raw text
			// token (possibly empty, then the next token is the end tag).
			if z.Next() != html.TextToken {
				if !external && executableScriptType(typ) {
					out = append(out, []byte{})
				}
				continue
			}
			if !external && executableScriptType(typ) {
				// Text normalizes newlines and does not unescape raw-text
				// elements such as <script>; it returns a view of the
				// tokenizer's buffer, so copy it.
				text := bytes.Clone(z.Text())
				out = append(out, bytes.ReplaceAll(text, []byte{0}, []byte("\uFFFD")))
			}
		}
	}
}

// executableScriptType reports whether a <script type> value makes the
// element something CSP script-src applies to.
func executableScriptType(typ string) bool {
	t := strings.ToLower(strings.TrimSpace(typ))
	switch t {
	case "", "module", "importmap", "speculationrules":
		return true
	}
	// The JavaScript MIME type essence matches (HTML spec), with or without
	// parameters.
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch t {
	case "text/javascript", "application/javascript", "application/ecmascript",
		"application/x-ecmascript", "application/x-javascript", "text/ecmascript",
		"text/javascript1.0", "text/javascript1.1", "text/javascript1.2",
		"text/javascript1.3", "text/javascript1.4", "text/javascript1.5",
		"text/jscript", "text/livescript", "text/x-ecmascript", "text/x-javascript":
		return true
	}
	return false
}
