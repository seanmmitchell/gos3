package fs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// The fs backend implements restic.Host:
//
//   - a bucket's repository is the one its restic-archived versions go to
//     (restic/<bucket-id> below the system directory, see restic.go), with
//     the recovery record keys/<bucket-id>.restic.json;
//   - an aux repository <name> (whole-bucket snapshots of buckets that keep
//     no repository of their own) is restic-aux/<name> below the system
//     directory. Its password is the hex of a random 32-byte key in
//     keys/aux/<name> (created once and never regenerated: a missing key of
//     a repository that has a record is an error asking for it to be
//     restored), and its recovery record keys/aux/<name>.restic.json holds
//     the repository and the password, like a bucket's.

var _ restic.Host = (*Backend)(nil)

// resticAuxDir (below the system directory) holds the aux repositories,
// auxKeysDir (below the key directory) their keys and recovery records.
const (
	resticAuxDir = "restic-aux"
	auxKeysDir   = "aux"
)

// maxAuxName is the longest aux repository name.
const maxAuxName = 64

// validAuxName reports whether name is a valid aux repository name:
// [a-z0-9-]{1,64}.
func validAuxName(name string) bool {
	if name == "" || len(name) > maxAuxName {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// errNoRepository reports that what (a bucket or an aux repository) has no
// restic repository yet (a lookup without create).
func errNoRepository(what string) error {
	return fmt.Errorf("fs: %s has no restic repository yet: %w: %w", what, errors.ErrUnsupported, iofs.ErrNotExist)
}

// ResticRepository implements restic.Host: the repository of bucket's
// restic archive. With create, the bucket's master key is created if
// needed and the repository prepared (initialized, and its recovery record
// written with the bucket's name), as for a first restic write. Without
// create nothing is created: a bucket whose repository has no recovery
// record yet answers an error wrapping errors.ErrUnsupported and
// fs.ErrNotExist.
func (b *Backend) ResticRepository(ctx context.Context, bucket string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	id, err := b.encryptionBucketID(ctx, bucket)
	if err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	if create {
		if _, err := b.bucketKey(id, true); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		if err := b.prepareResticFor(ctx, id, bucket); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
	}
	rec, _, err := readRecovery(b.recoveryPath(id))
	switch {
	case err != nil:
		return restic.RepositoryInfo{}, restic.Location{}, err
	case rec.Repository == "":
		return restic.RepositoryInfo{}, restic.Location{}, errNoRepository(fmt.Sprintf("bucket %q", bucket))
	}
	loc, err := b.resticLocation(id)
	if err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	record := b.recoveryPath(id)
	return restic.RepositoryInfo{
		BucketID:       id,
		Repository:     loc.Repository,
		RecordLocation: record,
		PasswordSource: fmt.Sprintf("field password of %s (hex of %s)", record, filepath.Join(b.sysDir, keysDirName, id)),
		Initialized:    rec.Initialized,
	}, loc, nil
}

// ResticAuxRepository implements restic.Host: the aux repository name,
// restic-aux/<name> below the system directory (see the top of the file).
// With create, its key is created if needed and the repository prepared;
// without create, a repository without a recovery record answers an error
// wrapping errors.ErrUnsupported and fs.ErrNotExist. An invalid name is
// storage.ErrInvalidName.
func (b *Backend) ResticAuxRepository(ctx context.Context, name string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	if err := b.begin(ctx); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	if !validAuxName(name) {
		return restic.RepositoryInfo{}, restic.Location{}, fmt.Errorf("%w: restic aux repository %q (want [a-z0-9-]{1,%d})", storage.ErrInvalidName, name, maxAuxName)
	}
	record := b.auxRecordPath(name)
	var key []byte
	if create {
		var err error
		if key, err = b.createAuxKey(name); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		if err := b.resticRuntime().Prepare(ctx, name, b.auxLocation(name, key), auxRecords{b}); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
	}
	rec, _, err := readRecovery(record)
	switch {
	case err != nil:
		return restic.RepositoryInfo{}, restic.Location{}, err
	case rec.Repository == "":
		return restic.RepositoryInfo{}, restic.Location{}, errNoRepository(fmt.Sprintf("aux repository %q", name))
	}
	if key == nil {
		if key, err = b.loadAuxKey(name); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
	}
	loc := b.auxLocation(name, key)
	return restic.RepositoryInfo{
		Repository:     loc.Repository,
		RecordLocation: record,
		PasswordSource: fmt.Sprintf("field password of %s (hex of %s)", record, b.auxKeyPath(name)),
		Initialized:    rec.Initialized,
	}, loc, nil
}

// auxRepository returns the directory of the aux repository name.
func (b *Backend) auxRepository(name string) string {
	return filepath.Join(b.sysDir, resticAuxDir, name)
}

// auxKeyPath returns the key file of the aux repository name.
func (b *Backend) auxKeyPath(name string) string {
	return filepath.Join(b.sysDir, keysDirName, auxKeysDir, name)
}

// auxRecordPath returns the recovery record of the aux repository name.
func (b *Backend) auxRecordPath(name string) string {
	return filepath.Join(b.sysDir, keysDirName, auxKeysDir, name+".restic.json")
}

// auxLocation returns how restic reaches the aux repository name, whose
// key is key.
func (b *Backend) auxLocation(name string, key []byte) restic.Location {
	return restic.Location{
		Repository: b.auxRepository(name),
		Password:   restic.PasswordFromKey(key),
		CacheDir:   filepath.Join(b.sysDir, resticCacheDir),
	}
}

// loadAuxKey reads the key of the aux repository name; a missing key is
// storage.ErrEncryptionUnavailable.
func (b *Backend) loadAuxKey(name string) ([]byte, error) {
	path := b.auxKeyPath(name)
	key, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%w: the key %s of restic aux repository %q is missing; restore it from backup", storage.ErrEncryptionUnavailable, path, name)
	case err != nil:
		return nil, fmt.Errorf("fs: read restic key: %w", err)
	case len(key) != crypt.KeySize:
		return nil, fmt.Errorf("fs: restic key %s has %d bytes, want %d", path, len(key), crypt.KeySize)
	}
	// A key restored from a backup may have lost its private mode.
	b.tightenPerm(path)
	return key, nil
}

// createAuxKey returns the key of the aux repository name, creating it
// when the repository has neither a key, a recovery record nor a
// directory. A key is never created for a repository that has one of
// them: a new key could not open it (restore the key from backup).
func (b *Backend) createAuxKey(name string) ([]byte, error) {
	b.restic.auxMu.Lock()
	defer b.restic.auxMu.Unlock()
	key, err := b.loadAuxKey(name)
	if !errors.Is(err, storage.ErrEncryptionUnavailable) {
		return key, err
	}
	path := b.auxKeyPath(name)
	rec, _, rerr := readRecovery(b.auxRecordPath(name))
	if rerr != nil {
		return nil, rerr
	}
	_, serr := os.Stat(b.auxRepository(name))
	if rec.Repository != "" || !errors.Is(serr, os.ErrNotExist) {
		return nil, err // the key of an existing repository is missing
	}
	keys := filepath.Join(b.sysDir, keysDirName)
	for _, dir := range []string{keys, filepath.Dir(path)} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("fs: create key directory: %w", err)
		}
		if err := config.RestrictDir(dir); err != nil {
			return nil, fmt.Errorf("fs: protect key directory: %w", err)
		}
	}
	key = make([]byte, crypt.KeySize)
	_, _ = rand.Read(key)
	if err := config.WriteFileAtomic(path, key, filePerm); err != nil {
		return nil, fmt.Errorf("fs: write restic key: %w", err)
	}
	b.log.Info("fs: created the key of a restic aux repository", "repository", b.auxRepository(name), "key", path)
	return key, nil
}

// auxRecords is the restic.RecoveryStore of aux repositories, by name: the
// records keys/aux/<name>.restic.json, which also hold the password.
type auxRecords struct{ b *Backend }

// Load implements restic.RecoveryStore.
func (s auxRecords) Load(_ context.Context, name string) (restic.Record, error) {
	rec, _, err := readRecovery(s.b.auxRecordPath(name))
	return rec.record(), err
}

// Store implements restic.RecoveryStore. An unchanged record is not
// rewritten.
func (s auxRecords) Store(_ context.Context, name string, rec restic.Record) error {
	key, err := s.b.loadAuxKey(name)
	if err != nil {
		return err
	}
	return writeRecovery(s.b.auxRecordPath(name), rec, key)
}
