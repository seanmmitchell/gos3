package native

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Bucket operations
//
// The caller (the s3 account backing) keeps the bucket records and
// configuration documents; the Store binds remote buckets (AttachBucket),
// reads their state (Refresh) and changes their versioning and object
// lock configuration on request.

// backingMarker is the marker object at the root of a gos3 store
// (internal/storage/s3store): a remote bucket holding one keeps gos3 state
// (journal, wrapped keys, data blobs) that must not become a native
// bucket's objects.
const backingMarker = "gos3.json"

// AttachBucket binds gos3 bucket req.Bucket to the remote bucket
// req.Remote, creating it (Create) or requiring it to exist (Adopt), and
// returns its state. It refuses remote buckets bound to another gos3
// bucket of this store and remote buckets holding a gos3 store
// (gos3.json at the root) with an error wrapping storage.ErrInvalidTarget.
// Creating a remote bucket that already exists fails with
// storage.ErrRemoteBucketExists; a created bucket whose state cannot be
// read is deleted again.
func (s *Store) AttachBucket(ctx context.Context, req AttachRequest) (RemoteState, error) {
	if req.Bucket == "" || req.Remote == "" {
		return RemoteState{}, fmt.Errorf("%w: native %q: a bucket and a remote bucket are required", storage.ErrInvalidTarget, s.name)
	}
	s.mu.Lock()
	other, bound := s.bound[strings.ToLower(req.Remote)]
	s.mu.Unlock()
	if bound && other != req.Bucket {
		return RemoteState{}, fmt.Errorf("%w: native %q: the remote bucket %q is already the bucket %q", storage.ErrInvalidTarget, s.name, req.Remote, other)
	}
	t := &target{bucket: req.Bucket, b: Bucket{Binding: Binding{Remote: req.Remote, Region: req.Region}}, region: req.Region, attaching: true}
	t.c = s.client().ForRegion(t.region)
	switch req.Intent {
	case Create:
		if err := s.createRemote(ctx, t, req.ObjectLock); err != nil {
			return RemoteState{}, err
		}
		t.b.Owned = true
	case Adopt:
		if err := s.adoptRemote(ctx, t); err != nil {
			return RemoteState{}, err
		}
	default:
		return RemoteState{}, fmt.Errorf("native: unknown attach intent %d", req.Intent)
	}
	st, err := s.probe(ctx, t)
	if err != nil {
		if t.b.Owned {
			if derr := t.c.DeleteBucket(context.WithoutCancel(ctx), t.b.Remote); derr != nil {
				s.log.Warn("native: delete a remote bucket created for a failed attach", "backing", s.name, "remote", t.b.Remote, "err", derr)
			}
		}
		return RemoteState{}, err
	}
	s.mu.Lock()
	s.bound[strings.ToLower(req.Remote)] = req.Bucket
	s.mu.Unlock()
	// Overlays of an earlier bucket of the same name never apply.
	if err := s.dropBucketOverlays(ctx, req.Bucket); err != nil {
		s.log.Warn("native: drop stale overlays", "backing", s.name, "bucket", req.Bucket, "err", err)
	}
	return st, nil
}

// createRemote creates t's remote bucket (in t's region; the client's
// when none was asked for). An existing bucket is refused: AWS us-east-1
// answers 200 for an existing bucket the caller owns, so it is looked for
// first.
func (s *Store) createRemote(ctx context.Context, t *target, objectLock bool) error {
	err := t.c.HeadBucket(ctx, t.b.Remote)
	switch {
	case err == nil || s3client.IsAccessDenied(err):
		return fmt.Errorf("native %q: the remote bucket %q exists: %w", s.name, t.b.Remote, storage.ErrRemoteBucketExists)
	case !missingObject(err):
		if _, wrong := s3client.IsWrongRegion(err); wrong {
			return fmt.Errorf("native %q: the remote bucket %q exists in another region: %w", s.name, t.b.Remote, storage.ErrRemoteBucketExists)
		}
		return s.remoteErr("HeadBucket", t, "", bucketOp, err)
	}
	err = t.c.CreateBucketWith(ctx, t.b.Remote, s3client.CreateBucketOptions{Region: t.region, ObjectLock: objectLock})
	switch {
	case s3client.IsCode(err, "BucketAlreadyOwnedByYou"), s3client.IsCode(err, "BucketAlreadyExists"):
		return fmt.Errorf("native %q: the remote bucket %q exists: %w", s.name, t.b.Remote, storage.ErrRemoteBucketExists)
	case s3client.IsCode(err, "InvalidBucketName"):
		return fmt.Errorf("%w: native %q: the remote refuses the bucket name %q: %w", storage.ErrInvalidTarget, s.name, t.b.Remote, err)
	case err != nil:
		return s.remoteErr("CreateBucket", t, "", objectOp, err)
	}
	return nil
}

