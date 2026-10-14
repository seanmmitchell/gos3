package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/admin"
	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/clientip"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/webui"
)

// SystemPrefix is the reserved path prefix of gos3's own endpoints. It can
// never clash with a bucket ("_" is invalid in bucket names).
const SystemPrefix = "/_gos3/"

// Public endpoint paths.
const (
	InfoPath    = "/_gos3/info"
	HealthPath  = "/_gos3/health"
	ReadyPath   = "/_gos3/ready"
	MetricsPath = "/_gos3/metrics"
)

// adminOp is the metrics operation name of admin API requests.
const adminOp = "Admin"

// ServeHTTP implements the top-level routing (see the package comment).
// Before routing it bounds response writes (see writeGuard), takes the TLS
// state from the connection (see withConnTLS) and checks the Host header
// (see hostPolicy).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w, finish := s.guardWrites(w)
	defer finish()
	r = withConnTLS(r)
	st := s.settings.Get()
	known := s.hosts.known(r.Host, &st)
	if !known {
		s.hosts.warn(s.log, r.Host)
		if s.hosts.strict {
			w, r, done := s.guardBody(w, r)
			defer done()
			s.rejectHost(w, r, true)
			return
		}
	}
	// 1. Virtual-hosted-style requests always go to S3: "/_gos3/..." is then
	// an object key. (Their host is always known.)
	if isVirtualHost(r.Host, st.Domains) {
		s.s3.ServeHTTP(w, r)
		return
	}
	// 2. gos3's own endpoints.
	if p := r.URL.Path; p == "/_gos3" || strings.HasPrefix(p, SystemPrefix) {
		w, r, done := s.guardBody(w, r)
		defer done()
		if !known && !hostIndependentPath(p, &st) {
			s.rejectHost(w, r, false)
			return
		}
		s.serveSystem(w, r, &st)
		return
	}
	// 3. A browser opening the server root lands in the UI.
	if s.ui != nil && st.UIRedirect && isBrowserRootRequest(r) {
		w, r, done := s.guardBody(w, r)
		defer done()
		if !known {
			s.rejectHost(w, r, false)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, webui.Prefix, http.StatusFound)
		return
	}
	// 4. S3. Anonymous requests for an unknown host are refused there
	// (after authentication, which decides whether the request is signed).
	if !known {
		r = r.WithContext(s3.WithUnknownHost(r.Context()))
	}
	s.s3.ServeHTTP(w, r)
}

// serveSystem routes /_gos3/... paths.
func (s *Server) serveSystem(w http.ResponseWriter, r *http.Request, st *config.Settings) {
	p := r.URL.Path
	switch {
	case p == admin.Prefix || strings.HasPrefix(p, admin.Prefix+"/"):
		s.admin.ServeHTTP(w, r)
	case p+"/" == webui.Prefix || strings.HasPrefix(p, webui.Prefix):
		if s.ui == nil {
			s.logSystem(r, http.StatusNotFound)
			writeJSONError(w, http.StatusNotFound, "NotFound", "the web UI is disabled on this server")
			return
		}
		s.ui.ServeHTTP(w, r)
	case p == InfoPath:
		s.serveInfo(w, r, st)
	case p == HealthPath:
		if !allowGet(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case p == ReadyPath:
		s.serveReady(w, r)
	case p == MetricsPath:
		s.serveMetrics(w, r, st)
	default:
		s.logSystem(r, http.StatusNotFound)
		writeJSONError(w, http.StatusNotFound, "NotFound", "unknown gos3 endpoint "+p)
	}
}

// serveInfo answers GET /_gos3/info: public unless publicInfo is off, then
// any authenticated identity. CORS is "*" so any UI origin can read it.
func (s *Server) serveInfo(w http.ResponseWriter, r *http.Request, st *config.Settings) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", req)
		}
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !allowGet(w, r) {
		return
	}
	if !st.PublicInfo {
		if _, ok := s.authenticate(w, r); !ok {
			return
		}
	}
	writeJSON(w, http.StatusOK, s.Info())
}

