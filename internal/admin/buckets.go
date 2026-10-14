package admin

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// aclPrivate is the default canned ACL.
const aclPrivate = "private"

// bucketSummary is the BucketSummary document of docs/admin-api.md.
type bucketSummary struct {
	Name       string    `json:"name"`
	CreatedAt  time.Time `json:"createdAt"`
	Region     string    `json:"region"`
	Versioning string    `json:"versioning"`
	ObjectLock bool      `json:"objectLock"`
	Owner      string    `json:"owner"`
	// Backing is the backing holding the bucket's data ("local" = the
	// server's own storage) and BackingMode its mode ("local", "blob",
	// "gateway", or "" when the backing is not configured any more).
	Backing     string           `json:"backing"`
	BackingMode string           `json:"backingMode"`
	Configs     bucketConfigs    `json:"configs"`
	Public      bool             `json:"public"`
	Usage       *bucketUsageJSON `json:"usage"`
}

// bucketConfigs flags which configuration documents a bucket has.
type bucketConfigs struct {
	Policy     bool   `json:"policy"`
	CORS       bool   `json:"cors"`
	Lifecycle  bool   `json:"lifecycle"`
	Tagging    bool   `json:"tagging"`
	Encryption bool   `json:"encryption"`
	Website    bool   `json:"website"`
	ObjectLock bool   `json:"objectLock"`
	ACL        string `json:"acl"`
}

// allConfigKeys are every bucket configuration document key (deleted by a
// forced bucket delete).
var allConfigKeys = []string{
	storage.ConfigPolicy, storage.ConfigCORS, storage.ConfigLifecycle, storage.ConfigTagging,
	storage.ConfigEncryption, storage.ConfigObjectLock, storage.ConfigACL, storage.ConfigWebsite,
	storage.ConfigOwnership, storage.ConfigNotification, storage.ConfigLogging,
	storage.ConfigPublicAccessBlock, storage.ConfigReplication, storage.ConfigAccelerate,
	storage.ConfigRequestPayment,
}

// listUsageConcurrency bounds parallel usage computations for GET /buckets.
const listUsageConcurrency = 4

func (a *api) listBuckets(rc *reqCtx) (int, any, error) {
	withUsage, err := queryBool(rc.r, "usage")
	if err != nil {
		return 0, nil, err
	}
	infos, err := a.d.Backend.ListBuckets(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	out := make([]*bucketSummary, len(infos))
	errs := make([]error, len(infos))
	sem := make(chan struct{}, listUsageConcurrency)
	var wg sync.WaitGroup
	for i, info := range infos {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out[i], errs[i] = a.summarize(rc, info, withUsage)
		}()
	}
	wg.Wait()
	buckets := make([]*bucketSummary, 0, len(infos))
	for i, s := range out {
		switch {
		case errors.Is(errs[i], storage.ErrBucketNotFound):
			// deleted concurrently
		case errs[i] != nil:
			return 0, nil, errs[i]
		default:
			buckets = append(buckets, s)
		}
	}
	return http.StatusOK, map[string]any{"buckets": buckets}, nil
}

func (a *api) getBucket(rc *reqCtx) (int, any, error) {
	info, err := a.d.Backend.GetBucket(rc.ctx, rc.r.PathValue("name"))
	if err != nil {
		return 0, nil, err
	}
	s, err := a.summarize(rc, info, true)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, s, nil
}

