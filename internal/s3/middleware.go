package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

// ServeHTTP implements the S3 request pipeline: request ID and common
// headers, panic recovery, metrics and access log around routing, CORS,
// authentication, authorization and dispatch to the operation handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(iam.WithAuthorizationBudget(r.Context()))
	start := s.now()
	rc := &reqContext{requestID: newRequestID(), start: start, opName: opUnknown}
	rw := &responseWriter{ResponseWriter: w}
	h := rw.Header()
	h.Set("X-Amz-Request-Id", rc.requestID)
	h.Set("X-Amz-Id-2", s.hostID(rc.requestID))
	h.Set("Server", s.serverHeader)
	h.Set("Date", start.UTC().Format(http.TimeFormat))
	// S3 responses (XML documents, errors, stored bucket policies, objects)
	// are served from the embedded web UI's origin: browsers must never
	// sniff one into HTML or script.
	h.Set("X-Content-Type-Options", "nosniff")

	var done func(op string, status int, bytesIn, bytesOut int64)
	if s.metrics != nil {
		done = s.metrics.Begin()
	}
	// Work on a shallow copy of the request: net/http inspects the Body of
	// its own *Request after the handler returns. When that is still its
	// expectContinueReader and no "100 Continue" was sent, it simply closes
	// the connection; any other Body type makes it try to discard up to
	// 256 KiB of the unread body first, which blocks until a client that
	// sent "Expect: 100-continue" gives up waiting (1 s for botocore and
	// curl) before the already written error response is flushed.
	r = r.WithContext(r.Context())
	if r.Body == nil {
		r.Body = http.NoBody
	}
	body := &countingBody{rc: r.Body}
	if r.ContentLength != 0 {
		body.ctl, body.idle = http.NewResponseController(w), s.bodyIdleTimeout
		if r.ProtoMajor == 1 {
			// When the handler did not read the whole body (typically an
			// early error such as a failed authentication), net/http reads
			// and discards up to 256 KiB of it before sending the response
			// so that the connection can be reused, without any deadline: a
			// client trickling its body would hold the response, the
			// goroutine and the connection forever. Drain it here instead,
			// within unreadBodyGrace; when that fails (or more than 256 KiB
			// remain, or a "100 Continue" was never sent) the response says
			// "Connection: close" and the body is abandoned. HTTP/2 does
			// not drain unread bodies.
			cl := r.ContentLength
			expect := strings.EqualFold(r.Header.Get("Expect"), "100-continue")
			rw.beforeHeader = func() {
				if body.consumed(cl) {
					return
				}
				if (expect && body.n.Load() == 0) || !body.drain(s.unreadBodyGrace, maxUnreadBodyDrain) {
					rw.Header().Set("Connection", "close")
					body.abandon()
				}
			}
		}
	}
	r.Body = body
	rc.in = body

	defer func() {
		abort := false
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				abort = true
			} else {
				s.log.LogAttrs(r.Context(), slog.LevelError, "s3: panic serving request",
					slog.String("request_id", rc.requestID), slog.String("op", rc.opName),
					slog.String("panic", fmt.Sprint(p)), slog.String("stack", string(debug.Stack())))
				if rw.wroteHeader {
					abort = true // cannot report an error mid-response; drop the connection
				} else {
					s.writeError(rw, r, rc, s3err.ErrInternalError)
				}
			}
		}
		status := rw.status
		if status == 0 {
			status = http.StatusOK
		}
		if clientAborted(r, rc) {
			// The client went away mid-request: nobody receives the 400
			// IncompleteBody. Record it as nginx's 499 "client closed
			// request" so it can be told apart from client errors.
			status = statusClientClosedRequest
			rc.logAttrs = append(rc.logAttrs, slog.Bool("client_aborted", true))
		}
		if done != nil {
			done(rc.opName, status, body.n.Load(), rw.bytes)
		}
		s.accessLog(r, rc, status, body.n.Load(), rw.bytes)
		if abort {
			panic(http.ErrAbortHandler)
		}
	}()
	s.serve(rw, r, rc)
}

// maxUnreadBodyDrain is the most of an unread request body drained to keep
// the connection (net/http's own limit).
const maxUnreadBodyDrain = 256 << 10

// statusClientClosedRequest is the status recorded (metrics, access log)
// for requests whose client disconnected before the request was complete.
const statusClientClosedRequest = 499

