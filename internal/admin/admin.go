// Package admin implements the gos3 admin REST API (JSON) mounted at
// /_gos3/admin/v1/ — server info & stats, runtime settings, IAM users / access
// keys / policies, bucket overview & management helpers, storage backings,
// encryption providers and restic recovery, background jobs (copy, mirror,
// whole-bucket restic snapshots) and discovery. It is specified in
// docs/admin-api.md and consumed by the web UI.
//
// Requests are authenticated with AWS SigV4 (same credentials as S3) and
// authorized with iam actions "admin:<Action>".
//
// CONTRACT: Deps and NewHandler are fixed. Implementation is owned by the
// admin work item (replace the stub, add files freely).
package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/clientip"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Prefix is the mount point of the admin API.
const Prefix = "/_gos3/admin/v1"

// Deps are the admin API's dependencies.
type Deps struct {
	Backend   storage.Backend
	Verifier  *auth.Verifier
	IAM       *iam.Manager
	Metrics   *metrics.Registry // may be nil
	Settings  *config.Store
	Discovery *discovery.Service // may be nil when disabled
	// Info returns this server's public identity document.
	Info func() discovery.Info
	// BackendName is the kind of the default backing: "fs", "memory" or
	// "s3". DataPath is the data directory: the fs backing's directory, or
	// the directory holding only system state for memory and s3 ("" if
	// none).
	BackendName string
	DataPath    string
	Logger      *slog.Logger
	// TrustProxyHeaders derives the aws:SourceIp and aws:SecureTransport
	// condition keys, honouring X-Forwarded-For / X-Forwarded-Proto from
	// trusted reverse proxies (server.trust_proxy_headers and
	// server.trusted_proxies) exactly like the S3 API, so one policy gives
	// the same answer on both APIs behind a reverse proxy. The zero value
	// ignores those headers.
	TrustProxyHeaders clientip.Resolver

	// Jobs runs the background jobs (copy and mirror jobs, restic
	// snapshot, restore and prune jobs) listed and controlled by the /jobs
	// endpoints. nil: those endpoints answer 404 ("not available").
	Jobs *jobs.Manager
	// CopyJobs validates copy and mirror job specs for POST /jobs with
	// "dryRun" (*copyjob.Service). nil: dry runs answer 404; jobs are
	// still submitted through Jobs.
	CopyJobs CopyJobService
	// Snapshots manages whole-bucket restic snapshots
	// (*resticsnap.Service). nil: the /buckets/{name}/restic/snapshots
	// endpoints answer 404 and GET /buckets/{name}/restic reports no
	// snapshot schedule.
	Snapshots SnapshotService
}

// CopyJobService checks copy and mirror job specs without creating jobs
// (implemented by the copy job runner).
type CopyJobService interface {
	// DryRun validates spec (a CopyJobSpec without "dryRun") and returns
	// its normalized form and warnings about what the job would do or skip.
	// Errors wrap jobs.ErrInvalidSpec for mistakes in the spec.
	DryRun(ctx context.Context, spec json.RawMessage) (norm json.RawMessage, warnings []string, err error)
}

// SnapshotService manages the whole-bucket restic snapshots of buckets
// (docs/admin-api.md "Whole-bucket restic snapshots"). Errors: unknown
// buckets wrap storage.ErrBucketNotFound; invalid documents and requests
// jobs.ErrInvalidSpec; a bucket without a snapshot repository
// errors.ErrUnsupported (or storage.ErrConfigNotFound when no schedule is
// configured); a repository not created yet io/fs.ErrNotExist; a snapshot
// that is not there restic.ErrSnapshotNotFound, one that may not be
// forgotten (a per-version archive snapshot) restic.ErrForgetRefused; a job
// that is already running jobs.ErrConflict; restic failures
// storage.ErrEncryptionUnavailable.
type SnapshotService interface {
	// Config returns the bucket's SnapshotConfig document
	// (storage.ErrConfigNotFound when none).
	Config(ctx context.Context, bucket string) (json.RawMessage, error)
	// PutConfig validates doc, prepares its repository, stores it and
	// returns the stored (normalized) document.
	PutConfig(ctx context.Context, bucket string, doc json.RawMessage) (json.RawMessage, error)
	// DeleteConfig removes the schedule (existing snapshots stay).
	DeleteConfig(ctx context.Context, bucket string) error
	// Status returns the bucket's SnapshotStatus document.
	Status(ctx context.Context, bucket string) (json.RawMessage, error)
	// Repository describes the repository the bucket's snapshots go to
	// (nil when none is configured), without secrets.
	Repository(ctx context.Context, bucket string) (*restic.RepositoryInfo, error)
	// Snapshots lists the bucket's whole-bucket snapshots (the service
	// caches the list).
	Snapshots(ctx context.Context, bucket string) ([]restic.Snapshot, error)
	// RunNow submits a restic-snapshot job for the bucket.
	RunNow(ctx context.Context, bucket, createdBy string) (jobs.Job, error)
	// Forget forgets one whole-bucket snapshot of the bucket.
	Forget(ctx context.Context, bucket, snapshotID string) error
	// Restore submits a restic-restore job restoring the snapshot into the
	// target bucket that req (the request body) names.
	Restore(ctx context.Context, bucket, snapshotID string, req json.RawMessage, createdBy string) (jobs.Job, error)
	// Prune submits a restic-prune job for the bucket's repository.
	Prune(ctx context.Context, bucket, createdBy string) (jobs.Job, error)
	// Credentials returns how to reach the bucket's snapshot repository,
	// secrets included (for GetResticCredentials only).
	Credentials(ctx context.Context, bucket string) (restic.Location, *restic.RepositoryInfo, error)
}

// NewHandler returns the admin API handler. It expects full request paths
// (starting with Prefix).
//
// The handler answers CORS preflights (OPTIONS) from the global CORS
// settings without authentication, authenticates every other request with
// d.Verifier (SigV4 header or presigned URL; anonymous requests get 401),
// authorizes it with d.IAM ("admin:<Action>", see docs/admin-api.md) and
// renders JSON responses; errors use the envelope
// {"error":{"code":"…","message":"…"}}.
//
// Backend, Verifier, IAM and Settings are required; NewHandler panics if one
// of them is nil (a wiring bug). Metrics, Discovery, Info, Logger, Jobs,
// CopyJobs and Snapshots are optional.
func NewHandler(d Deps) http.Handler {
	return newAPI(d)
}