// summarize builds the BucketSummary of a bucket. Usage that cannot be
// computed (other than a vanished bucket) is reported as null.
func (a *api) summarize(rc *reqCtx, info storage.BucketInfo, withUsage bool) (*bucketSummary, error) {
	s := &bucketSummary{
		Name:       info.Name,
		CreatedAt:  info.CreatedAt,
		Region:     info.Region,
		Versioning: string(info.Versioning),
		ObjectLock: info.ObjectLockEnabled,
		Owner:      info.Owner,
		Backing:    backingName(info.Backing),
	}
	s.BackingMode = a.backingMode(s.Backing)
	if s.Owner == "" {
		s.Owner = auth.RootUser
	}
	has := func(key string) (bool, error) {
		_, err := a.d.Backend.GetBucketConfig(rc.ctx, info.Name, key)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, storage.ErrConfigNotFound):
			return false, nil
		}
		return false, err
	}
	flags := []struct {
		key string
		dst *bool
	}{
		{storage.ConfigPolicy, &s.Configs.Policy},
		{storage.ConfigCORS, &s.Configs.CORS},
		{storage.ConfigLifecycle, &s.Configs.Lifecycle},
		{storage.ConfigTagging, &s.Configs.Tagging},
		{storage.ConfigEncryption, &s.Configs.Encryption},
		{storage.ConfigWebsite, &s.Configs.Website},
		{storage.ConfigObjectLock, &s.Configs.ObjectLock},
	}
	for _, f := range flags {
		ok, err := has(f.key)
		if err != nil {
			return nil, err
		}
		*f.dst = ok
	}
	acl, err := a.bucketACL(rc, info.Name)
	if err != nil {
		return nil, err
	}
	s.Configs.ACL = acl
	pol, err := a.bucketPolicy(rc, info.Name)
	if err != nil {
		return nil, err
	}
	s.Public = iam.IsPublicPolicy(pol) || iam.IsPublicACL(acl)
	if withUsage {
		u, at, err := a.bucketUsage(rc.ctx, info.Name)
		switch {
		case err == nil:
			s.Usage = &bucketUsageJSON{usageJSON: *toUsageJSON(u), ComputedAt: at.UTC()}
		case errors.Is(err, storage.ErrBucketNotFound):
			return nil, err
		case rc.ctx.Err() != nil:
			return nil, rc.ctx.Err()
		default:
			a.log.Warn("admin: computing bucket usage failed", "bucket", info.Name, "err", err)
		}
	}
	return s, nil
}

// bucketACL returns the bucket's canned ACL name ("private" by default).
// Stored AccessControlPolicy XML documents are reduced to the equivalent
// canned ACL by their group grants.
func (a *api) bucketACL(rc *reqCtx, bucket string) (string, error) {
	data, err := a.d.Backend.GetBucketConfig(rc.ctx, bucket, storage.ConfigACL)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return aclPrivate, nil
	}
	if err != nil {
		return "", err
	}
	return cannedACLOf(data), nil
}

// Group grantee URIs of S3 ACLs.
const (
	allUsersURI  = "http://acs.amazonaws.com/groups/global/AllUsers"
	authUsersURI = "http://acs.amazonaws.com/groups/global/AuthenticatedUsers"
)

