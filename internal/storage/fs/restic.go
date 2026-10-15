package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Restic-archived versions (storage.EncryptionRestic) are run by package
// restic; this file keeps where the fs backend puts their repositories and
// recovery records:
//
//   - the repository of a bucket is the local directory
//     restic/<bucket-id> below the system directory, its password the hex
//     of the bucket's master key (restic.PasswordFromKey), and restic's
//     cache is restic-cache/, shared by all of the backing's repositories;
//   - the recovery record keys/<bucket-id>.restic.json (resticRepository)
//     keeps the repository and its password after the bucket is deleted;
//   - the data file of a restic version holds only its snapshot ID.

// resticRepository is the JSON form of a bucket's recovery record,
// keys/<bucket-id>.restic.json: restic.Record plus the repository's
// password, so that the archive stays recoverable without gos3.
type resticRepository struct {
	Initialized bool   `json:"initialized,omitempty"`
	Repository  string `json:"repository"`
	Password    string `json:"password"`
}

// resticCacheDir (below the system directory) is restic's persistent cache
// of repository indexes and metadata, shared by all of the backing's
// repositories. Without it every invocation would download every index
// file of the repository again. Its contents are encrypted by restic and
// can be deleted at any time (restic rebuilds them).
const resticCacheDir = "restic-cache"

// snapshotRefSize is the size of a restic version's local data file, which
// holds only its snapshot ID in hex.
const snapshotRefSize = restic.SnapshotIDSize

// validSnapshot reports whether id is a restic snapshot ID as stored in a
// data file.
func validSnapshot(id string) bool { return restic.ValidSnapshot(id) }

// resticState is a Backend's in-process restic state: the runtime, created
// on first use (it tracks prepared repositories and shared restores).
type resticState struct {
	once sync.Once
	rt   *restic.Runtime
}

// resticRuntime returns the backend's restic runtime. Restores of
// snapshots being read are spooled (encrypted) in tmp/.
func (b *Backend) resticRuntime() *restic.Runtime {
	b.restic.once.Do(func() { b.restic.rt = &restic.Runtime{Log: b.log, TempDir: b.tmpDir} })
	return b.restic.rt
}

// resticRepository returns the local repository of the bucket with ID
// id. Repositories always live on the backing that holds the bucket.
func (b *Backend) resticRepository(id string) string {
	return filepath.Join(b.sysDir, "restic", id)
}

// resticLocation returns how to reach the repository of the bucket with ID
// id. It needs the bucket's master key, and fails when the bucket's
// recovery record names a repository this backing cannot reach (see
// readRecovery).
func (b *Backend) resticLocation(id string) (restic.Location, error) {
	key, err := b.bucketKey(id, false)
	if err != nil {
		return restic.Location{}, err
	}
	if _, _, err := readRecovery(b.recoveryPath(id)); err != nil {
		return restic.Location{}, err
	}
	return restic.Location{
		Repository: b.resticRepository(id),
		Password:   restic.PasswordFromKey(key),
		CacheDir:   filepath.Join(b.sysDir, resticCacheDir),
	}, nil
}

// --- recovery records ---

// recoveryPath returns the path of the recovery record of the bucket with
// ID id.
func (b *Backend) recoveryPath(id string) string {
	return filepath.Join(b.sysDir, keysDirName, id+".restic.json")
}

// readRecovery returns the recovery record at path (zero when it does not
// exist) and its raw content. A record of a repository on S3 fails closed:
// such repositories came from per-backing restic S3 settings that no longer
// exist, and writing on would start a new, empty local repository.
func readRecovery(path string) (resticRepository, []byte, error) {
	var rec resticRepository
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil, nil
	}
	if err != nil {
		return rec, nil, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, nil, fmt.Errorf("fs: invalid restic recovery record %s: %w", path, err)
	}
	if strings.HasPrefix(rec.Repository, "s3:") {
		return rec, nil, fmt.Errorf("%w: restic repository on S3 settings that no longer exist (recovery record %s); restore the bucket's archive from that repository",
			storage.ErrEncryptionUnavailable, path)
	}
	return rec, raw, nil
}

// resticRecords is the backend's restic.RecoveryStore: the recovery record
// files, which also hold the repository password.
type resticRecords struct{ b *Backend }

// Load implements restic.RecoveryStore.
func (s resticRecords) Load(_ context.Context, id string) (restic.Record, error) {
	rec, _, err := readRecovery(s.b.recoveryPath(id))
	return restic.Record{Initialized: rec.Initialized, Repository: rec.Repository}, err
}

// Store implements restic.RecoveryStore. An unchanged record is not
// rewritten.
func (s resticRecords) Store(_ context.Context, id string, rec restic.Record) error {
	key, err := s.b.bucketKey(id, false)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(resticRepository{
		Initialized: rec.Initialized,
		Repository:  rec.Repository,
		Password:    restic.PasswordFromKey(key),
	})
	if err != nil {
		return err
	}
	path := s.b.recoveryPath(id)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, raw) {
		return nil
	}
	return config.WriteFileAtomic(path, raw, filePerm)
}

// --- preparing repositories ---

// prepareRestic makes sure the repository of the bucket with ID id exists,
// initializing a new one, and that its recovery record is current (see
// restic.Runtime.Prepare). A repository found ready is not checked again
// by this process.
func (b *Backend) prepareRestic(ctx context.Context, id string) error {
	loc, err := b.resticLocation(id)
	if err != nil {
		return err
	}
	return b.resticRuntime().Prepare(ctx, id, loc, resticRecords{b})
}

// ValidateEncryptionSettings checks that the bucket can use cfg's provider.
// For restic it checks (without initializing anything) that the bucket's
// repository is either reachable with its password or does not exist yet;
// it creates the bucket's master key, from which that password is derived.
func (b *Backend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	if cfg.Provider != storage.EncryptionRestic {
		return b.PrepareEncryption(ctx, bucket, cfg.Provider)
	}
	id, err := b.encryptionBucketID(ctx, bucket)
	if err != nil {
		return err
	}
	if _, err = b.bucketKey(id, true); err != nil {
		return err
	}
	loc, err := b.resticLocation(id)
	if err != nil {
		return err
	}
	return b.resticRuntime().Validate(ctx, id, loc, resticRecords{b})
}
