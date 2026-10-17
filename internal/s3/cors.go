package s3

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// CORS configuration limits (as AWS).
const (
	maxCORSRules      = 100
	maxCORSConfigSize = 64 << 10
	maxCORSRuleIDLen  = 255
	// Preflight is unauthenticated. Bound its attacker-controlled header list
	// before parsing it or checking it against every configured CORS rule.
	maxCORSRequestHeadersSize  = 8 << 10
	maxCORSRequestHeadersCount = 64
)

// corsMethods are the methods a CORSRule may allow.
var corsMethods = map[string]bool{"GET": true, "PUT": true, "HEAD": true, "POST": true, "DELETE": true}

// defaultCORSExposeHeaders are exposed by the global CORS rule when the
// settings do not list any.
var defaultCORSExposeHeaders = []string{
	"ETag", "Content-Length", "Content-Type", "Content-Range", "Last-Modified",
	"x-amz-request-id", "x-amz-id-2", "x-amz-version-id", "x-amz-delete-marker",
	"x-amz-checksum-crc32", "x-amz-checksum-crc32c", "x-amz-checksum-crc64nvme",
	"x-amz-checksum-sha1", "x-amz-checksum-sha256", "x-amz-checksum-type",
	"x-amz-mp-parts-count", "x-amz-tagging-count", "x-amz-storage-class",
	"x-amz-server-side-encryption", "x-amz-object-lock-mode",
	"x-amz-object-lock-retain-until-date", "x-amz-object-lock-legal-hold",
}

// CORSConfiguration is the <CORSConfiguration> document
// (storage.ConfigCORS). Set XMLNS to s3Namespace when writing it.
type CORSConfiguration struct {
	XMLName xml.Name   `xml:"CORSConfiguration"`
	XMLNS   string     `xml:"xmlns,attr,omitempty"`
	Rules   []CORSRule `xml:"CORSRule"`

	// global marks the configuration built from the server-wide settings
	// (globalCORS); its "*" origin only serves signed requests (see
	// corsSignedOnly).
	global bool
}

// CORSRule is one <CORSRule>.
type CORSRule struct {
	ID             string   `xml:"ID,omitempty"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedHeaders []string `xml:"AllowedHeader,omitempty"`
	ExposeHeaders  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds,omitempty"`
}

