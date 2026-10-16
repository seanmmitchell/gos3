package native

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Remote errors
//
// A failure of the remote service maps to the storage error the S3 layer
// and the multi router understand (remoteErr):
//
//	remote answer                                    storage error
//	NoSuchUpload                                     ErrUploadNotFound
//	NoSuchBucket, bucket-level 403, a wrong region   ErrBucketUnavailable
//	  after one retry in the right region, SlowDown
//	  after the client's retries
//	object-level 403, any 403 while attaching        ErrRemoteDenied
//	NoSuchVersion / NoSuchKey / 404                  ErrVersionNotFound / ErrObjectNotFound
//	412, 409 ConditionalRequestConflict              ErrPreconditionFailed
//	501 NotImplemented                               ErrNotSupported
//	any other 4xx                                    ErrRemoteRejected
//	5xx, transport errors                            ErrBackingUnavailable
//
// ErrBucketUnavailable wraps ErrBackingUnavailable (the S3 layer answers
// 503) but tells the multi router that only this bucket is affected, so a
// deleted or forbidden remote bucket never marks the whole backing down.
// While a bucket is being attached a 403 is the request's fault (the
// credentials do not reach the bucket asked for), not an outage.
// Version-addressed reads and deletes map 404 and 400 answers to
// ErrVersionNotFound themselves (head, deleteVersion): services differ in
// how they report an unknown version ID.

var (
	// errClosed is returned by operations after Close.
	errClosed = errors.New("native: store closed")
	// errRemoteUploadGone is wrapped (with storage.ErrUploadNotFound) into
	// errors caused by a remote multipart upload that no longer exists
	// (aborted by someone else, or expired by a lifecycle rule).
	errRemoteUploadGone = errors.New("the remote multipart upload no longer exists")
	// errNoRemoteBucket is wrapped (with storage.ErrBucketUnavailable)
	// into errors caused by the remote bucket having vanished.
	errNoRemoteBucket = errors.New("the remote bucket does not exist")
)

// opKind tells remoteErr whether a request addressed an object or the
// bucket: a 403 on an object is that object's (object lock, a bucket
// policy), a 403 on the bucket makes the whole bucket unusable.
type opKind int

const (
	objectOp opKind = iota
	bucketOp
)

// remoteErr maps a remote failure of op on t's bucket (and key, "" for
// bucket operations) to the storage errors (see the table above).
func (s *Store) remoteErr(op string, t *target, key string, kind opKind, err error) error {
	if err == nil {
		return nil
	}
	where := t.bucket
	if key != "" {
		where += "/" + key
	}
	pre := fmt.Sprintf("native %q: %s %s", s.name, op, where)
	st, code := statusOf(err)
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w", pre, err)
	case code == "NoSuchUpload":
		return fmt.Errorf("%s: %w: %w", pre, errRemoteUploadGone, storage.ErrUploadNotFound)
	case code == "NoSuchBucket":
		return fmt.Errorf("%s: remote bucket %q: %w: %w", pre, t.b.Remote, errNoRemoteBucket, storage.ErrBucketUnavailable)
	}
	if _, ok := s3client.IsWrongRegion(err); ok {
		return fmt.Errorf("%s: remote bucket %q is in another region than %q: %w: %w", pre, t.b.Remote, t.region, storage.ErrBucketUnavailable, err)
	}
	switch {
	case code == "SlowDown":
		return fmt.Errorf("%s: remote bucket %q keeps throttling: %w: %w", pre, t.b.Remote, storage.ErrBucketUnavailable, err)
	case s3client.IsNotImplemented(err):
		return fmt.Errorf("%s: %w: %w", pre, storage.ErrNotSupported, err)
	case s3client.IsAccessDenied(err) && kind == bucketOp && !t.attaching:
		return fmt.Errorf("%s: access to remote bucket %q denied: %w: %w", pre, t.b.Remote, storage.ErrBucketUnavailable, err)
	case s3client.IsAccessDenied(err):
		return fmt.Errorf("%s: %w: %w", pre, storage.ErrRemoteDenied, err)
	case s3client.IsUnavailable(err):
		return fmt.Errorf("%s: %w: %w", pre, storage.ErrBackingUnavailable, err)
	case s3client.IsPreconditionFailed(err):
		return fmt.Errorf("%s: %w: %w", pre, storage.ErrPreconditionFailed, err)
	case code == "NoSuchVersion":
		return fmt.Errorf("%s: %w: %w", pre, storage.ErrVersionNotFound, err)
	case st == http.StatusNotFound:
		return fmt.Errorf("%s: %w: %w", pre, storage.ErrObjectNotFound, err)
	case st >= 400 && st < 500:
		return fmt.Errorf("%s: %w: %w", pre, storage.ErrRemoteRejected, err)
	}
	return fmt.Errorf("%s: %w", pre, err)
}

// statusOf returns the HTTP status and S3 error code of a remote error
// answer (0, "" if none).
func statusOf(err error) (int, string) {
	var e *s3client.Error
	if errors.As(err, &e) {
		return e.StatusCode, e.Code
	}
	return 0, ""
}

// errGone is wrapped into the not-found errors of lookups whose answer
// proves the version absent (a 404, a listing without it), as opposed to
// answers that only fail to find it (a 400): only the former prune the
// version's overlay.
var errGone = errors.New("the remote has no such version")

// goneErr is notFound for a version the remote proved absent (errGone).
func goneErr(bucket, key, versionID string) error {
	return fmt.Errorf("%w (%w)", notFound(bucket, key, versionID), errGone)
}

// notFound returns the error for a missing object addressed by versionID.
func notFound(bucket, key, versionID string) error {
	if versionID == "" {
		return fmt.Errorf("%w: %s/%s", storage.ErrObjectNotFound, bucket, key)
	}
	return fmt.Errorf("%w: %s/%s version %q", storage.ErrVersionNotFound, bucket, key, versionID)
}

// missingObject reports whether err (from HEAD/GET/DELETE of an object)
// says the object (or version) does not exist: a 404 that is not about
// the bucket. HEAD answers have no body, so a vanished remote bucket looks
// like a missing object there.
func missingObject(err error) bool {
	return s3client.IsNotFound(err) && !s3client.IsCode(err, "NoSuchBucket")
}

// missingVersion reports whether err (from a request addressing a version
// ID) says the version does not exist: a 404, or a 400 (some services
// answer InvalidArgument for version IDs they do not know).
func missingVersion(err error) bool {
	if missingObject(err) {
		return true
	}
	if _, wrong := s3client.IsWrongRegion(err); wrong {
		return false
	}
	st, _ := statusOf(err)
	return st == http.StatusBadRequest
}