// clientAborted reports whether the request failed because the client
// disconnected while sending its body: the body ended early
// (IncompleteBody) and net/http has cancelled the request context, which it
// does when the connection or stream is gone.
func clientAborted(r *http.Request, rc *reqContext) bool {
	return rc.errCode == s3err.ErrIncompleteBody.Code && r.Context().Err() != nil
}

// serve routes, authenticates, authorizes and dispatches one request.
func (s *Server) serve(w *responseWriter, r *http.Request, rc *reqContext) {
	settings := s.settings.Get()
	rc.accessLogInfo = settings.Logging.AccessLog
	rc.sourceIP, rc.secure = s.clientInfo(r)
	// The query as the signature verifier decodes it (not r.URL.Query(),
	// which drops pairs containing ';' or an invalid escape that a
	// signature still covers).
	q, err := auth.QueryValues(r)
	if err != nil {
		s.writeError(w, r, rc, err)
		return
	}
	rc.query = q

	t, err := parseTarget(r, settings.Domains)
	if err != nil {
		s.writeError(w, r, rc, err)
		return
	}
	rc.bucket, rc.key, rc.virtualHost = t.Bucket, t.Key, t.VirtualHost

	if r.Method == http.MethodOptions {
		rc.opName = opPreflight
		s.handlePreflight(w, r, rc, settings)
		return
	}
	// x-amz-* request headers that a presigner moved into the (signed)
	// query string count as headers (AWS semantics). They are applied to
	// the request only once the presigned signature has been verified;
	// routing already needs them (x-amz-copy-source).
	hoisted, err := presignedQueryHeaders(r, rc.query)
	if err != nil {
		s.writeError(w, r, rc, err)
		return
	}
	routeHdr := r.Header
	if len(hoisted) > 0 {
		routeHdr = r.Header.Clone()
		mergeHeaders(routeHdr, hoisted)
	}
	name, err := detectOp(r.Method, t, rc.query, routeHdr)
	if err != nil {
		s.writeError(w, r, rc, err)
		return
	}
	op := lookupOp(name)
	if op == nil { // detectOp and opTable disagree: a programming error
		s.writeError(w, r, rc, fmt.Errorf("s3: no op spec for %q", name))
		return
	}
	rc.op, rc.opName = op, op.Name

	if r.Header.Get("Origin") != "" {
		s.applyCORSHeaders(w, r, rc, settings)
	}
	if rc.key != "" {
		validate := validateObjectKey
		if op.NewKey {
			validate = validateNewObjectKey
		}
		if err := validate(rc.key); err != nil {
			s.writeError(w, r, rc, err)
			return
		}
	}

	if op.NoAuth {
		rc.identity = auth.Anonymous
		rc.auth = &auth.Result{
			Identity:             auth.Anonymous,
			Method:               auth.MethodAnonymous,
			Body:                 r.Body,
			DecodedContentLength: r.ContentLength,
			Trailer:              func() http.Header { return http.Header{} },
		}
	} else {
		res, err := s.verifier.Authenticate(r)
		if err != nil {
			s.writeError(w, r, rc, err)
			return
		}
		if res.Body == nil {
			res.Body = r.Body
		}
		if res.Trailer == nil {
			res.Trailer = func() http.Header { return http.Header{} }
		}
		rc.auth, rc.identity = res, res.Identity
		r.Body = res.Body
		if res.Method == auth.MethodPresignedV4 {
			mergeHeaders(r.Header, hoisted)
		}
		stripAWSChunked(r.Header)
	}
	rc.body = r.Body
	if rc.identity.IsAnonymous && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.setAnonymousResourcePolicy(w, r, rc, settings)
	}

	switch op.Name {
	case "ListObjects", "ListObjectsV2", "ListObjectVersions", "ListMultipartUploads":
		// Before authorization: the policy must see the values the
		// listing uses (N-02).
		if err := validateListQuery(rc.query); err != nil {
			s.writeError(w, r, rc, err)
			return
		}
	}
	if !op.AuthzByHandler {
		if err := s.authorizeOp(r.Context(), r, rc); err != nil {
			s.writeError(w, r, rc, err)
			return
		}
	} else if !op.NoAuth {
		// The handler reads the body before it authorizes the caller.
		s.limitPreAuthBody(rc)
	}

	handler := op.handler
	if s.handlerOverride != nil {
		if o := s.handlerOverride(op.Name); o != nil {
			handler = o
		}
	}
	if err := handler(s, w, r, rc); err != nil {
		s.writeError(w, r, rc, s.hideMissingObject(w, r, rc, err))
	}
}

