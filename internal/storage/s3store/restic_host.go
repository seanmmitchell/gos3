package s3store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	iofs "io/fs"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// The s3 backend implements restic.Host:
//
//   - a bucket's repository is the one its restic-archived versions go to
//     (<Bucket>/<Prefix>restic/<bucket-id>, see resticstore.go), with the
//     recovery record keys/<bucket-id>.restic.json; its password is the hex
//     of the bucket's master key keys/<bucket-id>.key, unwrapped with the
//     backing's KEK;
//   - an aux repository <name> (whole-bucket snapshots of buckets that keep
//     no repository of their own) is <Bucket>/<Prefix>restic-aux/<name>,
//     with the recovery record keys/aux/<name>.restic.json (no password).
//     Its password is the hex of a random 32-byte key wrapped by the KEK in
//     keys/aux/<name>.key (see bucketkeys.go), created with If-None-Match
//     and never regenerated: a missing key of a repository that has a
//     record or data is an error asking for it to be restored.
//
// Both need the KEK and the credentials restic uses (errors.ErrUnsupported
// otherwise). Restic reaches them with the backing's current connection.

var _ restic.Host = (*Backend)(nil)

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
	return fmt.Errorf("s3store: %s has no restic repository yet: %w: %w", what, errors.ErrUnsupported, iofs.ErrNotExist)
}

// s3URL returns the s3:// URL of key (with the prefix) in the backing's
// bucket.
func (b *Backend) s3URL(key string) string { return "s3://" + b.opts.Bucket + "/" + key }

// envNames returns the names of the "NAME=value" entries of env.
func envNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		out = append(out, name)
	}
	return out
}

// ResticRepository implements restic.Host: the repository of bucket's
// restic archive. With create, the bucket's master key is created if
// needed and the repository prepared (initialized, and its recovery record
// written), as for a first restic write. Without create nothing is created:
// a bucket whose repository has no recovery record yet answers an error
// wrapping errors.ErrUnsupported and fs.ErrNotExist. A backing without a
// KEK or without the credentials restic needs answers errors.ErrUnsupported
// (wrapped).
func (b *Backend) ResticRepository(ctx context.Context, bucket string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	lsn := bk.lsn
	bk.mu.RUnlock()
	if err := b.waitRead(ctx, lsn); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	if err := b.checkWriteOptions(storage.WriteOptions{Encryption: storage.EncryptionRestic}); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	var (
		loc         restic.Location
		initialized bool
	)
	if create {
		if _, err := b.bucketKey(ctx, bk, true); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		if loc, err = b.prepareRestic(ctx, bk); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		initialized = true
	} else {
		rec, err := b.recovery.Load(ctx, bk.id)
		switch {
		case err != nil:
			return restic.RepositoryInfo{}, restic.Location{}, err
		case rec.Repository == "":
			return restic.RepositoryInfo{}, restic.Location{}, errNoRepository(fmt.Sprintf("bucket %q", bucket))
		}
		if loc, err = b.resticLocation(ctx, bk); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		initialized = rec.Initialized
	}
	return restic.RepositoryInfo{
		BucketID:       bk.id,
		Repository:     loc.Repository,
		RecordLocation: b.s3URL(b.recoveryObject(bk.id)),
		PasswordSource: fmt.Sprintf("hex of the master key %s, unwrapped with the backing's encryption_key", b.s3URL(b.keyObject(bk.id))),
		Initialized:    initialized,
		EnvNames:       envNames(loc.Env),
		Options:        slices.Clone(loc.Options),
	}, loc, nil
}

