package restic

import "context"

// CONTRACT (M3): the exported names in this file are implemented by the
// backends that keep restic repositories (fs, s3store, s3account, the
// multi router) and used by the whole-bucket snapshot scheduler
// (internal/resticsnap), the admin API's recovery information and the
// `gos3 restic` CLI.

// RepositoryInfo describes a restic repository for operators: where it is
// and where its password comes from. It never carries secrets.
type RepositoryInfo struct {
	// BucketID is the backend's ID of the bucket the repository belongs to
	// ("" for an aux repository).
	BucketID string `json:"bucketId,omitempty"`
	// Repository is restic's --repo (a directory, or "s3:https://host/bucket/path").
	Repository string `json:"repository"`
	// RecordLocation is where the repository's recovery record is kept
	// (a file path or an s3:// URL).
	RecordLocation string `json:"recordLocation,omitempty"`
	// PasswordSource explains how the password is obtained (for example
	// "field password of <record>" or "hex of the master key
	// s3://bucket/prefix/keys/<id>.key, unwrapped with the backing's
	// encryption_key").
	PasswordSource string `json:"passwordSource"`
	// Initialized is set once the repository exists.
	Initialized bool `json:"initialized"`
	// EnvNames are the names (not values) of the environment variables
	// restic needs besides RESTIC_REPOSITORY/RESTIC_PASSWORD (for example
	// AWS_ACCESS_KEY_ID).
	EnvNames []string `json:"envNames,omitempty"`
	// Options are restic's -o options for the repository.
	Options []string `json:"options,omitempty"`
	// Backing names the backing that holds the repository, when the code
	// that resolved it knows (the multi router, the snapshot service).
	// Authorizing access to the repository's credentials against it, rather
	// than against a separate lookup, cannot race with a placement or
	// configuration change. It is not serialized.
	Backing string `json:"-"`
}

// Host is implemented by backends (and the multi router) that keep restic
// repositories.
//
// ResticRepository returns the repository of bucket's per-bucket archive
// (the one storage.EncryptionRestic writes to), with create preparing the
// bucket's keys and recovery record first. It returns an error wrapping
// errors.ErrUnsupported for buckets whose backend keeps no repository (a
// native bucket of an s3 backing, a memory backend) and
// storage.ErrBucketNotFound for unknown buckets.
//
// ResticAuxRepository returns a repository that does not belong to a
// bucket of this backend, named name ([a-z0-9-]{1,64}): it holds
// whole-bucket snapshots of buckets that keep no repository themselves.
// The backend chooses its location and keeps its key (an fs backend in its
// system directory, an s3 backing under its home, wrapped by the backing's
// encryption key). It returns an error wrapping errors.ErrUnsupported when
// the backend cannot hold one (no system directory, no encryption key).
//
// The returned Location carries the password and credentials; callers
// never log or return it to API clients (RepositoryInfo is safe to show).
type Host interface {
	ResticRepository(ctx context.Context, bucket string, create bool) (RepositoryInfo, Location, error)
	ResticAuxRepository(ctx context.Context, name string, create bool) (RepositoryInfo, Location, error)
}