// cannedACLOf interprets a stored ACL document: a canned ACL name or an
// AccessControlPolicy XML document.
func cannedACLOf(data []byte) string {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return aclPrivate
	}
	if !strings.HasPrefix(s, "<") {
		if len(s) > 64 {
			return aclPrivate
		}
		return strings.ToLower(s)
	}
	var doc struct {
		Grants []struct {
			Grantee struct {
				URI string `xml:"URI"`
			} `xml:"Grantee"`
			Permission string `xml:"Permission"`
		} `xml:"AccessControlList>Grant"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return aclPrivate
	}
	var allRead, allWrite, authRead bool
	for _, g := range doc.Grants {
		uri := strings.TrimSpace(g.Grantee.URI)
		perm := strings.ToUpper(strings.TrimSpace(g.Permission))
		read := perm == "READ" || perm == "FULL_CONTROL"
		write := perm == "WRITE" || perm == "FULL_CONTROL"
		switch uri {
		case allUsersURI:
			allRead = allRead || read
			allWrite = allWrite || write
		case authUsersURI:
			authRead = authRead || read
		}
	}
	switch {
	case allRead && allWrite:
		return iam.ACLPublicReadWrite
	case allRead:
		return iam.ACLPublicRead
	case authRead:
		return iam.ACLAuthenticatedRead
	}
	return aclPrivate
}

// forceDeleteAttempts bounds retries when concurrent writes refill a bucket
// being force-deleted.
const forceDeleteAttempts = 5

// purgeBatch is the number of versions collected per walk before deleting
// them (deleting inside a walk callback could deadlock a backend).
const purgeBatch = 1000

func (a *api) deleteBucket(rc *reqCtx) (int, any, error) {
	name := rc.r.PathValue("name")
	force, err := queryBool(rc.r, "force")
	if err != nil {
		return 0, nil, err
	}
	if !force {
		if err := a.d.Backend.DeleteBucket(rc.ctx, name); err != nil {
			if errors.Is(err, storage.ErrBucketNotEmpty) {
				return 0, nil, errorf(http.StatusConflict, codeConflict,
					"bucket %q is not empty (use ?force=true to delete all of its contents)", name)
			}
			return 0, nil, err
		}
		a.invalidateUsage(name)
		a.log.Info("admin: bucket deleted", "bucket", name, "by", rc.id.User)
		return http.StatusNoContent, nil, nil
	}
	if err := a.forceDeleteBucket(rc.ctx, name); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: bucket force-deleted", "bucket", name, "by", rc.id.User)
	return http.StatusNoContent, nil, nil
}

// forceDeleteBucket removes every version, delete marker, multipart upload
// and configuration document of a bucket, then the bucket itself. Writes
// racing with it are retried a few times. A bucket that disappears midway
// (deleted concurrently) counts as success.
func (a *api) forceDeleteBucket(ctx context.Context, name string) error {
	if _, err := a.d.Backend.GetBucket(ctx, name); err != nil {
		return err
	}
	defer a.invalidateUsage(name)
	var lastErr error
	for range forceDeleteAttempts {
		err := a.emptyBucket(ctx, name)
		if err == nil {
			err = a.d.Backend.DeleteBucket(ctx, name)
		}
		switch {
		case err == nil, errors.Is(err, storage.ErrBucketNotFound):
			return nil
		case errors.Is(err, storage.ErrBucketNotEmpty):
			lastErr = err
			continue
		}
		return err
	}
	return errorf(http.StatusConflict, codeConflict,
		"bucket %q kept receiving new objects while being deleted (%d attempts): %v", name, forceDeleteAttempts, lastErr)
}

// emptyBucket deletes all versions, uploads and configuration documents.
func (a *api) emptyBucket(ctx context.Context, name string) error {
	b := a.d.Backend
	stalls := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		type ver struct{ key, id string }
		batch := make([]ver, 0, 64)
		err := b.WalkVersions(ctx, name, "", "", "", func(o storage.ObjectInfo) error {
			batch = append(batch, ver{o.Key, o.VersionID})
			if len(batch) >= purgeBatch {
				return storage.ErrStopWalk
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		deleted := 0
		for _, v := range batch {
			_, err := b.DeleteObject(ctx, name, v.key, v.id)
			switch {
			case err == nil:
				deleted++
			case errors.Is(err, storage.ErrVersionNotFound), errors.Is(err, storage.ErrObjectNotFound):
				// removed concurrently
			default:
				return fmt.Errorf("deleting %q version %q: %w", v.key, v.id, err)
			}
		}
		// Guard against a backend that keeps listing versions it reports as
		// missing: give up after a few rounds without progress.
		if deleted > 0 {
			stalls = 0
		} else if stalls++; stalls >= 3 {
			return fmt.Errorf("deleting versions of bucket %q made no progress", name)
		}
	}
	uploads, err := b.ListMultipartUploads(ctx, name)
	if err != nil {
		return err
	}
	for _, u := range uploads {
		if err := b.AbortMultipartUpload(ctx, name, u.Key, u.UploadID); err != nil && !errors.Is(err, storage.ErrUploadNotFound) {
			return fmt.Errorf("aborting upload %q of %q: %w", u.UploadID, u.Key, err)
		}
	}
	for _, key := range allConfigKeys {
		if err := b.DeleteBucketConfig(ctx, name, key); err != nil && !errors.Is(err, storage.ErrConfigNotFound) {
			return fmt.Errorf("deleting %s configuration: %w", key, err)
		}
	}
	return nil
}
