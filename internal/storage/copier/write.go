package copier

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/compress"
)

// MapVersionID returns the version ID a copy of a version with source ID
// id is imported under: id itself when a VersionImporter accepts it
// (storage.ValidImportedVersionID), else "x" and 43 characters of
// unpadded base64url SHA-256 of id. Native buckets pass their remote
// service's IDs through, which need not be valid imported IDs; the mapping
// is deterministic, so copies find their versions again without a record.
func MapVersionID(id string) string {
	if storage.ValidImportedVersionID(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "x" + base64.RawURLEncoding.EncodeToString(sum[:])[:43]
}

// WriteMode says how WriteVersion may store a version.
type WriteMode int

const (
	// WriteImport imports the version (storage.VersionImporter), keeping
	// its ID (MapVersionID), modification time and metadata, or fails with
	// ErrNoImport.
	WriteImport WriteMode = iota
	// WriteImportOrPlain imports the version where the target can (see
	// WriteImport), else writes it plainly (see WritePlain).
	WriteImportOrPlain
	// WritePlain writes the version as a client would
	// (Backend.NewObjectWriter, a delete marker with Backend.DeleteObject):
	// the target gives it its own version ID and modification time; the
	// metadata is the source's.
	WritePlain
)

// Written describes a version WriteVersion stored.
type Written struct {
	// Info is the version as the target stored it (for a plain delete
	// marker: its version ID and delete-marker flag only).
	Info storage.ObjectInfo
	// Imported: the version kept its identity (version ID, modification
	// time); false for a plain write.
	Imported bool
	// Bytes is the number of content bytes written.
	Bytes int64
}

// WriteVersion stores version v (a version or delete marker of another
// bucket, with its metadata) as a version of bucket/key on b, with the
// content read from body (nil for a delete marker), as mode allows and pol
// says:
//
//   - encryption: with EncryptionPreserve as v.Encryption, with
//     EncryptionTarget as pol.TargetEncryption; the target bucket is
//     prepared for it first (storage.EncryptionBackend). A target that
//     cannot store it fails with ErrEncryptionUnsupported unless
//     pol.AllowPlaintext; storing an encrypted version unencrypted fails
//     with ErrPlaintext unless pol.AllowPlaintext (the SSE fields are then
//     cleared);
//   - compression: with CompressionPreserve compressed when v is
//     (storage.CompressingImporter / CompressingBackend), with
//     CompressionTarget as pol.TargetCompression;
//   - object lock: retention and legal hold are kept, which needs a target
//     bucket with object lock (ErrRetention), unless pol.DropRetention.
//
// An import that the target answers with errors.ErrUnsupported becomes a
// plain write when mode is WriteImportOrPlain. Restores use it to write
// versions read from elsewhere than a bucket.
func WriteVersion(ctx context.Context, b storage.Backend, bucket, key string, v storage.ObjectInfo, body io.Reader, pol Policy, mode WriteMode) (Written, error) {
	if err := pol.Validate(); err != nil {
		return Written{}, err
	}
	w := &writer{b: b, bucket: bucket, pol: pol}
	return w.write(ctx, key, v, MapVersionID(v.VersionID), body, mode)
}

// writer writes versions to one target bucket, caching what it learns of
// the bucket for the duration of one sync.
type writer struct {
	b      storage.Backend
	bucket string
	pol    Policy
	info   *storage.BucketInfo
	// prepared caches the outcome of PrepareEncryption per provider.
	prepared map[string]error
}

// bucketInfo returns the target bucket's info (read once).
func (w *writer) bucketInfo(ctx context.Context) (storage.BucketInfo, error) {
	if w.info == nil {
		info, err := w.b.GetBucket(ctx, w.bucket)
		if err != nil {
			return storage.BucketInfo{}, err
		}
		w.info = &info
	}
	return *w.info, nil
}

// checkRetention fails with ErrRetention when meta keeps a retention or
// legal hold the target bucket cannot hold.
func (w *writer) checkRetention(ctx context.Context, meta storage.ObjectMeta) error {
	if !hasLock(meta) {
		return nil
	}
	info, err := w.bucketInfo(ctx)
	if err != nil {
		return err
	}
	if !info.ObjectLockEnabled {
		return fmt.Errorf("%w (bucket %q)", ErrRetention, w.bucket)
	}
	return nil
}

// prepareEncryption readies the target bucket for data encrypted with
// mode, as the S3 layer does before an encrypted write.
func (w *writer) prepareEncryption(ctx context.Context, mode string) error {
	if err, ok := w.prepared[mode]; ok {
		return err
	}
	var err error
	if e, ok := w.b.(storage.EncryptionBackend); ok {
		err = e.PrepareEncryption(ctx, w.bucket, mode)
	} else {
		err = errors.ErrUnsupported
	}
	if err == nil || errors.Is(err, errors.ErrUnsupported) {
		if w.prepared == nil {
			w.prepared = make(map[string]error)
		}
		w.prepared[mode] = err
	}
	return err
}

// decide decides how version v is stored on the target: its metadata and
// write options.
func (w *writer) decide(ctx context.Context, v storage.ObjectInfo) (storage.ObjectMeta, storage.WriteOptions, error) {
	enc := v.Encryption
	if w.pol.Encryption == EncryptionTarget {
		enc = ""
		if w.pol.TargetEncryption != nil {
			enc = w.pol.TargetEncryption(v)
		}
	}
	if enc != "" {
		if err := w.prepareEncryption(ctx, enc); err != nil {
			if !errors.Is(err, errors.ErrUnsupported) {
				return storage.ObjectMeta{}, storage.WriteOptions{}, fmt.Errorf("preparing %s encryption in bucket %q: %w", enc, w.bucket, err)
			}
			if !w.pol.AllowPlaintext {
				return storage.ObjectMeta{}, storage.WriteOptions{}, fmt.Errorf("%w: %s encryption in bucket %q", ErrEncryptionUnsupported, enc, w.bucket)
			}
			enc = ""
		}
	}
	if enc == "" && v.Encryption != "" && !w.pol.AllowPlaintext {
		return storage.ObjectMeta{}, storage.WriteOptions{}, fmt.Errorf("%w (a %s-encrypted version)", ErrPlaintext, v.Encryption)
	}
	meta := w.pol.metaFor(v.ObjectMeta, enc)
	if err := w.checkRetention(ctx, meta); err != nil {
		return storage.ObjectMeta{}, storage.WriteOptions{}, err
	}
	return meta, storage.WriteOptions{Encryption: enc, Compression: w.pol.compressionFor(v)}, nil
}

// metaFor returns the metadata a copy stored with encryption enc gets
// from source metadata src.
func (p Policy) metaFor(src storage.ObjectMeta, enc string) storage.ObjectMeta {
	m := CloneMeta(src)
	m.Encryption = enc
	if enc == "" && src.Encryption != "" {
		m.SSE, m.SSEKMSKeyID = "", ""
	}
	if p.DropRetention {
		m.RetentionMode, m.RetainUntil, m.LegalHold = "", time.Time{}, false
	}
	m.MultipartRetentionSource = ""
	return m
}

// compressionFor returns how a copy of v is to be compressed.
func (p Policy) compressionFor(v storage.ObjectInfo) storage.Compression {
	if p.Compression == CompressionTarget {
		if p.TargetCompression != nil {
			return p.TargetCompression(v)
		}
		return storage.Compression{}
	}
	if v.Compression == storage.CompressionDeflate {
		return storage.Compression{Algorithm: storage.CompressionDeflate, Level: compress.DefaultLevel}
	}
	return storage.Compression{}
}

// hasLock reports whether metadata carries a retention or a legal hold.
func hasLock(m storage.ObjectMeta) bool {
	return m.RetentionMode != "" || !m.RetainUntil.IsZero() || m.LegalHold
}

// withOptions reports whether a write needs the write-options variant of
// the target's methods: only then, so that targets without them still
// take plain writes.
func withOptions(o storage.WriteOptions) bool {
	return o.Compression.Enabled() || o.Encryption != ""
}

// canFallBack reports whether an import error lets WriteImportOrPlain
// write plainly: the target does not import, but nothing says it cannot
// store the data as asked.
func canFallBack(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, ErrEncryptionUnsupported)
}

