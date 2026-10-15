// Package restic runs the restic executable for the backends that archive
// object versions with restic (storage.EncryptionRestic): each version is
// one snapshot (`restic backup --stdin`) in a per-bucket repository, and is
// read back with `restic dump`.
//
// The package is independent of where a backend keeps its repositories and
// recovery records. A backend describes a repository with a Location (the
// fs backend: a local directory; the s3 backend: restic's s3 backend inside
// its own bucket, see S3Repository) and its recovery records with a
// RecoveryStore, and drives a Runtime:
//
//   - Runtime.Prepare makes sure the repository exists (initializing a new
//     one) and that the bucket's recovery record is current;
//   - Runtime.Validate checks, without initializing anything, that a
//     repository is reachable or does not exist yet;
//   - Runtime.NewArchiver streams one version into a new snapshot;
//   - Runtime.Open returns a lazy reader of a snapshot, restored on first
//     use into an encrypted temporary file shared by concurrent readers.
//
// Every restic failure is reported wrapping storage.ErrEncryptionUnavailable
// (or the request's context error when the request ended), never as a plain
// error: it concerns one bucket's archive, not the health of the backend.
// Restic's diagnostics are kept bounded and have the repository's secrets
// redacted.
//
// Restic owns immutable snapshots. Keeping snapshots after S3 deletes
// provides an archive and makes open readers safe across concurrent
// overwrites and deletes; operators must not forget snapshots still
// referenced by gos3 metadata.
package restic