// parseCORSConfiguration parses and validates a CORSConfiguration document
// (PutBucketCors body or stored config): MalformedXML for structural
// problems (no rules, more than 100 rules, a rule without AllowedMethod or
// AllowedOrigin, oversized document), InvalidRequest for unsupported
// methods, multiple wildcards in an origin or header, and values that are
// not valid in the response headers they end up in: AllowedHeader and
// ExposeHeader must be HTTP header names (tokens; AllowedHeader may hold a
// '*' wildcard), AllowedOrigin may not contain spaces or control
// characters and ID no control characters. Without the character checks
// a stored CR/LF would reach Access-Control-* response headers, where only
// the HTTP stack's own sanitising stops header injection.
func parseCORSConfiguration(data []byte) (*CORSConfiguration, error) {
	if len(data) > maxCORSConfigSize {
		return nil, s3err.ErrMaxMessageLengthExceeded
	}
	var c CORSConfiguration
	if err := decodeXML(data, &c); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate checks the rules (see parseCORSConfiguration).
func (c *CORSConfiguration) validate() error {
	if len(c.Rules) == 0 || len(c.Rules) > maxCORSRules {
		return s3err.ErrMalformedXML
	}
	for i := range c.Rules {
		r := &c.Rules[i]
		r.ID = strings.TrimSpace(r.ID)
		if len(r.ID) > maxCORSRuleIDLen {
			return s3err.ErrInvalidArgument.WithMessage("The ID of a CORS rule must be at most 255 characters.")
		}
		if strings.ContainsFunc(r.ID, isControl) {
			return s3err.ErrInvalidRequest.WithMessage("The ID of a CORS rule may not contain control characters.")
		}
		if len(r.AllowedMethods) == 0 || len(r.AllowedOrigins) == 0 {
			return s3err.ErrMalformedXML
		}
		for j, m := range r.AllowedMethods {
			m = strings.TrimSpace(m)
			if !corsMethods[m] {
				return s3err.ErrInvalidRequest.WithMessage("Found unsupported HTTP method in CORS config. Unsupported method is %s", m)
			}
			r.AllowedMethods[j] = m
		}
		for j, o := range r.AllowedOrigins {
			o = strings.TrimSpace(o)
			if o == "" || strings.Count(o, "*") > 1 {
				return s3err.ErrInvalidRequest.WithMessage("AllowedOrigin %q can not have more than one wildcard.", o)
			}
			if strings.ContainsFunc(o, func(c rune) bool { return c == ' ' || isControl(c) }) {
				return s3err.ErrInvalidRequest.WithMessage("AllowedOrigin %q contains invalid characters.", o)
			}
			r.AllowedOrigins[j] = o
		}
		for j, h := range r.AllowedHeaders {
			h = strings.TrimSpace(h)
			if strings.Count(h, "*") > 1 {
				return s3err.ErrInvalidRequest.WithMessage("AllowedHeader %q can not have more than one wildcard.", h)
			}
			if h != "" && !httpguts.ValidHeaderFieldName(h) {
				return s3err.ErrInvalidRequest.WithMessage("AllowedHeader %q is not a valid header name.", h)
			}
			r.AllowedHeaders[j] = h
		}
		for j, h := range r.ExposeHeaders {
			h = strings.TrimSpace(h)
			if strings.Contains(h, "*") {
				return s3err.ErrInvalidRequest.WithMessage("ExposeHeader %q contains wildcard. We currently do not support wildcard for ExposeHeader.", h)
			}
			if h != "" && !httpguts.ValidHeaderFieldName(h) {
				return s3err.ErrInvalidRequest.WithMessage("ExposeHeader %q is not a valid header name.", h)
			}
			r.ExposeHeaders[j] = h
		}
		if r.MaxAgeSeconds != nil && *r.MaxAgeSeconds < 0 {
			return s3err.ErrMalformedXML
		}
	}
	return nil
}

// isControl reports whether c is an ASCII control character (including
// DEL), which HTTP header values may not carry (horizontal tab aside).
func isControl(c rune) bool { return c < 0x20 || c == 0x7f }

// match returns the first rule allowing origin, method and every requested
// header (nil if none).
func (c *CORSConfiguration) match(origin, method string, headers []string) *CORSRule {
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.allowsOrigin(origin) && r.allowsMethod(method) && r.allowsHeaders(headers) {
			return r
		}
	}
	return nil
}

func (r *CORSRule) allowsOrigin(origin string) bool {
	for _, o := range r.AllowedOrigins {
		if wildcardMatchFold(o, origin) {
			return true
		}
	}
	return false
}

func (r *CORSRule) allowsMethod(method string) bool {
	for _, m := range r.AllowedMethods {
		if m == method {
			return true
		}
	}
	return false
}