// ResticAuxRepository implements restic.Host: the aux repository name at
// <Bucket>/<Prefix>restic-aux/<name> (see the top of the file). With
// create, its key is created if needed and the repository prepared;
// without create, a repository without a recovery record answers an error
// wrapping errors.ErrUnsupported and fs.ErrNotExist. A backing without a
// KEK or without the credentials restic needs answers errors.ErrUnsupported
// (wrapped); an invalid name is storage.ErrInvalidName. The backing must
// be loaded (its marker names the backing the key is bound to).
func (b *Backend) ResticAuxRepository(ctx context.Context, name string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	if !validAuxName(name) {
		return restic.RepositoryInfo{}, restic.Location{}, fmt.Errorf("%w: restic aux repository %q (want [a-z0-9-]{1,%d})", storage.ErrInvalidName, name, maxAuxName)
	}
	if _, err := b.current(ctx); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	if err := b.checkWriteOptions(storage.WriteOptions{Encryption: storage.EncryptionRestic}); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	id := auxKeysDir + name // the record's ID and the key's cache ID
	var (
		key         []byte
		loc         restic.Location
		initialized bool
		err         error
	)
	if create {
		if key, err = b.auxKey(ctx, name, true); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		if loc, err = b.auxLocation(name, key); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		if err := b.restic.Prepare(ctx, id, loc, &b.recovery); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		initialized = true
	} else {
		rec, err := b.recovery.Load(ctx, id)
		switch {
		case err != nil:
			return restic.RepositoryInfo{}, restic.Location{}, err
		case rec.Repository == "":
			return restic.RepositoryInfo{}, restic.Location{}, errNoRepository(fmt.Sprintf("aux repository %q", name))
		}
		if key, err = b.auxKey(ctx, name, false); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		if loc, err = b.auxLocation(name, key); err != nil {
			return restic.RepositoryInfo{}, restic.Location{}, err
		}
		initialized = rec.Initialized
	}
	return restic.RepositoryInfo{
		Repository:     loc.Repository,
		RecordLocation: b.s3URL(b.recoveryObject(id)),
		PasswordSource: fmt.Sprintf("hex of the key %s, unwrapped with the backing's encryption_key", b.s3URL(b.auxKeyObject(name))),
		Initialized:    initialized,
		EnvNames:       envNames(loc.Env),
		Options:        slices.Clone(loc.Options),
	}, loc, nil
}

// auxKeyObject returns the key object of the aux repository name.
func (b *Backend) auxKeyObject(name string) string {
	return b.keys.prefix + keysDir + auxKeysDir + name + ".key"
}

// auxLocation returns how restic reaches the aux repository name, whose
// key is key, with the current connection.
func (b *Backend) auxLocation(name string, key []byte) (restic.Location, error) {
	repo, ok := b.s3Repository(resticAuxDir + name)
	if !ok {
		return restic.Location{}, fmt.Errorf("s3store: restic needs the backing's access key: %w", errors.ErrUnsupported)
	}
	return repo.Location(restic.PasswordFromKey(key), b.resticCacheDir()), nil
}

// auxKey returns the key of the aux repository name. With create, a
// repository that never had a key gets a new one (see loadAuxKey); without
// create a missing key is an error. Keys are cached once loaded; failures
// are not cached, so a restored key object is picked up.
func (b *Backend) auxKey(ctx context.Context, name string, create bool) ([]byte, error) {
	kek := b.conn.Load().kek
	if len(kek) == 0 {
		return nil, b.errNoKEK(fmt.Errorf("restic aux repository: %w", errors.ErrUnsupported))
	}
	id := auxKeysDir + name
	b.keyMu.RLock()
	e := b.bucketKeys[id]
	if e != nil && e.key != nil {
		key := e.key
		b.keyMu.RUnlock()
		return key, nil
	}
	b.keyMu.RUnlock()
	if e == nil {
		e = b.keyEntryOf(id)
	}
	e.load.Lock()
	defer e.load.Unlock()
	if e.key != nil {
		return e.key, nil
	}
	key, err := b.loadAuxKey(ctx, name, kek, create)
	if err != nil {
		return nil, err
	}
	b.keyMu.Lock()
	e.key = key
	b.keyMu.Unlock()
	return key, nil
}

// loadAuxKey reads the key of the aux repository name or, with create and
// no key object, creates it (If-None-Match: concurrent or retried
// creations end up with the one key that was stored). A key is never
// created for a repository that has a recovery record or data: a new key
// could not open it.
func (b *Backend) loadAuxKey(ctx context.Context, name string, kek []byte, create bool) ([]byte, error) {
	r := remote{c: b.client(), bucket: b.opts.Bucket}
	obj := b.auxKeyObject(name)
	aad := auxKeyAAD(b.eng.Marker().BackingID, name)
	for range 3 {
		data, _, err := r.getBytes(ctx, obj, maxKeyObject)
		switch {
		case err == nil:
			return unwrapKeyFile(data, kek, aad, obj)
		case !s3client.IsNotFound(err):
			return nil, err
		case !create:
			return nil, fmt.Errorf("%w: the key %s of restic aux repository %q is missing; restore it from backup",
				storage.ErrEncryptionUnavailable, obj, name)
		}
		if err := b.checkAuxUnused(ctx, name, obj); err != nil {
			return nil, err
		}
		if err := b.recordKEK(ctx, kek); err != nil {
			return nil, err
		}
		key := make([]byte, crypt.KeySize)
		_, _ = rand.Read(key)
		wrapped, err := crypt.Wrap(kek, key, aad)
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(keyFile{V: keyFileVersion, Alg: keyAlgorithm, Wrapped: wrapped, KEK: crypt.KeyCheck(kek)})
		_, err = r.put(ctx, obj, raw, s3client.PutOptions{IfNoneMatch: true, NoRetry: true})
		switch {
		case err == nil:
			b.log.Info("s3store: created the key of a restic aux repository", "backing", b.opts.Name, "key", obj)
			return key, nil
		case s3client.IsPreconditionFailed(err) || s3client.IsUnavailable(err):
			// Created concurrently, or our write's outcome is unknown: the
			// stored key wins.
			continue
		default:
			return nil, b.mapErr(ctx, "create restic key", err)
		}
	}
	return nil, fmt.Errorf("%w: cannot create the key %s", storage.ErrBackingUnavailable, obj)
}