// presignedQueryNonHeaders are x-amz-* query parameters (lower-cased)
// that never stand for request headers: the SigV4 query authentication
// parameters and body framing headers, which must come with the request.
var presignedQueryNonHeaders = map[string]bool{
	"x-amz-algorithm": true, "x-amz-credential": true, "x-amz-date": true,
	"x-amz-expires": true, "x-amz-signedheaders": true, "x-amz-signature": true,
	"x-amz-security-token": true, "x-amz-content-sha256": true,
	"x-amz-trailer": true, "x-amz-decoded-content-length": true,
}

// presignedQueryHeaders returns the x-amz-* query parameters of a request
// that uses SigV4 query authentication, as request headers. Presigners
// such as the AWS SDK for JavaScript v3 move x-amz-* headers (copy source,
// metadata, ACL, tagging, storage class, object lock, ...) into the query
// string, where the signature covers them; S3 treats them as the
// corresponding headers. Parameters whose header is also sent are
// skipped (the header wins), as are the authentication parameters and
// the checksum values (x-amz-checksum-crc32, -sha256, ...) with
// x-amz-sdk-checksum-algorithm: presigners compute those over an empty
// body when the URL is created, so honouring them would fail every upload
// through such a URL. The checksum settings x-amz-checksum-algorithm,
// -type and -mode are hoisted. Nil for other requests; InvalidArgument for
// a value that is not a valid header value.
func presignedQueryHeaders(r *http.Request, q url.Values) (http.Header, error) {
	if !q.Has("X-Amz-Signature") && !q.Has("X-Amz-Credential") && !q.Has("X-Amz-Algorithm") {
		return nil, nil
	}
	var out http.Header
	for k, vs := range q {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-amz-") || presignedQueryNonHeaders[lk] ||
			presignedChecksumValue(lk) || len(r.Header.Values(k)) > 0 {
			continue
		}
		if out == nil {
			out = http.Header{}
		}
		for _, v := range vs {
			if strings.ContainsFunc(v, func(c rune) bool { return (c < 0x20 && c != '\t') || c == 0x7f }) {
				return nil, s3err.ErrInvalidArgument.WithMessage("Invalid value for query parameter %s.", k).With("ArgumentName", k)
			}
			out.Add(k, v)
		}
	}
	return out, nil
}

// presignedChecksumValue reports whether the lower-cased query parameter
// lk is a checksum value a presigner computed over an empty body
// (x-amz-checksum-<algorithm>) or the SDK's announcement of one
// (x-amz-sdk-checksum-algorithm).
func presignedChecksumValue(lk string) bool {
	if lk == "x-amz-sdk-checksum-algorithm" {
		return true
	}
	alg, ok := strings.CutPrefix(lk, "x-amz-checksum-")
	return ok && slices.ContainsFunc(checksumAlgorithms, func(a string) bool { return strings.EqualFold(a, alg) })
}

// mergeHeaders adds the values of src to dst.
func mergeHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// hideMissingObject applies AWS's rule for reads of objects that do not
// exist: a caller without s3:ListBucket on the bucket gets AccessDenied
// (403) instead of NoSuchKey (404), so key existence cannot be probed by
// someone who may only read objects (e.g. anonymous users of a public-read
// bucket policy). Root, and callers allowed to list, get the 404.
func (s *Server) hideMissingObject(w http.ResponseWriter, r *http.Request, rc *reqContext, err error) error {
	switch rc.opName {
	case "GetObject", "HeadObject", "GetObjectAttributes":
	default:
		return err
	}
	if rc.identity.IsRoot && !rc.identity.IsAnonymous {
		return err
	}
	if !errors.Is(mapStorageError(err), s3err.ErrNoSuchKey) {
		return err
	}
	reason := rc.authzReason
	listErr := s.authorizeTarget(r.Context(), r, rc, authzTarget{Action: "s3:ListBucket", Bucket: rc.bucket})
	rc.authzReason = reason
	if listErr == nil {
		return err
	}
	if !errors.Is(listErr, s3err.ErrAccessDenied) {
		return listErr
	}
	// Do not reveal a delete marker either.
	h := w.Header()
	h.Del("X-Amz-Delete-Marker")
	h.Del("X-Amz-Version-Id")
	return s3err.ErrAccessDenied
}