func (r *CORSRule) allowsHeaders(headers []string) bool {
	for _, h := range headers {
		ok := false
		for _, a := range r.AllowedHeaders {
			if wildcardMatchFold(a, h) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// allowsOriginOnlyByWildcard reports whether the rule allows origin only
// through a bare "*" entry (no other pattern of the rule matches it).
func (r *CORSRule) allowsOriginOnlyByWildcard(origin string) bool {
	star := false
	for _, o := range r.AllowedOrigins {
		if o == "*" {
			star = true
		} else if wildcardMatchFold(o, origin) {
			return false
		}
	}
	return star
}

// wildcardOrigin reports whether the rule allows any origin ("*").
func (r *CORSRule) wildcardOrigin() bool {
	for _, o := range r.AllowedOrigins {
		if o == "*" {
			return true
		}
	}
	return false
}

// wildcardMatchFold matches s against a pattern with at most one '*'
// (case-insensitive).
func wildcardMatchFold(pattern, s string) bool {
	i := strings.IndexByte(pattern, '*')
	if i < 0 {
		return strings.EqualFold(pattern, s)
	}
	prefix, suffix := pattern[:i], pattern[i+1:]
	return len(s) >= len(prefix)+len(suffix) &&
		strings.EqualFold(s[:len(prefix)], prefix) &&
		strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

// globalCORS converts the server-wide CORS settings into a configuration
// with a single rule allowing every S3 method (nil when no origin is
// configured).
func globalCORS(c config.CORSConfig) *CORSConfiguration {
	var origins []string
	for _, o := range c.AllowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	if len(origins) == 0 {
		return nil
	}
	headers := c.AllowedHeaders
	if len(headers) == 0 {
		headers = []string{"*"}
	}
	expose := c.ExposeHeaders
	if len(expose) == 0 {
		expose = defaultCORSExposeHeaders
	}
	maxAge := c.MaxAgeSeconds
	if maxAge <= 0 {
		maxAge = 3600
	}
	return &CORSConfiguration{global: true, Rules: []CORSRule{{
		AllowedMethods: []string{"GET", "PUT", "HEAD", "POST", "DELETE"},
		AllowedOrigins: origins,
		AllowedHeaders: headers,
		ExposeHeaders:  expose,
		MaxAgeSeconds:  &maxAge,
	}}}
}

// corsConfigFor returns the CORS configuration that applies to a request:
// the bucket's when it has one, else the global settings (also used for
// service-level requests and nonexistent buckets). Nil when neither exists.
//
// A stored bucket configuration that no longer validates (it was stored
// before a validation rule was added, such as the header-name and origin
// character checks) disables CORS for the bucket: no CORS headers are
// sent and preflights are refused, as if the bucket had a configuration
// that matches nothing. It neither fails the bucket's requests nor falls
// back to the global rules, which could allow more than the bucket owner
// configured. A warning is logged each time the document is parsed (once
// per stored version, the result is cached); PutBucketCors or
// DeleteBucketCors repairs it.
func (s *Server) corsConfigFor(ctx context.Context, bucket string, settings config.Settings) *CORSConfiguration {
	if bucket != "" {
		raw, err := s.backend.GetBucketConfig(ctx, bucket, storage.ConfigCORS)
		if err == nil && len(raw) > 0 {
			v, perr := s.configs.get(bucket, storage.ConfigCORS, raw, func(b []byte) (any, error) {
				c, err := parseCORSConfiguration(b)
				if err != nil {
					s.log.LogAttrs(ctx, slog.LevelWarn, "s3: stored bucket CORS configuration is invalid; CORS is disabled for the bucket",
						slog.String("bucket", bucket), slog.String("error", err.Error()))
				}
				return c, err
			})
			if perr != nil {
				return nil
			}
			return v.(*CORSConfiguration)
		}
	}
	return globalCORS(settings.CORS)
}

// corsSignedOnly reports whether a CORS response for origin must be
// withheld from a request that carries no SigV4 authentication: the rule
// is the global (settings) rule and allows origin only through "*".
//
// Browsers send no credentials of their own to S3, so a cross-origin
// client legitimately using the "*" default always signs its requests
// (an Authorization header, or a presigned URL). An unsigned request is
// served anonymously, and anonymous access may depend on where the
// request comes from (aws:SourceIp allow-lists, LAN-only public buckets):
// any website the user visits could otherwise make the user's browser
// read such objects, or write to such buckets, and see the response.
// Bucket CORS configurations and global rules that list explicit
// origins are what an operator opted into and keep applying to anonymous
// requests.
func corsSignedOnly(cfg *CORSConfiguration, rule *CORSRule, origin string) bool {
	return cfg.global && rule.allowsOriginOnlyByWildcard(origin)
}

// requestCarriesSigV4 reports whether r carries SigV4 authentication: an
// Authorization header or presigned query parameters. Authentication
// either verifies such a request or rejects it, so it is never served
// anonymously; that does not hold for operations the core does not
// authenticate (PostObject, see actualRequestSigned).
func requestCarriesSigV4(r *http.Request, q url.Values) bool {
	return r.Header.Get("Authorization") != "" ||
		q.Has("X-Amz-Credential") || q.Has("X-Amz-Signature") || q.Has("X-Amz-Algorithm")
}

// splitHeaderList splits a comma-separated header list, lowercasing and
// dropping empty entries.
func splitHeaderList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handlePreflight answers a CORS preflight (OPTIONS) request without
// authentication: 400 without Origin or Access-Control-Request-Method,
// 403 AccessForbidden when no rule matches (or only the global "*" rule
// does and the actual request will not be signed), else 200 with the
// Access-Control-* headers.
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request, rc *reqContext, settings config.Settings) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		s.writeError(w, r, rc, s3err.ErrBadRequest.WithMessage("Insufficient information. Origin request header needed."))
		return
	}
	method := strings.TrimSpace(r.Header.Get("Access-Control-Request-Method"))
	if !corsMethods[method] {
		s.writeError(w, r, rc, s3err.ErrBadRequest.WithMessage("Invalid Access-Control-Request-Method: %s", method))
		return
	}
	headerValues := r.Header.Values("Access-Control-Request-Headers")
	if len(headerValues) > maxCORSRequestHeadersCount {
		s.writeError(w, r, rc, s3err.ErrBadRequest.WithMessage("Access-Control-Request-Headers contains too many headers."))
		return
	}
	headerBytes := 0
	for _, value := range headerValues {
		headerBytes += len(value)
		if headerBytes > maxCORSRequestHeadersSize {
			s.writeError(w, r, rc, s3err.ErrBadRequest.WithMessage("Access-Control-Request-Headers is too large."))
			return
		}
	}
	reqHeaders := splitHeaderList(strings.Join(headerValues, ","))
	if len(reqHeaders) > maxCORSRequestHeadersCount {
		s.writeError(w, r, rc, s3err.ErrBadRequest.WithMessage("Access-Control-Request-Headers contains too many headers."))
		return
	}
	cfg := s.corsConfigFor(r.Context(), rc.bucket, settings)
	if cfg == nil {
		s.writeError(w, r, rc, s3err.ErrCORSForbidden.WithMessage("CORSResponse: CORS is not enabled for this bucket."))
		return
	}
	rule := cfg.match(origin, method, reqHeaders)
	h := w.Header()
	h.Add("Vary", "Origin, Access-Control-Request-Headers, Access-Control-Request-Method")
	// Under the global "*" rule only requests that will be signed are
	// approved: the actual request must send an Authorization header
	// (which the browser lists here) or be a presigned URL. A preflight
	// itself never carries credentials.
	if rule != nil && corsSignedOnly(cfg, rule, origin) &&
		!slices.Contains(reqHeaders, "authorization") && !requestCarriesSigV4(r, rc.query) {
		rule = nil
	}
	if rule == nil {
		s.writeError(w, r, rc, s3err.ErrCORSForbidden)
		return
	}
	setCORSOrigin(h, rule, origin)
	h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if len(reqHeaders) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(reqHeaders, ", "))
	}
	if len(rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	if rule.MaxAgeSeconds != nil {
		h.Set("Access-Control-Max-Age", strconv.Itoa(*rule.MaxAgeSeconds))
	}
	writeEmpty(w, http.StatusOK)
}

