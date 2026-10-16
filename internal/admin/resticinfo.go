package admin

import (
	"context"
	"encoding/json"
	"errors"
	iofs "io/fs"
	"net/http"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Restic recovery information and credentials (docs/admin-api.md "Object
// encryption providers"): where a bucket's restic data lives, so that it
// can be recovered with the restic CLI alone, and, for callers allowed
// admin:GetResticCredentials, the secrets to do so.
//
// Every copy of a bucket (primary, replicas, a move target) keeps its own
// per-version archive repository on its backing; whole-bucket snapshots go
// to the repository their schedule names (SnapshotService).

// resticSnapshotTag tags whole-bucket snapshots (internal/resticsnap).
const resticSnapshotTag = "gos3-snapshot"

// Purposes of a restic repository (POST /buckets/{name}/restic/credentials).
const (
	resticPurposeArchive   = "archive"   // a copy's per-version archive
	resticPurposeSnapshots = "snapshots" // whole-bucket snapshots
)

// resticInfo is the ResticInfo document of GET /buckets/{name}/restic. It
// never carries secrets.
type resticInfo struct {
	// Settings is the bucket's encryption provider document.
	Settings storage.EncryptionSettings `json:"settings"`
	// Archive describes each copy's per-version archive repository.
	Archive []resticArchive `json:"archive"`
	// Snapshots is the bucket's SnapshotConfig (null when none, or when
	// whole-bucket snapshots are not available on this server).
	Snapshots json.RawMessage `json:"snapshots"`
	// SnapshotRepository is where whole-bucket snapshots go (null when
	// none is configured).
	SnapshotRepository *restic.RepositoryInfo `json:"snapshotRepository"`
	// SnapshotsError says why the snapshot schedule or its repository
	// could not be described.
	SnapshotsError string `json:"snapshotsError,omitempty"`
	// Tags are the restic tags gos3 writes (for `restic snapshots --tag`).
	Tags resticTags `json:"tags"`
}

// resticArchive describes the archive repository of one copy of a bucket.
type resticArchive struct {
	Backing string `json:"backing"`
	Role    string `json:"role"`
	// Info is null when the repository cannot be described (Error).
	Info  *restic.RepositoryInfo `json:"info"`
	Error string                 `json:"error"`
}

// resticTags are the restic tags of a bucket's snapshots.
type resticTags struct {
	Object   string `json:"object"`
	Bucket   string `json:"bucket"`
	BucketID string `json:"bucketId,omitempty"` // the primary's bucket ID, once its repository exists
	Snapshot string `json:"snapshot"`
}

// resticCredentials is the ResticCredentials document of POST
// /buckets/{name}/restic/credentials: what the restic CLI needs to open a
// repository (RESTIC_REPOSITORY, RESTIC_PASSWORD, extra environment and
// -o options).
type resticCredentials struct {
	Backing    string            `json:"backing,omitempty"`
	Purpose    string            `json:"purpose"`
	Repository string            `json:"repository"`
	Password   string            `json:"password"`
	Env        map[string]string `json:"env"`
	Options    []string          `json:"options"`
}

// resticMember is one copy of a bucket and the backend holding it.
type resticMember struct {
	backing, role string
	backend       storage.Backend // nil when the backing is no longer configured
}

// resticMembers lists the copies of bucket: the primary first, then the
// replicas, then a move target. Without a router the backend is the only
// (default) backing.
func (a *api) resticMembers(ctx context.Context, bucket string) ([]resticMember, error) {
	r, ok := a.d.Backend.(*multi.Router)
	if !ok {
		if _, err := a.d.Backend.GetBucket(ctx, bucket); err != nil {
			return nil, err
		}
		return []resticMember{{backing: config.DefaultBackingName, role: multi.RolePrimary, backend: a.d.Backend}}, nil
	}
	st, err := r.Placement(bucket)
	if err != nil {
		return nil, err
	}
	out := make([]resticMember, 0, len(st.Members))
	for _, m := range st.Members {
		be, _ := r.BackingBackend(m.Backing)
		out = append(out, resticMember{backing: m.Backing, role: m.Role, backend: be})
	}
	return out, nil
}

// noResticRepository reports whether err (from restic.Host) says that a
// bucket has no repository: none yet (fs.ErrNotExist) or none at all
// (errors.ErrUnsupported, e.g. a native bucket of an s3 backing).
func noResticRepository(err error) bool {
	return errors.Is(err, iofs.ErrNotExist) || errors.Is(err, errors.ErrUnsupported)
}

// describeArchive describes the archive repository of bucket's copy m
// without creating it.
func describeArchive(ctx context.Context, m resticMember, bucket string) (resticArchive, error) {
	out := resticArchive{Backing: m.backing, Role: m.role}
	h, ok := m.backend.(restic.Host)
	if !ok {
		// Also a backing that is no longer configured (nil backend).
		out.Error = "no repository: the backing keeps no restic repositories"
		return out, nil
	}
	info, _, err := h.ResticRepository(ctx, bucket, false)
	switch {
	case err == nil:
		out.Info = &info
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return out, err
	case errors.Is(err, iofs.ErrNotExist):
		out.Error = "not created yet: the repository is created by the bucket's first restic write"
	case errors.Is(err, errors.ErrUnsupported):
		out.Error = "no repository: this copy of the bucket keeps no restic repository"
	default:
		out.Error = strings.TrimPrefix(err.Error(), "storage: ")
	}
	return out, nil
}

// getResticInfo implements GET /buckets/{name}/restic: the bucket's
// encryption settings, each copy's archive repository, the whole-bucket
// snapshot schedule and repository, and the tags gos3 uses. Repositories
// are described as they are, never created; secrets are never included.
// What cannot be described is reported in the document (per copy, or
// snapshotsError) rather than failing it: the document is most useful
// when something is broken.
func (a *api) getResticInfo(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	settings, err := a.bucketEncryptionSettings(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	members, err := a.resticMembers(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	out := resticInfo{
		Settings: settings,
		Archive:  make([]resticArchive, 0, len(members)),
		Tags: resticTags{
			Object:   restic.TagObject,
			Bucket:   restic.BucketTag(bucket),
			Snapshot: resticSnapshotTag,
		},
	}
	for _, m := range members {
		ar, err := describeArchive(rc.ctx, m, bucket)
		if err != nil {
			return 0, nil, err
		}
		if ar.Info != nil && ar.Info.BucketID != "" && out.Tags.BucketID == "" {
			out.Tags.BucketID = restic.BucketIDTag(ar.Info.BucketID)
		}
		out.Archive = append(out.Archive, ar)
	}
	if err := a.describeSnapshotRepository(rc.ctx, bucket, &out); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

// describeSnapshotRepository fills the snapshot fields of out. Only a
// canceled request fails it.
func (a *api) describeSnapshotRepository(ctx context.Context, bucket string, out *resticInfo) error {
	if a.d.Snapshots == nil {
		return nil
	}
	cfg, err := a.d.Snapshots.Config(ctx, bucket)
	switch {
	case errors.Is(err, storage.ErrConfigNotFound):
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case err != nil:
		a.log.Warn("admin: cannot read the snapshot schedule", "bucket", bucket, "err", err)
		out.SnapshotsError = a.toAPIError(err).Message
		return nil
	}
	out.Snapshots = cfg
	repo, err := a.d.Snapshots.Repository(ctx, bucket)
	switch {
	case err == nil:
		out.SnapshotRepository = repo
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, storage.ErrConfigNotFound), noResticRepository(err):
	default:
		a.log.Warn("admin: cannot describe the snapshot repository", "bucket", bucket, "err", err)
		out.SnapshotsError = a.toAPIError(err).Message
	}
	return nil
}

// credentialsRequest is the body of POST /buckets/{name}/restic/credentials.
type credentialsRequest struct {
	// Backing selects the copy whose archive repository is wanted
	// (purpose archive; default: the primary).
	Backing string `json:"backing"`
	// Purpose is "archive" (default) or "snapshots".
	Purpose string `json:"purpose"`
}

// getResticCredentials implements POST /buckets/{name}/restic/credentials:
// the password, environment and options of one of the bucket's restic
// repositories, for recovering data with the restic CLI. Every disclosure
// is logged at Warn with the caller, the bucket, the backing and the
// purpose (never the secrets). A repository that does not exist (not
// created yet, or none on that backing) is 404. Errors never carry the
// secrets: the repository's location is resolved without running restic,
// and unexpected failures are reported without their cause (which is
// logged).
func (a *api) getResticCredentials(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	var req credentialsRequest
	if err := rc.decodeBody(&req, true); err != nil {
		return 0, nil, err
	}
	var (
		loc     restic.Location
		backing string
		err     error
	)
	switch req.Purpose {
	case "", resticPurposeArchive:
		req.Purpose = resticPurposeArchive
		loc, backing, err = a.archiveCredentials(rc.ctx, bucket, req.Backing)
	case resticPurposeSnapshots:
		if req.Backing != "" {
			return 0, nil, badRequest("\"backing\" selects a copy's archive repository; whole-bucket snapshots have one repository (leave it out)")
		}
		loc, backing, err = a.snapshotCredentials(rc.ctx, bucket)
	default:
		return 0, nil, badRequest("purpose must be %q or %q", resticPurposeArchive, resticPurposeSnapshots)
	}
	if err != nil {
		return 0, nil, a.credentialsError(bucket, req.Purpose, err)
	}
	out := resticCredentials{
		Backing:    backing,
		Purpose:    req.Purpose,
		Repository: loc.Repository,
		Password:   loc.Password,
		Env:        map[string]string{},
		Options:    append([]string{}, loc.Options...),
	}
	for _, e := range loc.Env {
		if name, value, ok := strings.Cut(e, "="); ok && name != "" {
			out.Env[name] = value
		}
	}
	ip, _ := ClientInfo(rc.r, a.d.TrustProxyHeaders)
	a.log.Warn("admin: restic repository credentials disclosed",
		"caller", describeIdentity(rc.id), "accessKey", rc.id.AccessKey, "remote", ip,
		"bucket", bucket, "backing", backing, "purpose", req.Purpose, "repository", loc.Repository)
	return http.StatusOK, out, nil
}

// archiveCredentials returns the location of the archive repository of
// bucket's copy on backing (default: the primary's), without creating it.
func (a *api) archiveCredentials(ctx context.Context, bucket, backing string) (restic.Location, string, error) {
	members, err := a.resticMembers(ctx, bucket)
	if err != nil {
		return restic.Location{}, "", err
	}
	var m *resticMember
	for i := range members {
		if backing == "" || members[i].backing == backing {
			m = &members[i]
			break
		}
	}
	if m == nil {
		return restic.Location{}, "", notFound("backing %q holds no copy of bucket %q", backing, bucket)
	}
	h, ok := m.backend.(restic.Host)
	if !ok {
		return restic.Location{}, m.backing, notFound("bucket %q has no restic archive repository on backing %q", bucket, m.backing)
	}
	_, loc, err := h.ResticRepository(ctx, bucket, false)
	if noResticRepository(err) {
		return restic.Location{}, m.backing, notFound("bucket %q has no restic archive repository on backing %q (none is created before the bucket's first restic write)", bucket, m.backing)
	}
	return loc, m.backing, err
}

// snapshotCredentials returns the location of bucket's whole-bucket
// snapshot repository and, when its schedule says, the backing holding it.
func (a *api) snapshotCredentials(ctx context.Context, bucket string) (restic.Location, string, error) {
	if a.d.Snapshots == nil {
		return restic.Location{}, "", errSnapshotsUnavailable()
	}
	loc, _, err := a.d.Snapshots.Credentials(ctx, bucket)
	if errors.Is(err, storage.ErrConfigNotFound) || noResticRepository(err) {
		return restic.Location{}, "", notFound("bucket %q has no whole-bucket snapshot repository", bucket)
	}
	if err != nil {
		return restic.Location{}, "", err
	}
	return loc, a.snapshotBacking(ctx, bucket), nil
}

// snapshotBacking returns the backing holding bucket's whole-bucket
// snapshots as its SnapshotConfig names it (a target of kind "backing"),
// or the primary's for a target of kind "bucket"; "" when unknown.
func (a *api) snapshotBacking(ctx context.Context, bucket string) string {
	raw, err := a.d.Snapshots.Config(ctx, bucket)
	if err != nil {
		return ""
	}
	var cfg struct {
		Target struct {
			Kind    string `json:"kind"`
			Backing string `json:"backing"`
		} `json:"target"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	switch cfg.Target.Kind {
	case "backing":
		return cfg.Target.Backing
	case "", "bucket":
		if members, err := a.resticMembers(ctx, bucket); err == nil && len(members) > 0 {
			return members[0].backing
		}
	}
	return ""
}

// credentialsError maps a failure to obtain a repository's credentials.
// API errors, missing buckets, unavailable encryption and canceled requests
// keep their meaning; other causes are logged and reported generically,
// so that nothing about the repository's secrets can reach the response.
func (a *api) credentialsError(bucket, purpose string, err error) error {
	var ae *apiError
	switch {
	case errors.As(err, &ae), errors.Is(err, storage.ErrBucketNotFound),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, storage.ErrEncryptionUnavailable), errors.Is(err, storage.ErrBackingUnavailable):
		a.log.Warn("admin: restic credentials unavailable", "bucket", bucket, "purpose", purpose, "err", err)
		return errorf(http.StatusServiceUnavailable, codeInternalError,
			"the restic repository of bucket %q cannot be reached now; check the server log", bucket)
	}
	a.log.Error("admin: reading restic credentials failed", "bucket", bucket, "purpose", purpose, "err", err)
	return errorf(http.StatusInternalServerError, codeInternalError,
		"cannot read the restic credentials of bucket %q; check the server log", bucket)
}