// adoptRemote checks that t's remote bucket exists, is accessible and
// holds no gos3 store, discovering its region when none was given.
func (s *Store) adoptRemote(ctx context.Context, t *target) error {
	if t.region == "" {
		region, err := s.client().BucketRegion(ctx, t.b.Remote)
		switch {
		case missingObject(err):
			return fmt.Errorf("%w: native %q: the remote bucket %q does not exist", storage.ErrInvalidTarget, s.name, t.b.Remote)
		case s3client.IsAccessDenied(err):
			return fmt.Errorf("native %q: the remote bucket %q is not accessible with the backing's credentials: %w: %w", s.name, t.b.Remote, storage.ErrRemoteDenied, err)
		case err != nil:
			return s.remoteErr("BucketRegion", t, "", objectOp, err)
		}
		t.b.Region, t.region, t.c = region, region, s.client().ForRegion(region)
	}
	err := s.call(ctx, t, func(c *s3client.Client) error { return c.HeadBucket(ctx, t.b.Remote) })
	switch {
	case missingObject(err):
		return fmt.Errorf("%w: native %q: the remote bucket %q does not exist", storage.ErrInvalidTarget, s.name, t.b.Remote)
	case s3client.IsAccessDenied(err):
		return fmt.Errorf("native %q: the remote bucket %q is not accessible with the backing's credentials: %w: %w", s.name, t.b.Remote, storage.ErrRemoteDenied, err)
	case err != nil:
		return s.remoteErr("HeadBucket", t, "", objectOp, err)
	}
	t.b.Region = t.region
	err = s.call(ctx, t, func(c *s3client.Client) error {
		_, err := c.HeadObjectWith(ctx, t.b.Remote, backingMarker, s3client.HeadOptions{})
		return err
	})
	switch {
	case err == nil:
		return fmt.Errorf("%w: native %q: the remote bucket %q holds a gos3 store (%s) and cannot be a native bucket",
			storage.ErrInvalidTarget, s.name, t.b.Remote, backingMarker)
	case missingObject(err):
		return nil
	}
	if _, marker := s3client.IsDeleteMarker(err); marker {
		return nil
	}
	return s.remoteErr("HeadObject", t, backingMarker, objectOp, err)
}

// Refresh re-reads the state of bucket's remote bucket, including its
// region when the remote says it moved.
func (s *Store) Refresh(ctx context.Context, bucket string) (RemoteState, error) {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return RemoteState{}, err
	}
	// Start from the recorded region; a learned one is re-learned.
	s.mu.Lock()
	delete(s.regions, t.b.Remote)
	s.mu.Unlock()
	t.region, t.c = t.b.Region, s.client().ForRegion(t.b.Region)
	return s.probe(ctx, t)
}

