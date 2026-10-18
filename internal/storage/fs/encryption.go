package fs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
)

// The optional storage interface implemented by PrepareEncryption and
// ValidateEncryptionSettings.
var _ storage.EncryptionBackend = (*Backend)(nil)

// encryptionKeyMarker is the bucket configuration entry recording that the
// bucket has a master key. With it a missing key file is a hard error
// (restore it from backup) instead of being silently regenerated, which
// would make every encrypted object unreadable.
const encryptionKeyMarker = "internal-encryption-key"

// keysDirName is the directory (in the system directory) holding the bucket
// master keys (keys/<bucket-id>) and restic recovery records.
const keysDirName = "keys"

// bucketKeyEntry is the cached master key state of one bucket.
type bucketKeyEntry struct {
	// load serializes loading and creating the key and storing its marker.
	load sync.Mutex
	// key and marked are written with both load and Backend.keyMu held,
	// and read with either.
	key    []byte
	marked bool // encryptionKeyMarker is known to be stored
}

// bucketKey returns the 32-byte master key of the bucket with ID bucketID.
// It is read from keys/<bucket-id> on first use and cached. When create is
// set, a bucket that never had a key gets a new random one, and the
// encryptionKeyMarker is stored; without create a missing key is an error.
//
// A cached key is returned under a read lock only, so key I/O of one bucket
// (file reads, bbolt transactions, fsyncs) never blocks other buckets'
// cached lookups. Failed lookups are not cached: a key file restored from
// backup is picked up without a restart.
func (b *Backend) bucketKey(bucketID string, create bool) ([]byte, error) {
	b.keyMu.RLock()
	e := b.keys[bucketID]
	if e != nil && e.key != nil && (e.marked || !create) {
		key := e.key
		b.keyMu.RUnlock()
		return key, nil
	}
	b.keyMu.RUnlock()
	if e == nil {
		b.keyMu.Lock()
		if e = b.keys[bucketID]; e == nil {
			if b.keys == nil {
				b.keys = make(map[string]*bucketKeyEntry)
			}
			e = &bucketKeyEntry{}
			b.keys[bucketID] = e
		}
		b.keyMu.Unlock()
	}
	e.load.Lock()
	defer e.load.Unlock()
	key := e.key
	if key == nil {
		var err error
		if key, err = b.loadBucketKey(bucketID, create); err != nil {
			return nil, err
		}
		b.keyMu.Lock()
		e.key = key
		b.keyMu.Unlock()
	}
	if create && !e.marked {
		if err := b.update(func(tx *bbolt.Tx) error {
			bb := tx.Bucket(bucketDataName(bucketID))
			if bb == nil {
				return storage.ErrBucketNotFound
			}
			return bb.Bucket(subConfig).Put([]byte(encryptionKeyMarker), []byte("1"))
		}); err != nil {
			return nil, err
		}
		b.keyMu.Lock()
		e.marked = true
		b.keyMu.Unlock()
	}
	return key, nil
}

// loadBucketKey reads the master key of a bucket from its key file or, when
// create is set and the bucket has no encryptionKeyMarker, creates it. The
// key directory is made private before a new key is written, and failing
// that fails the creation (fail closed, unlike the best-effort protectKeys).
func (b *Backend) loadBucketKey(bucketID string, create bool) ([]byte, error) {
	dir := filepath.Join(b.sysDir, keysDirName)
	path := filepath.Join(dir, bucketID)
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		// A key restored from a backup may have lost its private mode.
		b.tightenPerm(path)
	case errors.Is(err, os.ErrNotExist) && create:
		var used bool
		if err := b.view(func(tx *bbolt.Tx) error {
			bb := tx.Bucket(bucketDataName(bucketID))
			if bb == nil {
				return storage.ErrBucketNotFound
			}
			used = bb.Bucket(subConfig).Get([]byte(encryptionKeyMarker)) != nil
			return nil
		}); err != nil {
			return nil, err
		}
		if used {
			return nil, fmt.Errorf("%w: encryption key %s is missing; restore it from backup", storage.ErrEncryptionUnavailable, path)
		}
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("fs: create key directory: %w", err)
		}
		if err := config.RestrictDir(dir); err != nil {
			return nil, fmt.Errorf("fs: protect key directory: %w", err)
		}
		key = make([]byte, crypt.KeySize)
		_, _ = rand.Read(key)
		if err := config.WriteFileAtomic(path, key, filePerm); err != nil {
			return nil, fmt.Errorf("fs: write encryption key: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		// A per-bucket condition, not a failure of the backend. The file
		// error is not wrapped: a lost key must never read as "nothing
		// created yet" (fs.ErrNotExist) to callers such as the restic
		// recovery information.
		return nil, fmt.Errorf("%w: encryption key %s is missing: %v", storage.ErrEncryptionUnavailable, path, err)
	default:
		return nil, fmt.Errorf("fs: read encryption key: %w", err)
	}
	if len(key) != crypt.KeySize {
		return nil, fmt.Errorf("fs: encryption key %s has %d bytes, want %d", path, len(key), crypt.KeySize)
	}
	return key, nil
}