// stripAWSChunked removes the "aws-chunked" token from Content-Encoding (it
// describes the transfer framing, not the object), deleting the header when
// nothing remains.
func stripAWSChunked(h http.Header) {
	vals := h.Values("Content-Encoding")
	if len(vals) == 0 {
		return
	}
	var keep []string
	changed := false
	for _, v := range vals {
		for _, tok := range strings.Split(v, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if strings.EqualFold(tok, "aws-chunked") {
				changed = true
				continue
			}
			keep = append(keep, tok)
		}
	}
	if !changed {
		return
	}
	if len(keep) == 0 {
		h.Del("Content-Encoding")
		return
	}
	h.Set("Content-Encoding", strings.Join(keep, ","))
}

// checkAnonymousRequest refuses anonymous requests that a website could
// make a visitor's browser send, for data that is anonymously accessible
// only because of where the browser is (aws:SourceIp conditions, buckets
// reachable only on a LAN):
//
//   - requests whose Host is not one of the server's names (marked by
//     WithUnknownHost: a DNS-rebinding page is same-origin with the server
//     under its own host name, so CORS never applies);
//   - cross-origin requests that change data (any method but GET, HEAD
//     and OPTIONS) unless the bucket's CORS configuration, or a global CORS
//     origin listed explicitly, allows the origin and method. Browsers send
//     such "simple" requests (HTML form posts, including PostObject
//     uploads, and no-cors fetches with a text/plain body) without a
//     preflight, and CORS only hides the response, so without this check
//     any website could blindly write or delete such data.
//
// Requests that are not from a browser carry neither Origin nor
// Sec-Fetch-Site and are unaffected. The result is cached in rc (handlers
// authorize several targets per request).
func (s *Server) checkAnonymousRequest(ctx context.Context, r *http.Request, rc *reqContext) error {
	if rc.anonChecked {
		return rc.anonErr
	}
	rc.anonChecked = true
	switch {
	case unknownHost(r.Context()):
		rc.authzReason = "anonymous request for a host that is not one of the server's names"
		rc.anonErr = s3err.ErrAccessDenied.WithMessage(
			"Anonymous requests are not accepted for host %q: it is not one of this server's names (see server.allowed_hosts).", r.Host)
	case !safeMethod(r.Method) && crossOriginBrowserRequest(r) && !s.anonymousOriginAllowed(ctx, r, rc):
		rc.authzReason = "anonymous cross-origin write from a browser"
		rc.anonErr = s3err.ErrAccessDenied.WithMessage(
			"Anonymous cross-origin requests that change data are allowed only for origins that the bucket's CORS configuration allows.")
	}
	return rc.anonErr
}

// safeMethod reports whether method only reads.
func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// crossOriginBrowserRequest reports whether a browser sent r on behalf of
// a page of another origin: Sec-Fetch-Site (sent by current browsers) says
// same-site or cross-site, or, without it, the Origin header does not name
// the host the request was sent to ("null" for sandboxed and opaque
// origins). Requests without either header are not from a browser page.
func crossOriginBrowserRequest(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "same-origin", "none":
		return false
	case "same-site", "cross-site":
		return true
	}
	origin := r.Header.Get("Origin")
	return origin != "" && !sameOriginHost(origin, r.Host)
}

// sameOriginHost reports whether the host (and port) of origin is host,
// ignoring case and the scheme's default port. The scheme is not compared:
// behind a TLS-terminating proxy gos3 cannot tell which one the page used.
func sameOriginHost(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	strip := func(h string) string {
		h = strings.ToLower(h)
		switch {
		case u.Scheme == "http" && strings.HasSuffix(h, ":80"):
			return strings.TrimSuffix(h, ":80")
		case u.Scheme == "https" && strings.HasSuffix(h, ":443"):
			return strings.TrimSuffix(h, ":443")
		}
		return h
	}
	return strip(u.Host) == strip(host)
}

// anonymousOriginAllowed reports whether the CORS configuration that
// applies to r (the bucket's, else the global one) allows its Origin and
// method for an anonymous request: the global "*" rule does not (see
// corsSignedOnly).
func (s *Server) anonymousOriginAllowed(ctx context.Context, r *http.Request, rc *reqContext) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	cfg := s.corsConfigFor(ctx, rc.bucket, s.settings.Get())
	if cfg == nil {
		return false
	}
	rule := cfg.match(origin, r.Method, nil)
	return rule != nil && !corsSignedOnly(cfg, rule, origin)
}