// applyCORSHeaders adds CORS response headers to an actual (non-preflight)
// request carrying an Origin header, when a rule allows the origin and
// method (under the global "*" rule, only for signed requests; see
// corsSignedOnly). They are set before the handler runs so error
// responses carry them too.
func (s *Server) applyCORSHeaders(w http.ResponseWriter, r *http.Request, rc *reqContext, settings config.Settings) {
	origin := r.Header.Get("Origin")
	cfg := s.corsConfigFor(r.Context(), rc.bucket, settings)
	if cfg == nil {
		return
	}
	h := w.Header()
	h.Add("Vary", "Origin")
	rule := cfg.match(origin, r.Method, nil)
	if rule == nil || (corsSignedOnly(cfg, rule, origin) && !actualRequestSigned(r, rc)) {
		return
	}
	setCORSOrigin(h, rule, origin)
	h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if len(rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	if rule.MaxAgeSeconds != nil {
		h.Set("Access-Control-Max-Age", strconv.Itoa(*rule.MaxAgeSeconds))
	}
}

// actualRequestSigned reports whether an actual (routed) request will be
// served only once its SigV4 authentication has been verified. PostObject
// (NoAuth) never is: it authenticates from its form fields and serves forms
// without a policy signature anonymously, whatever Authorization header or
// X-Amz-* query parameters come with them.
func actualRequestSigned(r *http.Request, rc *reqContext) bool {
	return rc.op != nil && !rc.op.NoAuth && requestCarriesSigV4(r, rc.query)
}

// setCORSOrigin sets Access-Control-Allow-Origin ("*" for wildcard rules,
// else the request origin with Access-Control-Allow-Credentials: true).
func setCORSOrigin(h http.Header, rule *CORSRule, origin string) {
	if rule.wildcardOrigin() {
		h.Set("Access-Control-Allow-Origin", "*")
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
}