// protectKeys restricts access to the key directory and the secrets in it
// (bucket master keys, restic recovery records) at Open, e.g. after a
// restore from a backup that did not keep their permissions: on Windows it
// gives the directory a private ACL that its files inherit, elsewhere it
// removes group and other permission bits. This extra pass is best effort;
// on Windows, Open already requires a protected ACL on the parent .gos3.
// Only this small directory is visited, not the whole system directory.
func (b *Backend) protectKeys() {
	dir := filepath.Join(b.sysDir, keysDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.log.Warn("fs: cannot inspect the encryption key directory", "dir", dir, "err", err)
		}
		return
	}
	if err := config.RestrictDir(dir); err != nil {
		b.log.Warn("fs: could not restrict access to the encryption key directory to this user; "+
			"make sure other local users cannot read it", "dir", dir, "err", err)
	}
	b.tightenPerm(dir)
	for _, e := range entries {
		b.tightenPerm(filepath.Join(dir, e.Name()))
		if e.IsDir() && e.Name() == auxKeysDir {
			// Keys and records of aux repositories (restic_host.go).
			if sub, err := os.ReadDir(filepath.Join(dir, e.Name())); err == nil {
				for _, s := range sub {
					b.tightenPerm(filepath.Join(dir, e.Name(), s.Name()))
				}
			}
		}
	}
}

// encryptionBucketID returns the ID of bucket, the identity its keys and
// repository belong to.
func (b *Backend) encryptionBucketID(ctx context.Context, bucket string) (string, error) {
	if err := b.begin(ctx); err != nil {
		return "", err
	}
	var id string
	err := b.view(func(tx *bbolt.Tx) error {
		br, _, err := loadBucket(tx, bucket)
		if err == nil {
			id = br.ID
		}
		return err
	})
	return id, err
}

// PrepareEncryption implements storage.EncryptionBackend: it creates the
// bucket's master key if needed and, for restic, prepares its repository.
// Modes other than storage.EncryptionAES256 and storage.EncryptionRestic
// return errors.ErrUnsupported.
func (b *Backend) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	if mode != storage.EncryptionAES256 && mode != storage.EncryptionRestic {
		return errors.ErrUnsupported
	}
	id, err := b.encryptionBucketID(ctx, bucket)
	if err != nil {
		return err
	}
	if _, err = b.bucketKey(id, true); err != nil {
		return err
	}
	if mode == storage.EncryptionRestic {
		return b.prepareRestic(ctx, id)
	}
	return nil
}

// encryptedTemp returns a temporary file for data stored with the
// encryption mode ("" = none). An encrypted file gets a fresh random data
// ID (t.cryptoID, which becomes the data or part file's ID) and the stream
// key derived from it, so no two streams share a key.
func (b *Backend) encryptedTemp(bucketID, mode string) (*tmpFile, error) {
	t := b.newTemp()
	if mode == "" {
		return t, nil
	}
	if mode != storage.EncryptionAES256 && mode != storage.EncryptionRestic {
		return nil, errors.ErrUnsupported
	}
	master, err := b.bucketKey(bucketID, true)
	if err != nil {
		return nil, err
	}
	t.cryptoID = randomHex(16)
	t.key = crypt.ObjectDataKey(master, t.cryptoID)
	return t, nil
}

// cryptReader is the reader over an encrypted file: a crypt.Reader, which
// authenticates every chunk before returning its plaintext and supports
// random reads without decrypting the entire object, plus an idempotent
// Close running the close function given to decryptFile (which closes the
// file and, for temporary caches, removes it).
type cryptReader struct {
	*crypt.Reader
	close func() error
	once  sync.Once
	err   error
}

// Close runs the close function once; later calls return its result.
func (r *cryptReader) Close() error { r.once.Do(func() { r.err = r.close() }); return r.err }

// decryptFile returns a reader over the size plaintext bytes encrypted with
// key in f, which is read on demand. close is run by the reader's Close.
func decryptFile(f io.ReaderAt, key []byte, size int64, close func() error) (*cryptReader, error) {
	r, err := crypt.NewReader(f, key, size)
	if err != nil {
		return nil, err
	}
	return &cryptReader{Reader: r, close: close}, nil
}

// openEncrypted returns a reader over the size bytes (of content, or of its
// compressed form) stored natively encrypted in the data or part file f
// whose data ID is id, after checking the file's length. The reader's Close
// closes f; on error f is left open.
func (b *Backend) openEncrypted(f *os.File, bucketID, id string, size int64) (*cryptReader, error) {
	if !validDataID(id) {
		return nil, fmt.Errorf("%w: invalid encryption identity", errCorrupt)
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if want := storage.EncryptedSize(size); st.Size() != want {
		return nil, fmt.Errorf("%w: encrypted file has %d bytes, expected %d", errCorrupt, st.Size(), want)
	}
	master, err := b.bucketKey(bucketID, false)
	if err != nil {
		return nil, err
	}
	return decryptFile(f, crypt.ObjectDataKey(master, id), size, f.Close)
}