// serveReady answers GET /_gos3/ready: 200 when the backend answered
// ListBuckets in time (see backendReady), 503 when it did not or the server
// is shutting down. The endpoint is anonymous, so the storage error itself
// is only logged.
func (s *Server) serveReady(w http.ResponseWriter, r *http.Request) {
	if !allowGet(w, r) {
		return
	}
	if s.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": "shutting down"})
		return
	}
	if !s.backendReady() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": "storage unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// backendReady reports whether the backend answered ListBuckets within
// readyTimeout. The result is reused for readyCacheTTL, and concurrent
// callers share one check, so polling /_gos3/ready (anonymous) costs at
// most one backend call per readyCacheTTL. Failures are logged when the
// state changes, not on every request.
func (s *Server) backendReady() bool {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.readyAt.IsZero() && time.Since(s.readyAt) < readyCacheTTL {
		return s.readyOK
	}
	ctx, cancel := context.WithTimeout(s.lifeCtx, readyTimeout)
	_, err := s.backend.ListBuckets(ctx)
	cancel()
	ok := err == nil
	switch {
	case !ok && (s.readyOK || s.readyAt.IsZero()):
		s.log.Warn("readiness check failed: the storage backend did not answer", "err", err)
	case ok && !s.readyOK && !s.readyAt.IsZero():
		s.log.Info("readiness check: the storage backend answers again")
	}
	s.readyOK, s.readyAt = ok, time.Now()
	return ok
}

// serveMetrics answers GET /_gos3/metrics in the Prometheus text format:
// public when metricsPublic is set, else it requires admin:GetStats.
func (s *Server) serveMetrics(w http.ResponseWriter, r *http.Request, st *config.Settings) {
	if !s.cfg.Metrics.Enabled {
		s.logSystem(r, http.StatusNotFound)
		writeJSONError(w, http.StatusNotFound, "NotFound", "metrics are disabled on this server")
		return
	}
	if !allowGet(w, r) {
		return
	}
	if !st.MetricsPublic {
		id, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		d := s.iam.Authorize(r.Context(), iam.AuthzRequest{
			Identity:   id,
			Action:     "admin:GetStats",
			Conditions: requestConditions(r, s.clientIP),
		})
		if !d.Allowed {
			s.logSystem(r, http.StatusForbidden)
			writeJSONError(w, http.StatusForbidden, "AccessDenied", "admin:GetStats is required to read metrics")
			return
		}
	}
	extra := map[string]float64{}
	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()
	if buckets, err := s.backend.ListBuckets(ctx); err == nil {
		extra["buckets"] = float64(len(buckets))
	} else {
		s.log.Warn("metrics: listing buckets failed", "err", err)
	}
	if st.Discovery.Enabled {
		extra["discovery_peers"] = float64(len(s.disc.Peers()))
	}
	var buf bytes.Buffer
	if err := s.metrics.WritePrometheus(&buf, extra); err != nil {
		s.log.Error("metrics: rendering failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "InternalError", "rendering metrics failed")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

// authenticate verifies r's SigV4 signature. It writes a 401 JSON error and
// reports false for anonymous requests and authentication failures.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (auth.Identity, bool) {
	res, err := s.verifier.Authenticate(r)
	if err != nil {
		e := s3err.As(err)
		s.logSystem(r, http.StatusUnauthorized)
		writeJSONError(w, http.StatusUnauthorized, "Unauthenticated", e.Code+": "+e.Message)
		return auth.Identity{}, false
	}
	if res.Body != nil {
		defer res.Body.Close()
	}
	if res.Identity.IsAnonymous {
		s.logSystem(r, http.StatusUnauthorized)
		writeJSONError(w, http.StatusUnauthorized, "Unauthenticated",
			"AccessDenied: anonymous requests are not allowed; sign the request with SigV4")
		return auth.Identity{}, false
	}
	return res.Identity, true
}

// requestConditions is the IAM condition context of a system endpoint
// request (the same client address rules as the S3 and admin APIs).
func requestConditions(r *http.Request, proxy clientip.Resolver) map[string][]string {
	ip, secure := proxy.ClientInfo(r)
	return map[string][]string{"aws:SourceIp": {ip}, "aws:SecureTransport": {strconv.FormatBool(secure)}}
}

// instrumentAdmin records admin API requests in the metrics registry (op
// "Admin") and the access log.
func (s *Server) instrumentAdmin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		done := s.metrics.Begin()
		rec := &statusRecorder{ResponseWriter: w}
		var in *countingReader
		if r.Body != nil && r.Body != http.NoBody {
			// Replace the body on a shallow copy only: net/http must still
			// see its own Request.Body after the handler returns, or it
			// tries to drain an unread "Expect: 100-continue" body before
			// flushing an early error response (see s3.Server.ServeHTTP).
			r = r.WithContext(r.Context())
			in = &countingReader{ReadCloser: r.Body}
			r.Body = in
		}
		defer func() {
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			var n int64
			if in != nil {
				n = in.n.Load()
			}
			done(adminOp, status, n, rec.bytes)
			level := slog.LevelDebug
			if s.settings.Get().Logging.AccessLog {
				level = slog.LevelInfo
			}
			if s.log.Enabled(r.Context(), level) {
				s.log.LogAttrs(r.Context(), level, "admin request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", status),
					slog.Int64("bytes_in", n),
					slog.Int64("bytes_out", rec.bytes),
					slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
					slog.String("remote", s.remoteIP(r)))
			}
		}()
		h.ServeHTTP(rec, r)
	})
}