// importWriter starts an imported version with write options wopts.
func (w *writer) importWriter(ctx context.Context, key string, wopts storage.WriteOptions) (storage.VersionWriter, error) {
	if withOptions(wopts) {
		if ci, ok := w.b.(storage.CompressingImporter); ok {
			return ci.NewVersionWriterWith(ctx, w.bucket, key, wopts)
		}
		if wopts.Encryption != "" {
			if _, ok := w.b.(storage.VersionImporter); !ok {
				return nil, ErrNoImport
			}
			return nil, fmt.Errorf("%w: %s encryption of imported versions in bucket %q", ErrEncryptionUnsupported, wopts.Encryption, w.bucket)
		}
		// Compression is optional: import uncompressed.
	}
	imp, ok := w.b.(storage.VersionImporter)
	if !ok {
		return nil, ErrNoImport
	}
	return imp.NewVersionWriter(ctx, w.bucket, key)
}

// plainWriter starts a plain write with write options wopts.
func (w *writer) plainWriter(ctx context.Context, key string, wopts storage.WriteOptions) (storage.ObjectWriter, error) {
	if !withOptions(wopts) {
		return w.b.NewObjectWriter(ctx, w.bucket, key)
	}
	if cb, ok := w.b.(storage.CompressingBackend); ok {
		ow, err := cb.NewObjectWriterWith(ctx, w.bucket, key, wopts)
		if err != nil && wopts.Encryption != "" && errors.Is(err, errors.ErrUnsupported) {
			err = fmt.Errorf("%w: %s encryption in bucket %q: %v", ErrEncryptionUnsupported, wopts.Encryption, w.bucket, err)
		}
		return ow, err
	}
	if wopts.Encryption != "" {
		return nil, fmt.Errorf("%w: %s encryption in bucket %q", ErrEncryptionUnsupported, wopts.Encryption, w.bucket)
	}
	return w.b.NewObjectWriter(ctx, w.bucket, key)
}

