package s3store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
)

// Bucket master keys.
//
// Every bucket that stores encrypted data has a random 32-byte master key,
// kept wrapped by the backing's key-encryption key (Options.KEK) in
//
//	keys/<bucket-id>.key = {"v":1,"alg":"A256GCM","wrapped":<base64>,"kek":<KEK check>}
//
// with the additional data "gos3-bucket-key-v1/<backing-id>/<bucket-id>",
// so a key object copied to another bucket (or backing) does not unwrap.
// The object is created with If-None-Match: concurrent or retried creations
// end up with the one key that was stored. Once it exists the bucket is
// marked (a journaled keymark record), and from then on a missing key
// object is a hard storage.ErrEncryptionUnavailable error asking for it to
// be restored: a new key would make every encrypted object unreadable.
//
// Object data is encrypted with crypt.ObjectDataKey(master, data ID) and
// restic repositories use restic.PasswordFromKey(master).
//
// The keys of aux restic repositories (restic_host.go) have the same form,
// in keys/aux/<name>.key with the additional data
// "gos3-aux-key-v1/<backing-id>/<name>". Offline tools unwrap key objects
// with UnwrapBucketKey and UnwrapAuxKey.

const (
	keyFileVersion = 1
	keyAlgorithm   = "A256GCM"
	// maxKeyObject bounds the size of a key object read.
	maxKeyObject = 64 << 10
)

// keyFile is the content of keys/<bucket-id>.key.
type keyFile struct {
	V       int    `json:"v"`
	Alg     string `json:"alg"`
	Wrapped []byte `json:"wrapped"`
	KEK     string `json:"kek"`
}

// keyEntry is the cached master key of one bucket (or the key of one aux
// restic repository, see keyEntryOf).
type keyEntry struct {
	load sync.Mutex // serializes loading and creating the key
	key  []byte     // written with load and Backend.keyMu held
}

func (b *Backend) keyObject(bucketID string) string {
	return b.keys.prefix + keysDir + bucketID + ".key"
}

func keyAAD(backingID, bucketID string) []byte {
	return []byte("gos3-bucket-key-v1/" + backingID + "/" + bucketID)
}

// auxKeysDir (below keysDir) holds the keys and recovery records of aux
// restic repositories.
const auxKeysDir = "aux/"

func auxKeyAAD(backingID, name string) []byte {
	return []byte("gos3-aux-key-v1/" + backingID + "/" + name)
}

// keyObjectAAD returns the additional data of the key object
// keys/<rel>.key: an aux repository's below auxKeysDir, a bucket's
// otherwise.
func keyObjectAAD(backingID, rel string) []byte {
	if name, ok := strings.CutPrefix(rel, auxKeysDir); ok {
		return auxKeyAAD(backingID, name)
	}
	return keyAAD(backingID, rel)
}

// UnwrapBucketKey returns the master key of the bucket with ID bucketID of
// the s3 backing (home or container) whose marker names backingID: data is
// the content of its key object <prefix>keys/<bucket-id>.key and kek the
// backing's encryption_key. The password of the bucket's restic repository
// is restic.PasswordFromKey of the result. It is meant for offline tools
// (`gos3 restic env` with the server stopped); errors wrap
// storage.ErrEncryptionUnavailable (and ErrWrongKEK for another KEK).
func UnwrapBucketKey(data, kek []byte, backingID, bucketID string) ([]byte, error) {
	return unwrapKeyFile(data, kek, keyAAD(backingID, bucketID), keysDir+bucketID+".key")
}

// UnwrapAuxKey is UnwrapBucketKey for the key object
// <prefix>keys/aux/<name>.key of the aux restic repository name.
func UnwrapAuxKey(data, kek []byte, backingID, name string) ([]byte, error) {
	return unwrapKeyFile(data, kek, auxKeyAAD(backingID, name), keysDir+auxKeysDir+name+".key")
}

// errNoKEK reports a backing without a key-encryption key.
func (b *Backend) errNoKEK(err error) error {
	return fmt.Errorf("s3 backing %q has no encryption key configured: %w", b.opts.Name, err)
}

// keyEntryOf returns the cache entry of the key with ID id (a bucket ID,
// or auxKeysDir+name for an aux repository's), adding an empty one.
func (b *Backend) keyEntryOf(id string) *keyEntry {
	b.keyMu.Lock()
	defer b.keyMu.Unlock()
	e := b.bucketKeys[id]
	if e == nil {
		if b.bucketKeys == nil {
			b.bucketKeys = make(map[string]*keyEntry)
		}
		e = &keyEntry{}
		b.bucketKeys[id] = e
	}
	return e
}