// logSystem logs a rejected system endpoint request at debug level.
func (s *Server) logSystem(r *http.Request, status int) {
	s.log.Debug("gos3 endpoint request", "method", r.Method, "path", r.URL.Path, "status", status, "remote", s.remoteIP(r))
}

// remoteIP is the client address logged for a system endpoint request
// (X-Forwarded-For from trusted proxies, see package clientip).
func (s *Server) remoteIP(r *http.Request) string {
	ip, _ := s.clientIP.ClientInfo(r)
	return ip
}

// isVirtualHost reports whether host is "<bucket>.<domain>" for one of the
// configured virtual-host domains (the same rule as the S3 handler: port
// and trailing dot ignored, case-insensitive, IPv6 literals never match,
// and a host that is itself a configured domain is never a virtual host,
// even when it is a subdomain of another configured domain).
func isVirtualHost(host string, domains []string) bool {
	if len(domains) == 0 || host == "" {
		return false
	}
	h := normalizeHost(host)
	match := false
	for _, d := range domains {
		d = strings.TrimSpace(d)
		if i := strings.Index(d, "://"); i >= 0 {
			d = d[i+3:]
		}
		d, _, _ = strings.Cut(d, "/")
		d = normalizeHost(strings.TrimPrefix(d, "."))
		if d == "" {
			continue
		}
		if h == d {
			return false
		}
		if len(h) > len(d)+1 && strings.HasSuffix(h, d) && h[len(h)-len(d)-1] == '.' {
			match = true
		}
	}
	return match
}

// normalizeHost lowercases host and strips a port and a trailing dot.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(h, "[") {
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

// isBrowserRootRequest reports whether r is an unauthenticated browser
// navigation to "/" (as opposed to an S3 ListBuckets call).
func isBrowserRootRequest(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/" || r.Header.Get("Authorization") != "" {
		return false
	}
	q := r.URL.Query()
	for _, k := range []string{"X-Amz-Credential", "X-Amz-Signature", "X-Amz-Algorithm", "AWSAccessKeyId", "Signature"} {
		if q.Has(k) {
			return false
		}
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}

// allowGet answers 405 for methods other than GET and HEAD.
func allowGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeJSONError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method+" is not allowed here")
	return false
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":{"code":"InternalError","message":"encoding response failed"}}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// writeJSONError writes the admin API error envelope
// {"error":{"code":…,"message":…}}.
func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	type detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	writeJSON(w, status, map[string]detail{"error": {Code: code, Message: msg}})
}

// statusRecorder captures the status code and body size of a response.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush implements http.Flusher when the underlying writer does.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// countingReader counts the bytes read from a request body.
type countingReader struct {
	io.ReadCloser
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// errServing is returned by Serve when the server is already serving.
var errServing = errors.New("server: already serving")
