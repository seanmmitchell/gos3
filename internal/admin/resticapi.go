package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Whole-bucket restic snapshots (docs/admin-api.md "Whole-bucket restic
// snapshots"). The endpoints are thin: the SnapshotService (Deps.Snapshots)
// validates schedules, keeps the snapshot list cached and submits the
// snapshot, restore and prune jobs, which the jobs endpoints then show.

// actionCreateCopyJob is also required on the target bucket of a restore.
const actionCreateCopyJob = "admin:CreateCopyJob"

// errSnapshotsUnavailable is the error of the snapshot endpoints on a
// server without whole-bucket snapshots.
func errSnapshotsUnavailable() error {
	return notFound("whole-bucket restic snapshots are not available on this server")
}

// snapshots returns the snapshot service, after checking that bucket
// exists (so that every endpoint answers 404 for unknown buckets alike).
func (a *api) snapshots(ctx context.Context, bucket string) (SnapshotService, error) {
	if a.d.Snapshots == nil {
		return nil, errSnapshotsUnavailable()
	}
	if _, err := a.d.Backend.GetBucket(ctx, bucket); err != nil {
		return nil, err
	}
	return a.d.Snapshots, nil
}

// resticPartialTag tags the partial snapshots a whole-bucket snapshot run
// chains its backups on (resticsnap.TagPartial).
const resticPartialTag = "gos3-partial"

// snapshotJSON is the Snapshot document: a restic snapshot and whether it
// is a partial one (tagged resticPartialTag).
type snapshotJSON struct {
	restic.Snapshot
	Partial bool `json:"partial"`
}

// snapshotList converts a snapshot list (never null).
func snapshotList(list []restic.Snapshot) []snapshotJSON {
	out := make([]snapshotJSON, 0, len(list))
	for _, s := range list {
		out = append(out, snapshotJSON{Snapshot: s, Partial: s.HasTag(resticPartialTag)})
	}
	return out
}

// snapshotsDocument is the response of GET /buckets/{name}/restic/snapshots.
type snapshotsDocument struct {
	Config     json.RawMessage        `json:"config"`     // null when none
	Status     json.RawMessage        `json:"status"`     // SnapshotStatus
	Repository *restic.RepositoryInfo `json:"repository"` // null when none
	Snapshots  []snapshotJSON         `json:"snapshots"`
	// Error says why the repository or its snapshots could not be listed
	// (the rest of the document is still valid).
	Error string `json:"error,omitempty"`
}

// getSnapshots implements GET /buckets/{name}/restic/snapshots. A bucket
// without a schedule, or whose repository does not exist yet, has no
// snapshots; a repository that cannot be read is reported in "error" so
// that the schedule and its status still show.
func (a *api) getSnapshots(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	s, err := a.snapshots(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	out := snapshotsDocument{Snapshots: []snapshotJSON{}}
	cfg, err := s.Config(rc.ctx, bucket)
	switch {
	case err == nil:
		out.Config = cfg
	case !errors.Is(err, storage.ErrConfigNotFound):
		return 0, nil, err
	}
	if out.Status, err = s.Status(rc.ctx, bucket); err != nil && !errors.Is(err, storage.ErrConfigNotFound) {
		return 0, nil, err
	}
	if len(bytes.TrimSpace(out.Status)) == 0 {
		out.Status = json.RawMessage(`{}`)
	}
	partial := func(err error) error {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
			errors.Is(err, storage.ErrBucketNotFound):
			return err
		case errors.Is(err, storage.ErrConfigNotFound), noResticRepository(err):
			return nil
		}
		a.log.Warn("admin: cannot list whole-bucket snapshots", "bucket", bucket, "err", err)
		if out.Error == "" {
			out.Error = a.toAPIError(err).Message
		}
		return nil
	}
	repo, err := s.Repository(rc.ctx, bucket)
	if err != nil {
		if err := partial(err); err != nil {
			return 0, nil, err
		}
	}
	out.Repository = repo
	if out.Config != nil || repo != nil {
		list, err := s.Snapshots(rc.ctx, bucket)
		if err != nil {
			if err := partial(err); err != nil {
				return 0, nil, err
			}
		} else {
			out.Snapshots = snapshotList(list)
		}
	}
	return http.StatusOK, out, nil
}

// putSnapshotConfig implements PUT /buckets/{name}/restic/snapshots/config:
// the service validates the SnapshotConfig, prepares its repository and
// stores it. Changes of one bucket's schedule are serialized. A target on
// a named backing also needs the route's action on that backing
// (requireSnapshotBacking).
func (a *api) putSnapshotConfig(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	doc, err := rc.readJSONObject()
	if err != nil {
		return 0, nil, err
	}
	if err := a.requireSnapshotBacking(rc, doc); err != nil {
		return 0, nil, err
	}
	s, err := a.snapshots(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	defer a.configLocks.lock("restic-snapshots/" + bucket)()
	out, err := s.PutConfig(rc.ctx, bucket, doc)
	if err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: whole-bucket snapshot schedule set", "bucket", bucket, "by", describeIdentity(rc.id))
	return http.StatusOK, out, nil
}

// requireSnapshotBacking authorizes the backing a SnapshotConfig's target
// names: snapshots there put the bucket's data into a repository on that
// backing (whose credentials GetResticCredentials then discloses), so the
// caller needs admin:PutBucketSnapshots on arn:gos3:admin:::backing/<name>
// as well as on the bucket. The default target (the bucket's own
// repository, or one on the default backing) needs no more. It is checked
// before the service validates or prepares anything.
func (a *api) requireSnapshotBacking(rc *reqCtx, doc json.RawMessage) error {
	var in struct {
		Target *struct {
			Backing string `json:"backing"`
		} `json:"target"`
	}
	if err := json.Unmarshal(doc, &in); err != nil {
		return badRequest("invalid snapshot configuration: %s", jsonErrorMessage(err))
	}
	if in.Target == nil || in.Target.Backing == "" {
		return nil
	}
	return a.requireAction(rc, rc.action, iam.AdminBackingARN(in.Target.Backing), nil)
}

// deleteSnapshotConfig implements DELETE
// /buckets/{name}/restic/snapshots/config: scheduling stops, existing
// snapshots stay. It is idempotent.
func (a *api) deleteSnapshotConfig(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	s, err := a.snapshots(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	defer a.configLocks.lock("restic-snapshots/" + bucket)()
	if err := s.DeleteConfig(rc.ctx, bucket); err != nil && !errors.Is(err, storage.ErrConfigNotFound) {
		return 0, nil, err
	}
	a.log.Info("admin: whole-bucket snapshot schedule removed", "bucket", bucket, "by", describeIdentity(rc.id))
	return http.StatusNoContent, nil, nil
}

// runSnapshot implements POST /buckets/{name}/restic/snapshots: a
// restic-snapshot job now (409 while one is active for the bucket).
func (a *api) runSnapshot(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	s, err := a.snapshots(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	j, err := s.RunNow(rc.ctx, bucket, callerName(rc))
	if err != nil {
		return 0, nil, snapshotJobError(err)
	}
	a.log.Info("admin: whole-bucket snapshot started", "bucket", bucket, "job", j.ID, "by", describeIdentity(rc.id))
	return http.StatusAccepted, publicJob(j), nil
}

// pruneSnapshots implements POST /buckets/{name}/restic/prune: a
// restic-prune job of the bucket's snapshot repository.
func (a *api) pruneSnapshots(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	s, err := a.snapshots(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	j, err := s.Prune(rc.ctx, bucket, callerName(rc))
	if err != nil {
		return 0, nil, snapshotJobError(err)
	}
	a.log.Info("admin: snapshot repository prune started", "bucket", bucket, "job", j.ID, "by", describeIdentity(rc.id))
	return http.StatusAccepted, publicJob(j), nil
}

// snapshotJobError explains a snapshot or prune job that cannot start for
// a bucket without a schedule.
func snapshotJobError(err error) error {
	if errors.Is(err, storage.ErrConfigNotFound) {
		return badRequest("the bucket has no whole-bucket snapshot schedule: PUT …/restic/snapshots/config first")
	}
	return err
}

// validSnapshotID reports whether id names a restic snapshot: a full ID
// (64 lower-case hex digits, as the snapshot list reports them).
func validSnapshotID(id string) bool {
	if len(id) != restic.SnapshotIDSize {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// snapshotID returns the {id} path value, validated.
func snapshotID(rc *reqCtx) (string, error) {
	id := rc.r.PathValue("id")
	if !validSnapshotID(id) {
		return "", badRequest("snapshot ID %q must be 8 to %d lower-case hex digits", id, restic.SnapshotIDSize)
	}
	return id, nil
}

// forgetSnapshot implements DELETE /buckets/{name}/restic/snapshots/{id}:
// the service forgets one whole-bucket snapshot of the bucket, never a
// per-version archive snapshot (400).
func (a *api) forgetSnapshot(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	id, err := snapshotID(rc)
	if err != nil {
		return 0, nil, err
	}
	s, err := a.snapshots(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	if err := s.Forget(rc.ctx, bucket, id); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: whole-bucket snapshot forgotten", "bucket", bucket, "snapshot", id, "by", describeIdentity(rc.id))
	return http.StatusNoContent, nil, nil
}

// restoreTarget is the part of a restore request that authorization needs.
// It is decoded like the service decodes the whole body (encoding/json,
// into a struct), so both see the same target bucket.
type restoreTarget struct {
	Target struct {
		Bucket string `json:"bucket"`
	} `json:"target"`
}

// restoreSnapshot implements POST
// /buckets/{name}/restic/snapshots/{id}/restore: a restic-restore job
// restoring the snapshot into the target bucket the body names. Besides
// the route's admin:RestoreBucketSnapshot on the bucket, the caller needs
// admin:CreateCopyJob on the target bucket (it writes there like a copy
// job), evaluated before the service sees the request.
func (a *api) restoreSnapshot(rc *reqCtx) (int, any, error) {
	bucket := rc.r.PathValue("name")
	id, err := snapshotID(rc)
	if err != nil {
		return 0, nil, err
	}
	body, err := rc.readJSONObject()
	if err != nil {
		return 0, nil, err
	}
	var req restoreTarget
	if err := json.Unmarshal(body, &req); err != nil {
		return 0, nil, badRequest("invalid JSON body: %s", jsonErrorMessage(err))
	}
	target := req.Target.Bucket
	if err := a.requireAction(rc, actionCreateCopyJob, iam.AdminBucketARN(target), nil); err != nil {
		return 0, nil, err
	}
	if target == "" {
		return 0, nil, badRequest("\"target\": {\"bucket\": …} is required")
	}
	s, err := a.snapshots(rc.ctx, bucket)
	if err != nil {
		return 0, nil, err
	}
	j, err := s.Restore(rc.ctx, bucket, id, body, callerName(rc))
	if err != nil {
		return 0, nil, err
	}
	if err := a.checkJobBuckets(j, bucket, target); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: whole-bucket snapshot restore started", "bucket", bucket, "snapshot", id,
		"target", target, "job", j.ID, "by", describeIdentity(rc.id))
	return http.StatusAccepted, publicJob(j), nil
}

// readJSONObject reads a required body that must be one JSON object (its
// fields are for the service to validate).
func (rc *reqCtx) readJSONObject() (json.RawMessage, error) {
	data, err := rc.readBody()
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, badRequest("request body is required")
	}
	if data[0] != '{' {
		return nil, badRequest("invalid JSON body: the body must be a JSON object")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, badRequest("invalid JSON body: %s", jsonErrorMessage(err))
	}
	return json.RawMessage(data), nil
}
