package s3

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// reqContext is the per-request state the core hands to every handler.
// It is only used by the goroutine serving the request.
type reqContext struct {
	// op is the detected operation (nil for preflight / routing failures).
	op *opSpec
	// opName is the operation name for metrics and logs.
	opName string
	// requestID is the x-amz-request-id value (16 uppercase hex digits).
	requestID string
	// start is when the request arrived.
	start time.Time

	// bucket and key are the addressed resource ("" when absent); key is
	// the decoded object key, verbatim.
	bucket string
	key    string
	// virtualHost is true for virtual-hosted-style requests.
	virtualHost bool
	// query is the parsed query string.
	query url.Values

	// identity is the authenticated principal (auth.Anonymous if none).
	identity auth.Identity
	// auth is the authentication result (never nil once the handler runs).
	// For PostObject it is a synthetic anonymous result over the raw body.
	auth *auth.Result
	// body is the request payload (== r.Body when the handler runs): it
	// decodes aws-chunked encoding and verifies payload signatures.
	body io.ReadCloser

	// sourceIP is the client address (aws:SourceIp) and secure whether the
	// request arrived over TLS (aws:SecureTransport).
	sourceIP string
	secure   bool

	// accessLogInfo selects info (true) or debug level for the access log.
	accessLogInfo bool
	// errCode is the S3 error code rendered for this request ("" = none).
	errCode string
	// authzReason is the last authorization decision reason (for logs).
	authzReason string
	// logAttrs are extra attributes handlers add to the access log line.
	logAttrs []slog.Attr

	// in counts request body bytes read from the wire.
	in *countingBody

	// bucketInfo caches the result of Server.bucketInfo.
	bucketInfo   storage.BucketInfo
	bucketErr    error
	bucketLoaded bool
}

// versionID returns the versionId query parameter ("" if absent).
func (rc *reqContext) versionID() string { return rc.query.Get("versionId") }

// contentLength returns the decoded request payload length (the
// x-amz-decoded-content-length for aws-chunked uploads, else
// Content-Length), or -1 if unknown.
func (rc *reqContext) contentLength() int64 {
	if rc.auth == nil {
		return -1
	}
	return rc.auth.DecodedContentLength
}

// trailer returns the request trailers (aws-chunked uploads) once the body
// has been read to EOF; never nil.
func (rc *reqContext) trailer() http.Header {
	if rc.auth == nil || rc.auth.Trailer == nil {
		return http.Header{}
	}
	if h := rc.auth.Trailer(); h != nil {
		return h
	}
	return http.Header{}
}

// userName returns the IAM user name of the caller ("" for anonymous,
// auth.RootUser for root).
func (rc *reqContext) userName() string {
	if rc.identity.IsAnonymous {
		return ""
	}
	if rc.identity.IsRoot {
		return auth.RootUser
	}
	return rc.identity.User
}

// addLog adds attributes to the request's access log line.
func (rc *reqContext) addLog(attrs ...slog.Attr) { rc.logAttrs = append(rc.logAttrs, attrs...) }

// bucketInfo returns the addressed bucket's info, cached for the request.
// A missing bucket yields NoSuchBucket (with BucketName).
func (s *Server) bucketInfo(ctx context.Context, rc *reqContext) (storage.BucketInfo, error) {
	if !rc.bucketLoaded {
		rc.bucketInfo, rc.bucketErr = s.lookupBucket(ctx, rc.bucket)
		rc.bucketLoaded = true
	}
	return rc.bucketInfo, rc.bucketErr
}

// forgetBucket drops the cached bucket info (call after changing the
// bucket's versioning or object lock state within a request).
func (rc *reqContext) forgetBucket() { rc.bucketLoaded = false }

// lookupBucket fetches any bucket's info, mapping storage errors.
func (s *Server) lookupBucket(ctx context.Context, bucket string) (storage.BucketInfo, error) {
	if bucket == "" {
		return storage.BucketInfo{}, s3err.ErrNoSuchBucket
	}
	bi, err := s.backend.GetBucket(ctx, bucket)
	if err != nil {
		return storage.BucketInfo{}, bucketError(err, bucket)
	}
	return bi, nil
}

// bucketError maps a storage error for bucket, adding BucketName to
// NoSuchBucket.
func bucketError(err error, bucket string) error {
	if errors.Is(err, storage.ErrBucketNotFound) {
		return s3err.ErrNoSuchBucket.With("BucketName", bucket)
	}
	return mapStorageError(err)
}

// versioning returns the addressed bucket's versioning status.
func (s *Server) versioning(ctx context.Context, rc *reqContext) (storage.VersioningStatus, error) {
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return "", err
	}
	return bi.Versioning, nil
}