// probe reads the state of t's remote bucket: versioning and object lock
// configuration, each support (a 501 answer means none). Credentials that
// may use the bucket's objects but not read its configuration (a
// bucket-scoped Cloudflare R2 token) get 403 answers there: the feature
// is then left to the remote (no support), as long as the bucket itself
// answers HEAD.
func (s *Store) probe(ctx context.Context, t *target) (RemoteState, error) {
	st := RemoteState{Caps: Caps{Versioning: true, ObjectLock: true, Tagging: s.tagging(t)}}
	var status string
	err := s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		status, err = c.GetBucketVersioning(ctx, t.b.Remote)
		return err
	})
	var headed, reachable bool // whether the bucket was asked HEAD, and answered
	denied := func(what string, err error) bool {
		if !s3client.IsAccessDenied(err) {
			return false
		}
		if !headed {
			headed = true
			reachable = s.call(ctx, t, func(c *s3client.Client) error { return c.HeadBucket(ctx, t.b.Remote) }) == nil
		}
		if reachable {
			s.log.Warn("native: the credentials may not read the remote bucket's "+what+" configuration; gos3 leaves it to the remote",
				"backing", s.name, "bucket", t.bucket, "remote", t.b.Remote, "err", err)
		}
		return reachable
	}
	switch {
	case s3client.IsNotImplemented(err), denied("versioning", err):
		st.Caps.Versioning = false
	case err != nil:
		return RemoteState{}, s.remoteErr("GetBucketVersioning", t, "", bucketOp, err)
	}
	switch status {
	case "Enabled":
		st.Versioning = storage.VersioningEnabled
	case "Suspended":
		st.Versioning = storage.VersioningSuspended
	}
	var (
		cfg   s3client.ObjectLockConfig
		found bool
	)
	err = s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		cfg, found, err = c.GetObjectLockConfiguration(ctx, t.b.Remote)
		return err
	})
	switch {
	case s3client.IsNotImplemented(err), denied("object lock", err):
		st.Caps.ObjectLock = false
	case err != nil:
		return RemoteState{}, s.remoteErr("GetObjectLockConfiguration", t, "", bucketOp, err)
	case found && cfg.Enabled:
		st.ObjectLock = true
		st.LockConfig = lockConfigXML(cfg)
	}
	st.Binding = Binding{Remote: t.b.Remote, Region: t.region, Owned: t.b.Owned}
	return st, nil
}

