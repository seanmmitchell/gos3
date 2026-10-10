package s3

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

// ServeHTTP implements the S3 request pipeline: request ID and common
// headers, panic recovery, metrics and access log around routing, CORS,
// authentication, authorization and dispatch to the operation handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	rc := &reqContext{requestID: newRequestID(), start: start, opName: opUnknown}
	rw := &responseWriter{ResponseWriter: w}
	h := rw.Header()
	h.Set("X-Amz-Request-Id", rc.requestID)
	h.Set("X-Amz-Id-2", s.hostID(rc.requestID))
	h.Set("Server", s.serverHeader)
	h.Set("Date", start.UTC().Format(http.TimeFormat))

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
	rc.query = r.URL.Query()
	rc.sourceIP, rc.secure = s.clientInfo(r)

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
		if err := validateObjectKey(rc.key); err != nil {
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

	if !op.AuthzByHandler {
		if err := s.authorizeOp(r.Context(), r, rc); err != nil {
			s.writeError(w, r, rc, err)
			return
		}
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
// x-amz-checksum-* / x-amz-sdk-checksum-algorithm: presigners compute
// those over an empty body when the URL is created, so honouring them
// would fail every upload through such a URL. Nil for other requests;
// InvalidArgument for a value that is not a valid header value.
func presignedQueryHeaders(r *http.Request, q url.Values) (http.Header, error) {
	if !q.Has("X-Amz-Signature") && !q.Has("X-Amz-Credential") && !q.Has("X-Amz-Algorithm") {
		return nil, nil
	}
	var out http.Header
	for k, vs := range q {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-amz-") || presignedQueryNonHeaders[lk] ||
			strings.HasPrefix(lk, "x-amz-checksum-") || lk == "x-amz-sdk-checksum-algorithm" ||
			len(r.Header.Values(k)) > 0 {
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

// clientInfo returns the client IP and whether the connection is secure,
// honoring X-Forwarded-For / X-Forwarded-Proto when TrustProxyHeaders is set.
func (s *Server) clientInfo(r *http.Request) (ip string, secure bool) {
	ip = r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	secure = r.TLS != nil
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if a := net.ParseIP(strings.TrimSpace(first)); a != nil {
				ip = a.String()
			}
		}
		if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
			secure = strings.EqualFold(strings.TrimSpace(p), "https")
		}
	}
	return ip, secure
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