// bucketKey returns the master key of bk. With create, a bucket without a
// key gets a new one, and the bucket is marked as having one; without
// create, a missing key is an error. Keys are cached by bucket ID once
// loaded; failures are not cached, so a restored key object is picked up.
func (b *Backend) bucketKey(ctx context.Context, bk *bucket, create bool) ([]byte, error) {
	bk.mu.RLock()
	if bk.gone() {
		err := b.goneErr(bk)
		bk.mu.RUnlock()
		return nil, err
	}
	id, marked := bk.id, bk.keyed
	bk.mu.RUnlock()
	kek := b.conn.Load().kek
	if len(kek) == 0 {
		return nil, b.errNoKEK(storage.ErrEncryptionUnavailable)
	}

	b.keyMu.RLock()
	e := b.bucketKeys[id]
	if e != nil && e.key != nil && (marked || !create) {
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
	key := e.key
	if key == nil {
		var err error
		if key, err = b.loadBucketKey(ctx, id, kek, marked, create); err != nil {
			return nil, err
		}
		b.keyMu.Lock()
		e.key = key
		b.keyMu.Unlock()
	}
	if create && !marked {
		if err := b.relock(ctx, bk); err != nil {
			return nil, err
		}
		var done <-chan error
		if !bk.keyed {
			bk.keyed = true
			done = b.appendLocked(bk, &record{Op: opKeyMark, B: bk.ix.Info.Name}, nil)
		}
		bk.mu.Unlock()
		if done != nil {
			if err := await(ctx, done); err != nil {
				return nil, err
			}
		}
	}
	return key, nil
}

// loadBucketKey reads the master key of bucket id or, with create and no
// key object (and no keymark), creates it.
func (b *Backend) loadBucketKey(ctx context.Context, id string, kek []byte, marked, create bool) ([]byte, error) {
	r := remote{c: b.client(), bucket: b.opts.Bucket}
	obj := b.keyObject(id)
	aad := keyAAD(b.eng.Marker().BackingID, id)
	for range 3 {
		data, _, err := r.getBytes(ctx, obj, maxKeyObject)
		switch {
		case err == nil:
			return unwrapKeyFile(data, kek, aad, obj)
		case !s3client.IsNotFound(err):
			return nil, err
		case marked:
			return nil, fmt.Errorf("%w: the encryption key %s of a bucket with encrypted data is missing; restore it from backup",
				storage.ErrEncryptionUnavailable, obj)
		case !create:
			return nil, fmt.Errorf("%w: bucket has no encryption key (%s)", storage.ErrEncryptionUnavailable, obj)
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
			b.log.Info("s3store: created bucket encryption key", "backing", b.opts.Name, "key", obj)
			return key, nil
		case s3client.IsPreconditionFailed(err) || s3client.IsUnavailable(err):
			// Created concurrently, or our write's outcome is unknown: the
			// stored key wins.
			continue
		default:
			return nil, b.mapErr(ctx, "create encryption key", err)
		}
	}
	return nil, fmt.Errorf("%w: cannot create the encryption key %s", storage.ErrBackingUnavailable, obj)
}

// recordKEK makes sure the marker holds the check value of kek before kek
// wraps a new key. A KEK configured by Open was recorded then; one
// committed by PrepareConnection on a backing without a check value is
// recorded here, at its first use. Another recorded value (a different
// KEK configured elsewhere meanwhile) is ErrWrongKEK.
func (b *Backend) recordKEK(ctx context.Context, kek []byte) error {
	m := b.eng.Marker()
	if m.KEKCheck == crypt.KeyCheck(kek) {
		return nil
	}
	m, err := recordKEKCheck(ctx, b.client(), b.opts.Bucket, b.opts.Prefix, m.BackingID, kek)
	switch {
	case errors.Is(err, ErrWrongKEK):
		return fmt.Errorf("%w: %w", storage.ErrEncryptionUnavailable, err)
	case err != nil:
		return b.mapErr(ctx, "record the encryption key check", err)
	}
	b.eng.noteMarker(m)
	return nil
}

// verifyKEK checks kek against the first key object of the backing (a
// bucket's or an aux repository's, if any): a KEK that cannot unwrap it is
// ErrWrongKEK.
func (b *Backend) verifyKEK(ctx context.Context, c *s3client.Client, backingID string, kek []byte) error {
	dir := b.keys.prefix + keysDir
	var obj string
	err := c.Walk(ctx, b.opts.Bucket, dir, "", func(o s3client.ObjectAttrs) error {
		if strings.HasSuffix(o.Key, ".key") {
			obj = o.Key
			return storage.ErrStopWalk
		}
		return nil
	})
	if err != nil && !errors.Is(err, storage.ErrStopWalk) {
		return remoteErr("list keys", err)
	}
	if obj == "" {
		return nil
	}
	data, _, err := remote{c: c, bucket: b.opts.Bucket}.getBytes(ctx, obj, maxKeyObject)
	if err != nil {
		return err
	}
	rel := strings.TrimSuffix(strings.TrimPrefix(obj, dir), ".key")
	if _, err := unwrapKeyFile(data, kek, keyObjectAAD(backingID, rel), obj); err != nil {
		return fmt.Errorf("%w: it cannot open %s: %w", ErrWrongKEK, obj, err)
	}
	return nil
}

// unwrapKeyFile decodes and unwraps a key object.
func unwrapKeyFile(data, kek, aad []byte, obj string) ([]byte, error) {
	var f keyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: encryption key %s is damaged: %v", storage.ErrEncryptionUnavailable, obj, err)
	}
	if f.V != keyFileVersion || f.Alg != keyAlgorithm {
		return nil, fmt.Errorf("%w: encryption key %s has unsupported version %d / algorithm %q", storage.ErrEncryptionUnavailable, obj, f.V, f.Alg)
	}
	if f.KEK != "" && f.KEK != crypt.KeyCheck(kek) {
		return nil, fmt.Errorf("%w: %w: %s is wrapped with another key-encryption key", storage.ErrEncryptionUnavailable, ErrWrongKEK, obj)
	}
	key, err := crypt.Unwrap(kek, f.Wrapped, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: encryption key %s: %w", storage.ErrEncryptionUnavailable, obj, err)
	}
	if len(key) != crypt.KeySize {
		return nil, fmt.Errorf("%w: encryption key %s has %d bytes", storage.ErrEncryptionUnavailable, obj, len(key))
	}
	return key, nil
}