// lockConfigDoc is an S3 ObjectLockConfiguration document.
type lockConfigDoc struct {
	XMLName           xml.Name     `xml:"ObjectLockConfiguration"`
	XMLNS             string       `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string       `xml:"ObjectLockEnabled,omitempty"`
	Rule              *lockRuleDoc `xml:"Rule,omitempty"`
}

type lockRuleDoc struct {
	DefaultRetention *lockRetentionDoc `xml:"DefaultRetention"`
}

type lockRetentionDoc struct {
	Mode  string `xml:"Mode,omitempty"`
	Days  int    `xml:"Days,omitempty"`
	Years int    `xml:"Years,omitempty"`
}

// lockConfigXML renders a remote object lock configuration as an S3
// ObjectLockConfiguration document.
func lockConfigXML(cfg s3client.ObjectLockConfig) []byte {
	doc := lockConfigDoc{XMLNS: "http://s3.amazonaws.com/doc/2006-03-01/"}
	if cfg.Enabled {
		doc.ObjectLockEnabled = "Enabled"
	}
	if cfg.Mode != "" && cfg.Days+cfg.Years > 0 {
		doc.Rule = &lockRuleDoc{DefaultRetention: &lockRetentionDoc{Mode: cfg.Mode, Days: cfg.Days, Years: cfg.Years}}
	}
	data, _ := xml.Marshal(doc) // cannot fail for this type
	return data
}

// SetVersioning changes the versioning status of bucket's remote bucket
// (storage.VersioningEnabled or VersioningSuspended). A remote that does
// not support or refuses it answers storage.ErrNotSupported.
func (s *Store) SetVersioning(ctx context.Context, bucket string, st storage.VersioningStatus) error {
	if st != storage.VersioningEnabled && st != storage.VersioningSuspended {
		return fmt.Errorf("native: cannot set versioning status %q", st)
	}
	t, err := s.target(ctx, bucket)
	if err != nil {
		return err
	}
	err = s.call(ctx, t, func(c *s3client.Client) error { return c.PutBucketVersioning(ctx, t.b.Remote, string(st)) })
	if refused(err) {
		return fmt.Errorf("%w: native %q: the remote bucket %q refuses versioning %s: %w", storage.ErrNotSupported, s.name, t.b.Remote, st, err)
	}
	return s.remoteErr("PutBucketVersioning", t, "", bucketOp, err)
}

// EnableObjectLock enables object lock on bucket's remote bucket. Services
// that only allow it when the bucket is created answer
// storage.ErrNotSupported.
func (s *Store) EnableObjectLock(ctx context.Context, bucket string) error {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return err
	}
	var (
		cfg   s3client.ObjectLockConfig
		found bool
	)
	err = s.call(ctx, t, func(c *s3client.Client) error {
		var err error
		cfg, found, err = c.GetObjectLockConfiguration(ctx, t.b.Remote)
		return err
	})
	switch {
	case s3client.IsNotImplemented(err):
		return fmt.Errorf("%w: native %q: the remote does not support object lock: %w", storage.ErrNotSupported, s.name, err)
	case err != nil:
		return s.remoteErr("GetObjectLockConfiguration", t, "", bucketOp, err)
	case found && cfg.Enabled:
		return nil
	}
	err = s.call(ctx, t, func(c *s3client.Client) error {
		return c.PutObjectLockConfiguration(ctx, t.b.Remote, s3client.ObjectLockConfig{Enabled: true})
	})
	if refused(err) {
		return fmt.Errorf("%w: native %q: the remote bucket %q cannot enable object lock (create a new bucket with object lock): %w",
			storage.ErrNotSupported, s.name, t.b.Remote, err)
	}
	return s.remoteErr("PutObjectLockConfiguration", t, "", bucketOp, err)
}

// PutObjectLockConfig mirrors the default retention of an
// ObjectLockConfiguration document (as the S3 layer stores it) to
// bucket's remote bucket.
func (s *Store) PutObjectLockConfig(ctx context.Context, bucket string, doc []byte) error {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return err
	}
	var d lockConfigDoc
	if err := xml.Unmarshal(doc, &d); err != nil {
		return fmt.Errorf("%w: native %q: object lock configuration: %w", storage.ErrInvalidName, s.name, err)
	}
	cfg := s3client.ObjectLockConfig{Enabled: true}
	if d.Rule != nil && d.Rule.DefaultRetention != nil {
		r := d.Rule.DefaultRetention
		cfg.Mode, cfg.Days, cfg.Years = strings.ToUpper(strings.TrimSpace(r.Mode)), r.Days, r.Years
	}
	if err := s.checkCompliance(t, cfg.Mode); err != nil {
		return err
	}
	err = s.call(ctx, t, func(c *s3client.Client) error { return c.PutObjectLockConfiguration(ctx, t.b.Remote, cfg) })
	if s3client.IsNotImplemented(err) {
		return fmt.Errorf("%w: native %q: the remote does not support object lock: %w", storage.ErrNotSupported, s.name, err)
	}
	return s.remoteErr("PutObjectLockConfiguration", t, "", bucketOp, err)
}

// refused reports a 501 or a 4xx refusal other than 403 (a bucket state
// the service does not allow the change in).
func refused(err error) bool {
	if err == nil {
		return false
	}
	if s3client.IsNotImplemented(err) {
		return true
	}
	if _, wrong := s3client.IsWrongRegion(err); wrong {
		return false
	}
	st, code := statusOf(err)
	return st >= 400 && st < 500 && st != http.StatusForbidden && st != http.StatusNotFound && code != "SlowDown"
}

// CheckEmpty returns storage.ErrBucketNotEmpty while bucket's remote
// bucket holds objects (versioned buckets: versions or delete markers).
// A remote bucket that no longer exists counts as empty, so the gos3
// bucket can still be deleted.
func (s *Store) CheckEmpty(ctx context.Context, bucket string) error {
	t, err := s.target(ctx, bucket)
	if err != nil {
		return err
	}
	var any bool
	if t.versioned() {
		err = s.call(ctx, t, func(c *s3client.Client) error {
			page, err := c.ListObjectVersions(ctx, t.b.Remote, s3client.VersionListInput{MaxKeys: 1})
			any = len(page.Entries) > 0 || len(page.CommonPrefixes) > 0
			return err
		})
	} else {
		err = s.call(ctx, t, func(c *s3client.Client) error {
			page, err := c.ListObjectsV2(ctx, t.b.Remote, s3client.ListInput{MaxKeys: 1})
			any = len(page.Objects) > 0 || len(page.CommonPrefixes) > 0
			return err
		})
	}
	switch {
	case s3client.IsCode(err, "NoSuchBucket"):
		return nil
	case err != nil:
		return s.remoteErr("ListObjects", t, "", bucketOp, err)
	case any:
		return fmt.Errorf("%w: %q", storage.ErrBucketNotEmpty, bucket)
	}
	return nil
}

// DetachBucket forgets bucket: writers created before fail at Commit,
// its in-progress uploads are aborted (remote uploads best effort) and its
// overlays dropped. With removeRemote (a remote bucket gos3 created) the
// remote bucket is deleted too, which fails while it holds objects;
// failures to delete it are logged, not returned (the gos3 bucket is gone
// either way, and the remote bucket stays as an unmanaged one).
func (s *Store) DetachBucket(ctx context.Context, bucket string, removeRemote bool) error {
	s.mu.Lock()
	s.gens[bucket]++
	ups := append([]*upload(nil), s.byBucket[bucket]...)
	s.mu.Unlock()
	t, terr := s.target(ctx, bucket)
	var errs []error
	for _, u := range ups {
		u.op.Lock()
		if !s.isGone(u) {
			if rid := s.remoteUploadID(u); rid != "" {
				if ut, err := s.uploadTarget(ctx, u); err == nil {
					err = s.call(ctx, ut, func(c *s3client.Client) error { return c.AbortMultipartUpload(ctx, ut.b.Remote, u.key, rid) })
					if err != nil && !s3client.IsCode(err, "NoSuchUpload") {
						s.log.Warn("native: abort the remote upload of a detached bucket", "backing", s.name, "bucket", bucket,
							"key", u.key, "upload", u.id, "err", err)
					}
				}
			}
			// The bucket is gone: forget the upload even if its records
			// cannot be deleted (they then bring it back after a restart,
			// where it can be aborted again if the bucket is recreated).
			if err := s.dropUpload(ctx, u, true); err != nil {
				errs = append(errs, err)
			}
		}
		u.op.Unlock()
	}
	if err := s.dropBucketOverlays(ctx, bucket); err != nil {
		errs = append(errs, err)
	}
	// Do not remove the remote bucket after local cleanup failed: the
	// account layer retains the binding for explicit recovery.
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if terr == nil {
		if removeRemote {
			err := s.call(ctx, t, func(c *s3client.Client) error { return c.DeleteBucket(ctx, t.b.Remote) })
			if err != nil && !s3client.IsCode(err, "NoSuchBucket") {
				s.log.Warn("native: delete the remote bucket of a deleted bucket; it stays as an unmanaged bucket", "backing", s.name,
					"bucket", bucket, "remote", t.b.Remote, "err", err)
			}
		}
		s.mu.Lock()
		if s.bound[strings.ToLower(t.b.Remote)] == bucket {
			delete(s.bound, strings.ToLower(t.b.Remote))
		}
		delete(s.regions, t.b.Remote)
		delete(s.noTagging, t.b.Remote)
		s.mu.Unlock()
	} else if !errors.Is(terr, storage.ErrBucketNotFound) {
		s.log.Warn("native: detach a bucket that cannot be resolved; its remote bucket is left alone", "backing", s.name, "bucket", bucket, "err", terr)
	}
	return errors.Join(errs...)
}

// UnbindBucket forgets only an in-memory binding after the account record
// has been released. It never contacts the remote bucket or its objects.
func (s *Store) UnbindBucket(bucket, remote string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bound[strings.ToLower(remote)] != bucket {
		return
	}
	delete(s.bound, strings.ToLower(remote))
	delete(s.regions, remote)
	delete(s.noTagging, remote)
}
