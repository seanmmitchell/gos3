package copier

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// newBucket creates bucket name on b with the given versioning status and
// object lock.
func newBucket(t *testing.T, b storage.Backend, name string, versioning storage.VersioningStatus, lock bool) {
	t.Helper()
	ctx := t.Context()
	if err := b.CreateBucket(ctx, name, storage.CreateBucketOptions{ObjectLockEnabled: lock}); err != nil {
		t.Fatalf("CreateBucket %s: %v", name, err)
	}
	if versioning != storage.VersioningUnversioned && !lock {
		if err := b.SetBucketVersioning(ctx, name, versioning); err != nil {
			t.Fatalf("SetBucketVersioning %s: %v", name, err)
		}
	}
}

// memEndpoint returns an endpoint on a new memory backend with a bucket of
// the given versioning status.
func memEndpoint(t *testing.T, bucket, prefix string, versioning storage.VersioningStatus) Endpoint {
	t.Helper()
	b := memory.New()
	newBucket(t, b, bucket, versioning, false)
	return Endpoint{Backend: b, Bucket: bucket, Prefix: prefix}
}

func etagOf(body string) string {
	sum := md5.Sum([]byte(body))
	return hex.EncodeToString(sum[:])
}

// put writes body as a new version of key with meta (ETag computed),
// stored with wopts.
func put(t *testing.T, e Endpoint, key, body string, meta storage.ObjectMeta, wopts ...storage.WriteOptions) storage.ObjectInfo {
	t.Helper()
	ctx := t.Context()
	var (
		w   storage.ObjectWriter
		err error
	)
	if len(wopts) > 0 {
		w, err = e.Backend.(storage.CompressingBackend).NewObjectWriterWith(ctx, e.Bucket, key, wopts[0])
	} else {
		w, err = e.Backend.NewObjectWriter(ctx, e.Bucket, key)
	}
	if err != nil {
		t.Fatalf("writer for %s: %v", key, err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("writing %s: %v", key, err)
	}
	if meta.ETag == "" {
		meta.ETag = etagOf(body)
	}
	oi, err := w.Commit(ctx, meta, storage.CommitOptions{})
	if err != nil {
		t.Fatalf("commit %s: %v", key, err)
	}
	return oi
}

// putText writes body as key with a content type.
func putText(t *testing.T, e Endpoint, key, body string) storage.ObjectInfo {
	t.Helper()
	return put(t, e, key, body, storage.ObjectMeta{ContentType: "text/plain"})
}

func del(t *testing.T, e Endpoint, key, versionID string) storage.DeleteResult {
	t.Helper()
	r, err := e.Backend.DeleteObject(t.Context(), e.Bucket, key, versionID)
	if err != nil {
		t.Fatalf("delete %s %s: %v", key, versionID, err)
	}
	return r
}

func history(t *testing.T, e Endpoint, key string) []storage.ObjectInfo {
	t.Helper()
	vs, err := KeyVersions(t.Context(), e.Backend, e.Bucket, key)
	if err != nil {
		t.Fatalf("listing %s: %v", key, err)
	}
	return vs
}

func read(t *testing.T, e Endpoint, key, versionID string) string {
	t.Helper()
	_, rc, err := e.Backend.OpenObject(t.Context(), e.Bucket, key, versionID)
	if err != nil {
		t.Fatalf("open %s %s: %v", key, versionID, err)
	}
	if rc == nil {
		t.Fatalf("open %s %s: a delete marker", key, versionID)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s %s: %v", key, versionID, err)
	}
	return string(data)
}

// syncAll copies src to dst in identity mode, every key once.
func syncAll(t *testing.T, src, dst Endpoint, pol Policy) Result {
	t.Helper()
	var total Result
	err := ZipWalk(t.Context(), src, dst, "", pol.all(), func(ps []KeyPair) error {
		for _, p := range ps {
			r, err := SyncKey(t.Context(), src, dst, p, pol)
			if err != nil {
				return err
			}
			total.Add(r)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return total
}

// syncAllErr is syncAll returning the first error.
func syncAllErr(t *testing.T, src, dst Endpoint, pol Policy) (Result, error) {
	t.Helper()
	var total Result
	err := ZipWalk(t.Context(), src, dst, "", pol.all(), func(ps []KeyPair) error {
		for _, p := range ps {
			r, err := SyncKey(t.Context(), src, dst, p, pol)
			total.Add(r)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return total, err
}

// syncAllFP copies src to dst in fingerprint mode, every key once.
func syncAllFP(t *testing.T, src, dst Endpoint, st FingerprintStore, pol Policy) (Result, error) {
	t.Helper()
	var total Result
	err := ZipWalk(t.Context(), src, dst, "", pol.all(), func(ps []KeyPair) error {
		for _, p := range ps {
			r, err := SyncKeyFingerprint(t.Context(), src, dst, p, st, pol)
			total.Add(r)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return total, err
}

// mustFP is syncAllFP failing the test on errors.
func mustFP(t *testing.T, src, dst Endpoint, st FingerprintStore, pol Policy) Result {
	t.Helper()
	r, err := syncAllFP(t, src, dst, st, pol)
	if err != nil {
		t.Fatalf("fingerprint sync: %v", err)
	}
	return r
}

// sameHistory checks that dst's key holds exactly src's versions: IDs,
// modification times, delete markers, ETags, metadata and content.
func sameHistory(t *testing.T, src Endpoint, srcKey string, dst Endpoint, dstKey string) {
	t.Helper()
	sv, dv := history(t, src, srcKey), history(t, dst, dstKey)
	if len(sv) != len(dv) {
		t.Fatalf("%s: %d versions on the source, %d on the target:\n src %v\n dst %v", srcKey, len(sv), len(dv), ids(sv), ids(dv))
	}
	for i := range sv {
		s, d := sv[i], dv[i]
		if !SameVersion(s, d) {
			t.Fatalf("%s version %d: source %+v, target %+v", srcKey, i, s, d)
		}
		if !s.DeleteMarker {
			if !MetaEqual(s.ObjectMeta, d.ObjectMeta) {
				t.Fatalf("%s version %s: metadata %+v, target %+v", srcKey, s.VersionID, s.ObjectMeta, d.ObjectMeta)
			}
			if got, want := read(t, dst, dstKey, d.VersionID), read(t, src, srcKey, s.VersionID); got != want {
				t.Fatalf("%s version %s: content %q, want %q", srcKey, s.VersionID, got, want)
			}
		}
	}
}

func ids(vs []storage.ObjectInfo) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.VersionID
		if v.DeleteMarker {
			out[i] += "(dm)"
		}
	}
	return out
}

func noChange(t *testing.T, what string, r Result) {
	t.Helper()
	if r.Changed() || r.PlainWrites != 0 || len(r.Conflicts) != 0 || r.Deferred {
		t.Fatalf("%s: want nothing done, got %+v", what, r)
	}
}

// view wraps a backend and rewrites what it reports: listing ETags (as a
// native bucket may report an object a foreign tool uploaded in parts),
// version IDs (as a remote service's IDs that are not valid imported IDs)
// and the encryption of every version.
type view struct {
	storage.Backend
	listETag   func(storage.ObjectInfo) string
	idOut      func(string) string
	idIn       func(string) string
	encryption string
	// beforeOpen runs before each OpenObject, afterOpen once the version
	// is open (its reader still returns that version's content).
	beforeOpen func(key, versionID string)
	afterOpen  func(key, versionID string)
}

func (v *view) out(oi storage.ObjectInfo, listing bool) storage.ObjectInfo {
	if v.idOut != nil {
		oi.VersionID = v.idOut(oi.VersionID)
	}
	if listing && v.listETag != nil {
		oi.ETag = v.listETag(oi)
	}
	if v.encryption != "" && !oi.DeleteMarker {
		oi.Encryption = v.encryption
	}
	return oi
}

func (v *view) in(id string) string {
	if v.idIn != nil && id != "" {
		return v.idIn(id)
	}
	return id
}

func (v *view) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	return v.Backend.WalkObjects(ctx, bucket, prefix, after, func(oi storage.ObjectInfo) error { return fn(v.out(oi, true)) })
}

func (v *view) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	return v.Backend.WalkVersions(ctx, bucket, prefix, keyMarker, v.in(versionIDMarker), func(oi storage.ObjectInfo) error { return fn(v.out(oi, true)) })
}

func (v *view) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	oi, err := v.Backend.GetObjectInfo(ctx, bucket, key, v.in(versionID))
	return v.out(oi, false), err
}

func (v *view) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if v.beforeOpen != nil {
		v.beforeOpen(key, versionID)
	}
	oi, rc, err := v.Backend.OpenObject(ctx, bucket, key, v.in(versionID))
	if err == nil && v.afterOpen != nil {
		v.afterOpen(key, versionID)
	}
	return v.out(oi, false), rc, err
}

// nativeTarget is a memory backend that, as a target, behaves like a
// native bucket of an s3 backing: imports answer errors.ErrUnsupported, it
// reports no Import capability and cannot encrypt. Plain writes, write
// options without encryption and conditional deletes work.
type nativeTarget struct{ *memory.Backend }

func (nativeTarget) NewVersionWriter(context.Context, string, string) (storage.VersionWriter, error) {
	return nil, errors.ErrUnsupported
}

func (nativeTarget) NewVersionWriterWith(context.Context, string, string, storage.WriteOptions) (storage.VersionWriter, error) {
	return nil, errors.ErrUnsupported
}

func (nativeTarget) PrepareEncryption(context.Context, string, string) error {
	return errors.ErrUnsupported
}

func (n nativeTarget) NewObjectWriterWith(ctx context.Context, bucket, key string, o storage.WriteOptions) (storage.ObjectWriter, error) {
	if o.Encryption != "" {
		return nil, errors.ErrUnsupported
	}
	return n.Backend.NewObjectWriterWith(ctx, bucket, key, o)
}

func (nativeTarget) BucketCaps(context.Context, string) (storage.BucketCaps, error) {
	return storage.BucketCaps{Kind: storage.BucketKindNative, Clone: true, Versioning: true, ObjectLock: true}, nil
}

func (nativeTarget) NewBucketCaps(context.Context, storage.CreateBucketOptions) (storage.BucketCaps, error) {
	return storage.BucketCaps{Kind: storage.BucketKindNative, Clone: true, Versioning: true, ObjectLock: true}, nil
}

// nativeEndpoint returns an endpoint on a new nativeTarget.
func nativeEndpoint(t *testing.T, bucket, prefix string, versioning storage.VersioningStatus) Endpoint {
	t.Helper()
	b := nativeTarget{memory.New()}
	newBucket(t, b, bucket, versioning, false)
	return Endpoint{Backend: b, Bucket: bucket, Prefix: prefix}
}

// bareTarget hides every optional interface of a backend except
// conditional deletes: it does not even claim to import.
type bareTarget struct {
	storage.Backend
	storage.ConditionalDeleter
}

func hasConflict(r Result, substr string) bool {
	for _, c := range r.Conflicts {
		if strings.Contains(c.Reason, substr) {
			return true
		}
	}
	return false
}
