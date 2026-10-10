package s3

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	name, err := detectOp(r.Method, t, rc.query, r.Header)
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
