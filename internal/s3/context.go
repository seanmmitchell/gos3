package s3

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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

// countingBody counts the bytes read from the request body.
type countingBody struct {
	rc io.ReadCloser
	n  atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c *countingBody) Close() error { return c.rc.Close() }