// checkAuxUnused fails when the aux repository name, whose key object obj
// is missing, has a recovery record or any object in its repository.
func (b *Backend) checkAuxUnused(ctx context.Context, name, obj string) error {
	rec, err := b.recovery.Load(ctx, auxKeysDir+name)
	if err != nil {
		return err
	}
	used := rec.Repository != ""
	if !used {
		out, err := b.client().ListObjectsV2(ctx, b.opts.Bucket, s3client.ListInput{Prefix: b.keys.prefix + resticAuxDir + name + "/", MaxKeys: 1})
		if err != nil {
			return b.mapErr(ctx, "list restic aux repository", err)
		}
		used = len(out.Objects) > 0
	}
	if used {
		return fmt.Errorf("%w: the key %s of restic aux repository %q is missing; restore it from backup",
			storage.ErrEncryptionUnavailable, obj, name)
	}
	return nil
}

// --- offline access (the `gos3 restic` CLI) ---

// ResticRecord is a restic recovery record of an s3 backing (see
// ListResticRecords).
type ResticRecord struct {
	restic.Record
	// BucketID is the ID of the bucket whose archive the record describes
	// ("" for an aux repository).
	BucketID string
	// Aux is the name of the aux repository the record describes ("" for
	// a bucket's).
	Aux string
	// Object is the record's key and KeyObject the key of the wrapped key
	// the repository's password derives from, both with the prefix.
	Object, KeyObject string
}

// ListResticRecords reads the restic recovery records of the s3 backing
// (a home or a container) at bucket/prefix: keys/<bucket-id>.restic.json
// and keys/aux/<name>.restic.json, in key order. It is meant for offline
// tools (`gos3 restic env` with the server stopped), which find a bucket's
// record by its name (restic.Record.Bucket) and read its password with
// ReadResticKey. Remote failures wrap storage.ErrBackingUnavailable; a
// damaged record wraps storage.ErrEncryptionUnavailable.
func ListResticRecords(ctx context.Context, c *s3client.Client, bucket, prefix string) ([]ResticRecord, error) {
	dir := prefix + keysDir
	var out []ResticRecord
	err := c.Walk(ctx, bucket, dir, "", func(o s3client.ObjectAttrs) error {
		rel, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, dir), ".restic.json")
		if !ok {
			return nil
		}
		rec := ResticRecord{Object: o.Key, KeyObject: dir + rel + ".key"}
		if name, aux := strings.CutPrefix(rel, auxKeysDir); aux {
			if !validAuxName(name) {
				return nil
			}
			rec.Aux = name
		} else if rel == "" || strings.Contains(rel, "/") {
			return nil
		} else {
			rec.BucketID = rel
		}
		out = append(out, rec)
		return nil
	})
	if err != nil {
		return nil, remoteErr("list restic recovery records", err)
	}
	r := remote{c: c, bucket: bucket}
	recs := out[:0]
	for _, rec := range out {
		data, _, err := r.getBytes(ctx, rec.Object, maxKeyObject)
		switch {
		case s3client.IsNotFound(err):
			continue // deleted since it was listed
		case err != nil:
			return nil, err
		}
		if err := json.Unmarshal(data, &rec.Record); err != nil {
			return nil, fmt.Errorf("%w: restic recovery record %s is damaged: %v", storage.ErrEncryptionUnavailable, rec.Object, err)
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// ReadResticKey reads the key object of rec (from ListResticRecords on
// bucket) and unwraps it with kek for the backing backingID (the marker's,
// see ReadMarker): the password of rec's repository is
// restic.PasswordFromKey of the result.
func ReadResticKey(ctx context.Context, c *s3client.Client, bucket string, kek []byte, backingID string, rec ResticRecord) ([]byte, error) {
	data, _, err := remote{c: c, bucket: bucket}.getBytes(ctx, rec.KeyObject, maxKeyObject)
	if err != nil {
		return nil, err
	}
	if rec.Aux != "" {
		return UnwrapAuxKey(data, kek, backingID, rec.Aux)
	}
	return UnwrapBucketKey(data, kek, backingID, rec.BucketID)
}
