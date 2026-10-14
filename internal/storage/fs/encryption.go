package fs

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/minio/sio"
	"go.etcd.io/bbolt"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// keyMu serializes key creation with the marker in the bucket configuration.
// The marker makes a missing key a hard error, including on subsequent writes.
const encryptionKeyMarker = "internal-encryption-key"

func (b *Backend) bucketKey(bucketID string, create bool) ([]byte, error) {
	b.keyMu.Lock()
	defer b.keyMu.Unlock()
	if key := b.keys[bucketID]; key != nil {
		return key, nil
	}
	dir := filepath.Join(b.sysDir, "keys")
	path := filepath.Join(dir, bucketID)
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		var used bool
		if e := b.view(func(tx *bbolt.Tx) error {
			bb := tx.Bucket(bucketDataName(bucketID))
			if bb == nil {
				return storage.ErrBucketNotFound
			}
			used = bb.Bucket(subConfig).Get([]byte(encryptionKeyMarker)) != nil
			return nil
		}); e != nil {
			return nil, e
		}
		if used {
			return nil, fmt.Errorf("%w: encryption key is missing; restore the key from backup", storage.ErrEncryptionUnavailable)
		}
		if err = os.MkdirAll(dir, dirPerm); err != nil {
			return nil, err
		}
		if err = config.RestrictDir(dir); err != nil {
			return nil, err
		}
		key = make([]byte, 32)
		_, _ = rand.Read(key)
		if err = config.WriteFileAtomic(path, key, filePerm); err != nil {
			return nil, err
		}

	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: encryption key is missing: %w", storage.ErrEncryptionUnavailable, err)
	}
	if err != nil {
		return nil, fmt.Errorf("fs: read encryption key: %w", err)
	}
	if create {
		if err = b.update(func(tx *bbolt.Tx) error {
			bb := tx.Bucket(bucketDataName(bucketID))
			if bb == nil {
				return storage.ErrBucketNotFound
			}
			return bb.Bucket(subConfig).Put([]byte(encryptionKeyMarker), []byte("1"))
		}); err != nil {
			return nil, err
		}
	}
	if len(key) != 32 {
		return nil, errors.New("fs: invalid encryption key length")
	}
	if b.keys == nil {
		b.keys = make(map[string][]byte)
	}
	b.keys[bucketID] = key
	return key, nil
}

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

func (b *Backend) encryptedTemp(bucketID, mode string) (*tmpFile, error) {
	t := b.newTemp()
	if mode == "" {
		return t, nil
	}
	if mode != storage.EncryptionAES256 && mode != storage.EncryptionRestic {
		return nil, errors.ErrUnsupported
	}
	key, err := b.bucketKey(bucketID, true)
	if err != nil {
		return nil, err
	}
	t.cryptoID = randomHex(16)
	t.key, err = objectDataKey(key, t.cryptoID)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Bind every stream to its immutable data identity, preventing ciphertext
// substitution between two objects encrypted under the same bucket master key.
func objectDataKey(master []byte, id string) ([]byte, error) {
	return hkdf.Key(sha256.New, master, nil, "gos3-object-data-v1/"+id, 32)
}

// cryptReader authenticates each block before returning plaintext and supports
// random reads without decrypting an entire object. close also drops read caches.
type cryptReader struct {
	*io.SectionReader
	close func() error
	once  sync.Once
	err   error
}

func (r *cryptReader) Close() error { r.once.Do(func() { r.err = r.close() }); return r.err }

func decryptFile(f *os.File, key []byte, size int64, close func() error) (*cryptReader, error) {
	r, err := sio.DecryptReaderAt(f, storage.EncryptionConfig(key))
	if err != nil {
		return nil, err
	}
	return &cryptReader{SectionReader: io.NewSectionReader(r, 0, size), close: close}, nil
}

func (b *Backend) finishEncryptedData(ctx context.Context, t *tmpFile, bucketID, mode, bucket, key string) (string, string, error) {
	if mode == storage.EncryptionRestic {
		return b.finishRestic(ctx, t, bucketID, bucket, key)
	}
	return b.finishData(t, bucketID)
}

func resticPassword(key []byte) string { return hex.EncodeToString(key) }