// isVersioned reports whether the addressed bucket has ever had versioning
// enabled (Enabled or Suspended), i.e. whether x-amz-version-id headers are
// sent.
func (s *Server) isVersioned(ctx context.Context, rc *reqContext) (bool, error) {
	st, err := s.versioning(ctx, rc)
	return st != storage.VersioningUnversioned, err
}

// countingBody wraps the request body as read from the wire: it counts the
// bytes read and enforces read deadlines on the connection.
//
// net/http only bounds reading the request headers, so without deadlines a
// client could hold a connection (and its goroutine and file descriptor)
// forever by sending a body very slowly or not at all. While the body is
// read, every Read re-arms a rolling idle deadline (idle); a stall fails the
// read with os.ErrDeadlineExceeded, which handlers report as RequestTimeout
// (as AWS does). After EOF the deadline is cleared. setLimit adds an
// absolute deadline for work done before the client is authenticated.
type countingBody struct {
	rc     io.ReadCloser
	n      atomic.Int64
	eof    atomic.Bool
	failed atomic.Bool // a Read failed (timeout, disconnect, bad framing)
	idle   time.Duration

	mu      sync.Mutex
	ctl     *http.ResponseController // nil: deadlines unsupported or disabled
	armedAt time.Time                // when the idle deadline was last set
	limit   time.Time                // absolute deadline (zero = none)
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.arm()
	n, err := c.rc.Read(p)
	c.n.Add(int64(n))
	if err == io.EOF {
		c.eof.Store(true)
		c.clear()
	} else if err != nil {
		c.failed.Store(true)
	}
	// After any other error the deadline is left as is: an expired one
	// keeps net/http from waiting for the rest of a stalled body.
	return n, err
}

func (c *countingBody) Close() error { return c.rc.Close() }

// setDeadline sets the connection read deadline; it disables deadlines
// when the connection does not support them. c.mu must be held.
func (c *countingBody) setDeadline(t time.Time) {
	if c.ctl == nil {
		return
	}
	if err := c.ctl.SetReadDeadline(t); err != nil {
		c.ctl = nil
	}
}

// arm sets the read deadline before a Read: now+idle, capped by the
// absolute limit. Re-arming is skipped shortly (at most a second) after
// the last one.
func (c *countingBody) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctl == nil || c.idle <= 0 {
		return
	}
	now := time.Now()
	if c.limit.IsZero() && !c.armedAt.IsZero() && now.Sub(c.armedAt) < min(time.Second, c.idle/8) {
		return
	}
	d := now.Add(c.idle)
	if !c.limit.IsZero() && c.limit.Before(d) {
		d = c.limit
	}
	c.setDeadline(d)
	c.armedAt = now
}

// clear removes the read deadline (the body was read to its end).
func (c *countingBody) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armedAt.IsZero() {
		c.setDeadline(time.Time{})
		c.armedAt = time.Time{}
	}
}

// setLimit sets (or, with the zero time, removes) an absolute deadline for
// reading the body. Removing it re-arms the idle deadline at once, since an
// expiring limit could otherwise fail a later read (HTTP/2 closes the
// stream's body when its deadline passes, even without a pending read).
func (c *countingBody) setLimit(t time.Time) {
	c.mu.Lock()
	c.limit = t
	c.armedAt = time.Time{}
	c.mu.Unlock()
	if t.IsZero() && !c.eof.Load() {
		c.arm()
	}
}

// abandon gives up on the rest of the body once the response starts: the
// read deadline is set to now, so neither net/http's discard of the unread
// body nor a later Read can wait for the client.
func (c *countingBody) abandon() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idle = 0 // no more re-arming
	c.limit = time.Time{}
	c.armedAt = time.Time{}
	c.setDeadline(time.Now())
}

// drain reads and discards the rest of the body, at most max bytes and
// within grace, and reports whether it reached EOF. It does what net/http
// would do after the handler to keep the connection reusable, but bounded
// in time (net/http waits without a deadline) and through Read, so the
// deadline is cleared at EOF before net/http's background read starts.
func (c *countingBody) drain(grace time.Duration, max int64) bool {
	if c.failed.Load() {
		return false
	}
	c.mu.Lock()
	if c.ctl == nil {
		c.mu.Unlock()
		return false
	}
	c.idle = 0 // no re-arming: grace is the budget
	c.limit = time.Time{}
	c.armedAt = time.Now()
	c.setDeadline(c.armedAt.Add(grace))
	c.mu.Unlock()
	_, err := io.CopyN(io.Discard, c, max+1)
	return err == io.EOF && c.eof.Load()
}

// consumed reports whether the body of a request with the given
// Content-Length (-1: unknown) has been read completely.
func (c *countingBody) consumed(contentLength int64) bool {
	return c.eof.Load() || (contentLength > 0 && c.n.Load() >= contentLength)
}
