package s3store

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

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Restic-archived versions (storage.EncryptionRestic) are run by package
// restic. On an s3 backing:
//
//   - the repository of a bucket is restic's s3 backend at
//     <Bucket>/<Prefix>restic/<bucket-id> on the backing's service, reached
//     with the backing's current credentials; its password is the hex of
//     the bucket's master key (restic.PasswordFromKey);
//   - the recovery record keys/<bucket-id>.restic.json holds
//     {initialized, repository, bucket} and never the password, which
//     derives from the wrapped master key next to it;
//   - a version's snapshot ID is kept in the index (blobRef.Restic); empty
//     objects never reach restic;
//   - restic's cache lives in <TempDir>/restic-cache/<backing-id> (or the
//     user cache directory);
//   - aux repositories (restic.Host, see restic_host.go) live at
//     <Bucket>/<Prefix>restic-aux/<name>, keyed by keys/aux/<name>.key.

func (b *Backend) recoveryObject(id string) string {
	return b.keys.prefix + keysDir + id + ".restic.json"
}

// recoveryStore is the backend's restic.RecoveryStore.
type recoveryStore struct {
	b    *Backend
	mu   sync.Mutex
	last map[string][]byte // the record last stored or loaded, by bucket ID
}

// Load implements restic.RecoveryStore.
func (s *recoveryStore) Load(ctx context.Context, id string) (restic.Record, error) {
	b := s.b
	data, _, err := remote{c: b.client(), bucket: b.opts.Bucket}.getBytes(ctx, b.recoveryObject(id), maxKeyObject)
	if s3client.IsNotFound(err) {
		return restic.Record{}, nil
	}
	if err != nil {
		return restic.Record{}, b.recoveryErr(ctx, "read", err)
	}
	var rec restic.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return restic.Record{}, fmt.Errorf("%w: restic recovery record %s is damaged: %v", storage.ErrEncryptionUnavailable, b.recoveryObject(id), err)
	}
	s.remember(id, data)
	return rec, nil
}

// Store implements restic.RecoveryStore; an unchanged record is not
// rewritten. Every field of rec is kept (restic.Record never holds the
// password).
func (s *recoveryStore) Store(ctx context.Context, id string, rec restic.Record) error {
	b := s.b
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	same := bytes.Equal(s.last[id], raw)
	s.mu.Unlock()
	if same {
		return nil
	}
	if _, err := (remote{c: b.client(), bucket: b.opts.Bucket}).put(ctx, b.recoveryObject(id), raw, s3client.PutOptions{}); err != nil {
		return b.recoveryErr(ctx, "store", err)
	}
	s.remember(id, raw)
	return nil
}

// recoveryErr maps a failure to read or store a recovery record: the
// service's failures wrap storage.ErrBackingUnavailable; refusals (such as
// access denied) concern the bucket's archive and wrap
// storage.ErrEncryptionUnavailable.
func (b *Backend) recoveryErr(ctx context.Context, op string, err error) error {
	if errors.Is(err, storage.ErrBackingUnavailable) || s3client.IsUnavailable(err) || ctx.Err() != nil {
		return b.mapErr(ctx, op+" restic recovery record", err)
	}
	return fmt.Errorf("%w: %s restic recovery record: %w", storage.ErrEncryptionUnavailable, op, err)
}

func (s *recoveryStore) remember(id string, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = make(map[string][]byte)
	}
	s.last[id] = raw
}

// resticRepository describes the repository of bucket id with the current
// connection; ok is false when the connection lacks what restic needs
// (credentials).
func (b *Backend) resticRepository(id string) (restic.S3Repository, bool) {
	return b.s3Repository(resticDir + id)
}

// s3Repository describes the repository at rel (below the prefix) with the
// current connection; ok is false when the connection lacks what restic
// needs (credentials).
func (b *Backend) s3Repository(rel string) (restic.S3Repository, bool) {
	c := b.conn.Load()
	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = c.client.Endpoint()
	}
	if c.accessKey == "" || c.secretKey == "" {
		return restic.S3Repository{}, false
	}
	path := strings.Trim(b.keys.prefix, "/")
	if path != "" {
		path += "/"
	}
	return restic.S3Repository{
		Endpoint:  endpoint,
		Bucket:    b.opts.Bucket,
		Path:      path + strings.TrimSuffix(rel, "/"),
		Region:    c.region,
		AccessKey: c.accessKey,
		SecretKey: c.secretKey,
		PathStyle: c.pathStyle,
	}, true
}

// resticAvailable reports whether restic versions can be written now.
func (b *Backend) resticAvailable() bool {
	_, ok := b.resticRepository("x")
	return ok && b.restic.Installed()
}

// resticCacheDir returns restic's cache directory for this backing.
func (b *Backend) resticCacheDir() string {
	base := b.opts.TempDir
	if base == "" {
		if d, err := os.UserCacheDir(); err == nil {
			base = filepath.Join(d, "gos3")
		} else {
			base = os.TempDir()
		}
	}
	return filepath.Join(base, "restic-cache", b.eng.Marker().BackingID)
}

// resticLocation returns how restic reaches the repository of bk (which
// needs the bucket's master key).
func (b *Backend) resticLocation(ctx context.Context, bk *bucket) (restic.Location, error) {
	master, err := b.bucketKey(ctx, bk, false)
	if err != nil {
		return restic.Location{}, err
	}
	repo, ok := b.resticRepository(bk.id)
	if !ok {
		return restic.Location{}, fmt.Errorf("%w: s3 backing %q has no access key for restic", storage.ErrEncryptionUnavailable, b.opts.Name)
	}
	return repo.Location(restic.PasswordFromKey(master), b.resticCacheDir()), nil
}

// prepareRestic makes sure the repository of bk exists (see
// restic.Runtime.PrepareWith; the recovery record names the bucket, so
// offline tools find its repository by its name) and returns its location.
func (b *Backend) prepareRestic(ctx context.Context, bk *bucket) (restic.Location, error) {
	loc, err := b.resticLocation(ctx, bk)
	if err != nil {
		return restic.Location{}, err
	}
	bk.mu.RLock()
	name := bk.ix.Info.Name
	bk.mu.RUnlock()
	return loc, b.restic.PrepareWith(ctx, bk.id, name, loc, &b.recovery)
}

// resticWriter archives one version into the bucket's repository while it
// is written (see restic.Archiver).
type resticWriter struct {
	a    *restic.Archiver
	size int64
}

// newResticWriter creates the bucket's key if needed, prepares its
// repository and returns a writer archiving bucket/key into it.
func (b *Backend) newResticWriter(ctx context.Context, bk *bucket, bucket, key string) (*resticWriter, error) {
	if _, err := b.bucketKey(ctx, bk, true); err != nil {
		return nil, err
	}
	loc, err := b.prepareRestic(ctx, bk)
	if err != nil {
		return nil, err
	}
	return &resticWriter{a: b.restic.NewArchiverWith(ctx, loc, restic.ArchiveOptions{Bucket: bucket, BucketID: bk.id, Key: key})}, nil
}

func (w *resticWriter) Write(p []byte) (int, error) {
	n, err := w.a.Write(p)
	w.size += int64(n)
	return n, err
}