// setAnonymousResourcePolicy adds "Cross-Origin-Resource-Policy:
// same-site" to the response of an anonymous read, unless the CORS
// configuration that applies opts in to cross-origin reads: a bucket CORS
// rule allowing GET, or a global rule listing explicit origins. CORS does
// not govern no-cors loads (<script>, <link rel=stylesheet>, <img>,
// <video>): without the header any website could run an anonymously
// readable script object in its own page and read what it defines,
// measure images and media, and tell existing keys from missing ones by
// load and error events, for data that is accessible only from the
// visitor's network. Navigations, signed requests (presigned URLs
// included) and CORS-mode fetches are not affected.
func (s *Server) setAnonymousResourcePolicy(w http.ResponseWriter, r *http.Request, rc *reqContext, settings config.Settings) {
	if cfg := s.corsConfigFor(r.Context(), rc.bucket, settings); cfg != nil && corsAllowsAnonymousReads(cfg) {
		return
	}
	w.Header().Set("Cross-Origin-Resource-Policy", "same-site")
}

// corsAllowsAnonymousReads reports whether a CORS configuration allows GET
// from some origin for anonymous requests (the global "*" origin does not;
// see corsSignedOnly).
func corsAllowsAnonymousReads(cfg *CORSConfiguration) bool {
	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		if !rule.allowsMethod(http.MethodGet) {
			continue
		}
		if !cfg.global {
			return true
		}
		for _, o := range rule.AllowedOrigins {
			if o != "*" {
				return true
			}
		}
	}
	return false
}

// clientInfo returns the client IP and whether the connection is secure,
// honoring X-Forwarded-For / X-Forwarded-Proto from trusted proxies (see
// package clientip).
func (s *Server) clientInfo(r *http.Request) (ip string, secure bool) {
	return s.clientIP.ClientInfo(r)
}

// accessLog writes the structured access log line for a finished request.
func (s *Server) accessLog(r *http.Request, rc *reqContext, status int, in, out int64) {
	level := slog.LevelDebug
	if rc.accessLogInfo {
		level = slog.LevelInfo
	}
	ctx := r.Context()
	if !s.log.Enabled(ctx, level) {
		return
	}
	attrs := make([]slog.Attr, 0, 16+len(rc.logAttrs))
	attrs = append(attrs,
		slog.String("request_id", rc.requestID),
		slog.String("op", rc.opName),
		slog.String("method", r.Method),
		slog.String("path", rawRequestPath(r)),
		slog.Int("status", status),
		slog.Int64("bytes_in", in),
		slog.Int64("bytes_out", out),
		slog.Float64("duration_ms", float64(s.now().Sub(rc.start).Microseconds())/1000),
		slog.String("remote", rc.sourceIP),
	)
	if rc.bucket != "" {
		attrs = append(attrs, slog.String("bucket", rc.bucket))
	}
	if rc.key != "" {
		attrs = append(attrs, slog.String("key", rc.key))
	}
	if rc.auth != nil {
		attrs = append(attrs, slog.String("auth", string(rc.auth.Method)))
		if !rc.identity.IsAnonymous {
			attrs = append(attrs, slog.String("user", rc.userName()), slog.String("access_key", rc.identity.AccessKey))
		}
	}
	if rc.errCode != "" {
		attrs = append(attrs, slog.String("error", rc.errCode))
	}
	if rc.authzReason != "" {
		attrs = append(attrs, slog.String("authz", rc.authzReason))
	}
	attrs = append(attrs, rc.logAttrs...)
	s.log.LogAttrs(ctx, level, "s3 request", attrs...)
}

// newRequestID returns 16 uppercase hex digits.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand panics on failure)
	return strings.ToUpper(hex.EncodeToString(b[:]))
}

// responseWriter records the status and body size of a response.
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
	// beforeHeader, if set, runs once before the final status is written.
	beforeHeader func()
}

// WriteHeader records the status; 1xx informational headers pass through
// without completing the response.
func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	if code >= 100 && code < 200 {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.status = code
	w.wroteHeader = true
	if w.beforeHeader != nil {
		w.beforeHeader()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher.
func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.ResponseController.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