// importErr returns the error of an import that cannot fall back to a
// plain write.
func importErr(err error) error {
	if canFallBack(err) && !errors.Is(err, ErrNoImport) {
		return fmt.Errorf("%w: %w", ErrNoImport, err)
	}
	return err
}

// write stores v as key, imported under version ID id (see WriteVersion).
func (w *writer) write(ctx context.Context, key string, v storage.ObjectInfo, id string, body io.Reader, mode WriteMode) (Written, error) {
	if v.DeleteMarker {
		return w.writeMarker(ctx, key, v, id, mode)
	}
	meta, wopts, err := w.decide(ctx, v)
	if err != nil {
		return Written{}, err
	}
	if body == nil {
		body = eofReader{}
	}
	if mode != WritePlain {
		vw, err := w.importWriter(ctx, key, wopts)
		switch {
		case err == nil:
			n, err := io.Copy(vw, body)
			if err != nil {
				_ = vw.Abort()
				return Written{}, err
			}
			info := storage.ObjectInfo{ObjectMeta: meta, VersionID: id, LastModified: v.LastModified}
			out, err := vw.Commit(ctx, info)
			if err != nil {
				return Written{}, err
			}
			return Written{Info: out, Imported: true, Bytes: n}, nil
		case mode == WriteImport || !canFallBack(err):
			return Written{}, importErr(err)
		}
	}
	ow, err := w.plainWriter(ctx, key, wopts)
	if err != nil {
		return Written{}, err
	}
	n, err := io.Copy(ow, body)
	if err != nil {
		_ = ow.Abort()
		return Written{}, err
	}
	out, err := ow.Commit(ctx, meta, storage.CommitOptions{})
	if err != nil {
		return Written{}, err
	}
	return Written{Info: out, Bytes: n}, nil
}

// writeMarker stores delete marker v as key (see write).
func (w *writer) writeMarker(ctx context.Context, key string, v storage.ObjectInfo, id string, mode WriteMode) (Written, error) {
	if mode != WritePlain {
		vw, err := w.importWriter(ctx, key, storage.WriteOptions{})
		switch {
		case err == nil:
			out, err := vw.Commit(ctx, storage.ObjectInfo{VersionID: id, DeleteMarker: true, LastModified: v.LastModified})
			if err != nil {
				return Written{}, err
			}
			return Written{Info: out, Imported: true}, nil
		case mode == WriteImport || !canFallBack(err):
			return Written{}, importErr(err)
		}
	}
	res, err := w.b.DeleteObject(ctx, w.bucket, key, "")
	if err != nil {
		return Written{}, err
	}
	return Written{Info: storage.ObjectInfo{Bucket: w.bucket, Key: key, VersionID: res.VersionID, DeleteMarker: res.DeleteMarker, IsLatest: true}}, nil
}

// eofReader is an empty body.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
