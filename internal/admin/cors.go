package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
)

// corsMethods are the methods the admin API accepts cross-origin.
const corsMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"

// corsOriginAllowed reports whether origin matches one of the allowed
// origins: "*" (any), an exact origin (scheme and host compared
// case-insensitively) or a wildcard-subdomain origin such as
// "https://*.example.com" (matches "https://a.example.com" and
// "https://a.b.example.com", not "https://example.com").
func corsOriginAllowed(origin string, allowed []string) bool {
	if origin == "" {
		return false
	}
	o := strings.ToLower(origin)
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "":
		case a == "*":
			return true
		case a == o:
			return true
		case strings.Contains(a, "://*."):
			i := strings.Index(a, "://*.")
			scheme, rest := a[:i+3], a[i+4:] // rest starts with "."
			if !strings.HasPrefix(o, scheme) {
				continue
			}
			host := o[len(scheme):]
			if len(host) > len(rest) && strings.HasSuffix(host, rest) &&
				!strings.ContainsAny(host[:len(host)-len(rest)], "/:@") {
				return true
			}
		}
	}
	return false
}

// applyCORS adds CORS headers to an actual (non-preflight) response when the
// request's Origin is allowed.
func applyCORS(w http.ResponseWriter, r *http.Request, c config.CORSConfig) {
	h := w.Header()
	h.Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if !corsOriginAllowed(origin, c.AllowedOrigins) {
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	if exp := joinNonEmpty(c.ExposeHeaders); exp != "" {
		h.Set("Access-Control-Expose-Headers", exp)
	}
}

// preflight answers an OPTIONS request without authentication.
func (a *api) preflight(w http.ResponseWriter, r *http.Request, c config.CORSConfig) {
	h := w.Header()
	h.Add("Vary", "Origin")
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	origin := r.Header.Get("Origin")
	reqMethod := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || reqMethod == "" {
		// Not a CORS preflight: describe the API's methods.
		h.Set("Allow", corsMethods)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !corsOriginAllowed(origin, c.AllowedOrigins) {
		writeError(w, errorf(http.StatusForbidden, codeAccessDenied,
			"CORS preflight rejected: origin %q is not allowed (configure cors.allowedOrigins)", origin))
		return
	}
	if !strings.Contains(", "+corsMethods+",", ", "+strings.ToUpper(strings.TrimSpace(reqMethod))+",") {
		writeError(w, errorf(http.StatusForbidden, codeAccessDenied,
			"CORS preflight rejected: method %q is not allowed", reqMethod))
		return
	}
	var reqHeaders []string
	for _, v := range r.Header.Values("Access-Control-Request-Headers") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				reqHeaders = append(reqHeaders, strings.ToLower(f))
			}
		}
	}
	for _, rh := range reqHeaders {
		if !corsHeaderAllowed(rh, c.AllowedHeaders) {
			writeError(w, errorf(http.StatusForbidden, codeAccessDenied,
				"CORS preflight rejected: header %q is not allowed (configure cors.allowedHeaders)", rh))
			return
		}
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", corsMethods)
	if len(reqHeaders) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(reqHeaders, ", "))
	}
	if exp := joinNonEmpty(c.ExposeHeaders); exp != "" {
		h.Set("Access-Control-Expose-Headers", exp)
	}
	if c.MaxAgeSeconds > 0 {
		h.Set("Access-Control-Max-Age", strconv.Itoa(c.MaxAgeSeconds))
	}
	w.WriteHeader(http.StatusNoContent)
}

// corsHeaderAllowed matches a (lower-cased) request header against the
// allowed header patterns ("*" wildcards allowed, case-insensitive).
func corsHeaderAllowed(name string, allowed []string) bool {
	for _, p := range allowed {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" && wildcardMatch(p, name) {
			return true
		}
	}
	return false
}

// wildcardMatch matches s against pattern where '*' matches any sequence.
func wildcardMatch(pattern, s string) bool {
	// Iterative glob with backtracking on the last '*'; O(len(p)*len(s)).
	p, i, star, mark := 0, 0, -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

func joinNonEmpty(ss []string) string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}
