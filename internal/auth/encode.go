package auth

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const upperHex = "0123456789ABCDEF"

// isUnreserved reports whether c is an RFC 3986 unreserved character, the
// only bytes SigV4 leaves unencoded (besides '/' in paths).
func isUnreserved(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// uriEncode encodes s with the AWS SigV4 rules: unreserved characters are
// kept, every other byte becomes %XX (uppercase hex). '/' is kept when
// keepSlash is set (paths) and encoded otherwise (query keys/values).
func uriEncode(s string, keepSlash bool) string {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isUnreserved(c) && !(keepSlash && c == '/') {
			n++
		}
	}
	if n == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 2*n)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) || (keepSlash && c == '/') {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&15])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// percentDecode decodes %XX escapes leniently: a '%' that does not start a
// valid escape is kept literally (so it is re-encoded as %25). When plusSpace
// is set, '+' decodes to ' ' (query-string semantics).
func percentDecode(s string, plusSpace bool) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s):
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
			b.WriteByte(c)
		case c == '+' && plusSpace:
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// canonicalURIPath returns the SigV4 canonical URI for S3 from the escaped
// request path as sent by the client: the path is split on literal '/',
// each segment is percent-decoded and re-encoded with the AWS rules. No
// path normalisation (".", "..", "//") is performed, as S3 requires.
// An escaped "%2F" stays "%2F" because splitting happens before decoding.
func canonicalURIPath(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	segs := strings.Split(escapedPath, "/")
	for i, s := range segs {
		segs[i] = uriEncode(percentDecode(s, false), false)
	}
	p := strings.Join(segs, "/")
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	return p
}

// requestEscapedPath returns the path of r exactly as the client sent it:
// from RequestURI (server side; unaffected by handlers rewriting r.URL),
// else from the URL (client side / synthetic requests).
func requestEscapedPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		p, _, _ := strings.Cut(r.RequestURI, "?")
		return p
	}
	return urlEscapedPath(r)
}

// urlEscapedPath mirrors how Go's http client writes the request path.
func urlEscapedPath(r *http.Request) string {
	return escapedURLPath(r.URL)
}

// escapedURLPath returns the escaped path of u as Go's http client would
// send it ("/" when empty), following the AWS SDK rules for opaque URLs.
func escapedURLPath(u *url.URL) string {
	if u == nil {
		return "/"
	}
	if op := u.Opaque; op != "" {
		if i := strings.IndexByte(op, '?'); i >= 0 {
			op = op[:i]
		}
		if rest, ok := strings.CutPrefix(op, "//"); ok {
			// "//host/path" form: the path starts at the first '/'.
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return rest[i:]
			}
			return "/"
		}
		if op == "" || op[0] != '/' {
			return "/" + op
		}
		return op
	}
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

// requestRawQuery returns the raw query string as sent by the client.
func requestRawQuery(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		_, q, _ := strings.Cut(r.RequestURI, "?")
		return q
	}
	if r.URL == nil {
		return ""
	}
	return r.URL.RawQuery
}

// queryParam is one decoded query parameter.
type queryParam struct {
	key, value string
}

// maxQueryParams bounds the number of query parameters considered.
const maxQueryParams = 1000

// parseRawQuery splits a raw query into decoded key/value pairs in order.
// Empty pieces ("a&&b") are skipped; a piece without '=' has an empty value.
// '+' decodes to a space (as net/url does). Invalid escapes are kept literally.
func parseRawQuery(raw string) ([]queryParam, bool) {
	if raw == "" {
		return nil, true
	}
	var out []queryParam
	for raw != "" {
		var piece string
		piece, raw, _ = strings.Cut(raw, "&")
		if piece == "" {
			continue
		}
		if len(out) >= maxQueryParams {
			return nil, false
		}
		k, v, _ := strings.Cut(piece, "=")
		out = append(out, queryParam{key: percentDecode(k, true), value: percentDecode(v, true)})
	}
	return out, true
}

// canonicalQueryString builds the SigV4 canonical query string from decoded
// parameters, excluding keys in exclude.
func canonicalQueryString(params []queryParam, exclude string) string {
	type enc struct{ k, v string }
	list := make([]enc, 0, len(params))
	for _, p := range params {
		if exclude != "" && p.key == exclude {
			continue
		}
		list = append(list, enc{uriEncode(p.key, false), uriEncode(p.value, false)})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].k != list[j].k {
			return list[i].k < list[j].k
		}
		return list[i].v < list[j].v
	})
	var b strings.Builder
	for i, e := range list {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(e.k)
		b.WriteByte('=')
		b.WriteString(e.v)
	}
	return b.String()
}

// canonicalHeaderValue trims the spaces and tabs around v and collapses
// runs of spaces into one, as SigV4 requires. Other (Unicode) whitespace is
// part of the signed value: trimming it too would let anyone holding a
// presigned URL append, say, U+00A0 to a signed header without breaking
// the signature.
func canonicalHeaderValue(v string) string {
	v = strings.Trim(v, " \t")
	if !strings.Contains(v, "  ") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	space := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteByte(c)
	}
	return b.String()
}