import (
	"context"
	"encoding/hex"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// Location is how restic reaches one repository.
type Location struct {
	// Repository is restic's --repo: a local directory, or for example
	// "s3:https://host/bucket/path" (see S3Repository).
	Repository string
	// Password is the repository password (see PasswordFromKey).
	Password string
	// Env holds extra "NAME=value" environment entries for restic, such as
	// the AWS_* credentials of an s3 repository. Restic never inherits
	// RESTIC_* or AWS_* variables from the server's environment.
	Env []string
	// Options are extended options passed as `-o <option>` (for example
	// "s3.bucket-lookup=path").
	Options []string
	// CacheDir is restic's local cache (--cache-dir), created on demand;
	// "" runs restic without one (--no-cache). A cache spares every
	// invocation downloading the repository's index files again; its
	// contents are encrypted by restic and can be deleted at any time.
	CacheDir string
}

// secrets returns the values that must never appear in logs or errors: the
// password and the values of secret-looking environment entries.
func (l Location) secrets() []string {
	s := []string{l.Password}
	for _, e := range l.Env {
		name, value, _ := strings.Cut(e, "=")
		name = strings.ToUpper(name)
		for _, marker := range []string{"KEY", "SECRET", "TOKEN", "PASSWORD"} {
			if strings.Contains(name, marker) {
				s = append(s, value)
				break
			}
		}
	}
	return s
}

// Record is a bucket's recovery record: which repository holds its
// archive and whether it was initialized. It is kept independently of the
// bucket's metadata (and after the bucket is deleted), so that the archive
// stays recoverable. Backends may store more (fs stores the password too).
type Record struct {
	// Initialized is set once the repository is known to exist; from then
	// on a missing repository is an error instead of being created anew.
	Initialized bool `json:"initialized,omitempty"`
	// Repository is Location.Repository.
	Repository string `json:"repository"`
}

// RecoveryStore loads and stores the recovery records of a backend's
// buckets, by bucket ID.
type RecoveryStore interface {
	// Load returns the record of the bucket with ID id, or the zero
	// Record when it has none.
	Load(ctx context.Context, id string) (Record, error)
	// Store saves the record of the bucket with ID id. Prepare stores the
	// same record more than once; implementations may skip writes that
	// would not change the stored record.
	Store(ctx context.Context, id string, rec Record) error
}

// PasswordFromKey derives the password of a bucket's repository from the
// bucket's master key: its lower-case hex encoding.
func PasswordFromKey(master []byte) string { return hex.EncodeToString(master) }

// SnapshotIDSize is the length of a snapshot ID in hex.
const SnapshotIDSize = 64

// ValidSnapshot reports whether id is a full restic snapshot ID (64
// lower-case hex digits).
func ValidSnapshot(id string) bool {
	if len(id) != SnapshotIDSize {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// S3Repository describes a repository in restic's s3 backend.
type S3Repository struct {
	// Endpoint is the service URL, "http(s)://host[:port]".
	Endpoint string
	Bucket   string
	// Path is the repository's key prefix inside Bucket, without leading
	// or trailing slashes (for example "gos3/restic/<bucket-id>").
	Path string
	// Region is the signing region ("" lets restic choose).
	Region                             string
	AccessKey, SecretKey, SessionToken string
	// PathStyle addresses the bucket as <endpoint>/<bucket> instead of
	// <bucket>.<host> (restic's s3.bucket-lookup=path|dns).
	PathStyle bool
}

// Location returns the Location of the repository r, with the password and
// cache directory given. Credentials travel in the AWS_* environment
// entries; the repository string holds none, so it can be stored in a
// Record.
func (r S3Repository) Location(password, cacheDir string) Location {
	repo := "s3:" + strings.TrimRight(r.Endpoint, "/") + "/" + r.Bucket
	if p := strings.Trim(r.Path, "/"); p != "" {
		repo += "/" + p
	}
	lookup := "dns"
	if r.PathStyle {
		lookup = "path"
	}
	loc := Location{
		Repository: repo,
		Password:   password,
		Env: []string{
			"AWS_ACCESS_KEY_ID=" + r.AccessKey,
			"AWS_SECRET_ACCESS_KEY=" + r.SecretKey,
		},
		Options:  []string{"s3.bucket-lookup=" + lookup},
		CacheDir: cacheDir,
	}
	if r.SessionToken != "" {
		loc.Env = append(loc.Env, "AWS_SESSION_TOKEN="+r.SessionToken)
	}
	if r.Region != "" {
		loc.Env = append(loc.Env, "AWS_DEFAULT_REGION="+r.Region)
		loc.Options = append(loc.Options, "s3.region="+r.Region)
	}
	return loc
}

// Runtime runs restic for one backend: it tracks which repositories this
// process prepared and the snapshots being restored for readers. The zero
// value is ready to use; a Runtime must not be copied after first use.
type Runtime struct {
	// Log receives a warning for every failed restic invocation (nil
	// discards them).
	Log *slog.Logger
	// TempDir holds the encrypted restores of snapshots being read ("":
	// os.TempDir()).
	TempDir string
	// LookPath finds the restic executable (default exec.LookPath; tests
	// replace it).
	LookPath func(file string) (string, error)

	mu     sync.Mutex
	repos  map[string]*repoState // by Location.Repository
	spools map[string]*spool     // by spoolKey
}

// repoState is the preparation state of one repository.
type repoState struct {
	mu sync.Mutex // serializes preparing the repository
	// prepared is set once this process found (or made) the repository
	// initialized and recorded it; it is not checked again.
	prepared bool
}

// repo returns the preparation state of the repository repo.
func (rt *Runtime) repo(repo string) *repoState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.repos == nil {
		rt.repos = make(map[string]*repoState)
	}
	st := rt.repos[repo]
	if st == nil {
		st = &repoState{}
		rt.repos[repo] = st
	}
	return st
}

// log returns the runtime's logger.
func (rt *Runtime) log() *slog.Logger {
	if rt.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return rt.Log
}

// lookPath returns the executable lookup function.
func (rt *Runtime) lookPath() func(string) (string, error) {
	if rt.LookPath == nil {
		return exec.LookPath
	}
	return rt.LookPath
}

// Installed reports whether a restic executable is on the server's PATH
// (its version is only checked when it is run).
func (rt *Runtime) Installed() bool {
	_, err := rt.lookPath()("restic")
	return err == nil
}
