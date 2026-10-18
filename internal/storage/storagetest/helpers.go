package storagetest

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// env is the per-subtest environment: a fresh backend plus helpers that
// report failures precisely through t.
type env struct {
	t     *testing.T
	b     storage.Backend
	ctx   context.Context
	suite *suite
}

// newEnv creates a fresh backend from the suite's factory and closes it
// when t finishes.
func newEnv(t *testing.T, s *suite) *env {
	t.Helper()
	b := s.factory(t)
	if b == nil {
		t.Fatal("storagetest: Factory returned a nil backend")
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return &env{t: t, b: b, ctx: t.Context(), suite: s}
}

// run runs fn as a subtest sharing e's backend.
func (e *env) run(name string, fn func(e *env)) {
	e.t.Helper()
	e.t.Run(name, func(t *testing.T) {
		e.suite.skipListed(t)
		fn(&env{t: t, b: e.b, ctx: t.Context(), suite: e.suite})
	})
}

// runFresh runs fn as a subtest with its own fresh backend.
func (e *env) runFresh(name string, fn func(e *env)) {
	e.t.Helper()
	e.t.Run(name, func(t *testing.T) {
		e.suite.skipListed(t)
		fn(newEnv(t, e.suite))
	})
}

// --- error assertions ---

// noErr fails the test immediately when err is non-nil.
func (e *env) noErr(what string, err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("%s: unexpected error: %v", what, err)
	}
}

// isAny reports whether err matches any of targets.
func isAny(err error, targets ...error) bool {
	for _, tg := range targets {
		if errors.Is(err, tg) {
			return true
		}
	}
	return false
}

func errNames(targets []error) string {
	names := make([]string, len(targets))
	for i, tg := range targets {
		names[i] = fmt.Sprintf("%q", tg.Error())
	}
	return strings.Join(names, " or ")
}

// wantErr records a failure unless err matches one of targets.
func (e *env) wantErr(what string, err error, targets ...error) bool {
	e.t.Helper()
	if err == nil {
		e.t.Errorf("%s: got nil error, want errors.Is(err, %s)", what, errNames(targets))
		return false
	}
	if !isAny(err, targets...) {
		e.t.Errorf("%s: got error %q, want errors.Is(err, %s)", what, err, errNames(targets))
		return false
	}
	return true
}

// --- buckets ---

func (e *env) mkBucket(name string) {
	e.t.Helper()
	e.mkBucketOpts(name, storage.CreateBucketOptions{})
}

func (e *env) mkBucketOpts(name string, opts storage.CreateBucketOptions) {
	e.t.Helper()
	err := e.b.CreateBucket(e.ctx, name, opts)
	if opts.ObjectLockEnabled {
		e.skipIfNotSupported(err)
	}
	e.noErr(fmt.Sprintf("CreateBucket(%q)", name), err)
}

// skipIfNotSupported skips the (sub)test when err says the backend cannot
// do what the test needs (storage.ErrNotSupported, e.g. versioning or
// object lock on a gateway backing); other errors are left to the caller.
func (e *env) skipIfNotSupported(err error) {
	e.t.Helper()
	if errors.Is(err, storage.ErrNotSupported) {
		e.t.Skipf("backend does not support this: %v", err)
	}
}

// mkVersioned creates a bucket and sets its versioning status (no-op for
// VersioningUnversioned).
func (e *env) mkVersioned(name string, status storage.VersioningStatus) {
	e.t.Helper()
	e.mkBucket(name)
	if status != storage.VersioningUnversioned {
		e.setVersioning(name, status)
	}
}

func (e *env) setVersioning(name string, status storage.VersioningStatus) {
	e.t.Helper()
	err := e.b.SetBucketVersioning(e.ctx, name, status)
	e.skipIfNotSupported(err)
	e.noErr(fmt.Sprintf("SetBucketVersioning(%q, %q)", name, status), err)
}

// --- keys ---

// key returns fixture key k as the suite uses it. With
// Options.NoControlKeys every control character (below 0x20, and DEL)
// becomes '!' (0x21), the lowest printable character after the space, so
// a fixture key keeps its byte-wise order relative to the printable keys
// around it (fixtures use control characters only to sort early).
func (e *env) key(k string) string {
	if !e.suite.noControlKeys {
		return k
	}
	b := []byte(k) // byte-wise: control characters are never part of a multi-byte rune
	for i, c := range b {
		if c < 0x20 || c == 0x7f {
			b[i] = '!'
		}
	}
	return string(b)
}

// keys maps key over ks (into a new slice).
func (e *env) keys(ks ...string) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = e.key(k)
	}
	return out
}

// skipIfControlKeys skips a subtest whose purpose is control-character
// keys when Options.NoControlKeys is set.
func (e *env) skipIfControlKeys() {
	e.t.Helper()
	if e.suite.noControlKeys {
		e.t.Skip("NoControlKeys: the backend cannot store keys with control characters")
	}
}

// --- objects ---

// etagOf returns the unquoted MD5 hex of data, as the S3 layer computes it.
func etagOf(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// simpleMeta returns minimal metadata for data.
func simpleMeta(data []byte) storage.ObjectMeta {
	return storage.ObjectMeta{ETag: etagOf(data), ContentType: "application/octet-stream"}
}

// putObject writes and commits one object version. On any failure the
// writer is aborted. It is safe to call from goroutines.
func putObject(ctx context.Context, b storage.Backend, bucket, key string, data []byte, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if len(data) > 0 {
		n, err := w.Write(data)
		if err == nil && n != len(data) {
			err = fmt.Errorf("short write: %d of %d bytes", n, len(data))
		}
		if err != nil {
			_ = w.Abort()
			return storage.ObjectInfo{}, fmt.Errorf("write: %w", err)
		}
	}
	info, err := w.Commit(ctx, meta, opts)
	if err != nil {
		_ = w.Abort()
		return storage.ObjectInfo{}, err
	}
	return info, nil
}

// put stores data under bucket/key with simpleMeta and fails the test on error.
func (e *env) put(bucket, key string, data []byte) storage.ObjectInfo {
	e.t.Helper()
	return e.putMeta(bucket, key, data, simpleMeta(data))
}

func (e *env) putMeta(bucket, key string, data []byte, meta storage.ObjectMeta) storage.ObjectInfo {
	e.t.Helper()
	info, err := putObject(e.ctx, e.b, bucket, key, data, meta, storage.CommitOptions{})
	e.noErr(fmt.Sprintf("put %s/%q", bucket, key), err)
	return info
}

// readObject opens a version and reads its whole content. For delete
// markers data is nil. It verifies the reader/DeleteMarker contract.
func readObject(ctx context.Context, b storage.Backend, bucket, key, versionID string) (storage.ObjectInfo, []byte, error) {
	info, r, err := b.OpenObject(ctx, bucket, key, versionID)
	if err != nil {
		if r != nil {
			_ = r.Close()
			return info, nil, fmt.Errorf("OpenObject returned a non-nil reader together with error %w", err)
		}
		return info, nil, err
	}
	if info.DeleteMarker {
		if r != nil {
			_ = r.Close()
			return info, nil, errors.New("OpenObject returned a non-nil reader for a delete marker")
		}
		return info, nil, nil
	}
	if r == nil {
		return info, nil, errors.New("OpenObject returned a nil reader for an object version")
	}
	data, rerr := io.ReadAll(r)
	cerr := r.Close()
	if rerr != nil {
		return info, nil, fmt.Errorf("reading object: %w", rerr)
	}
	if cerr != nil {
		return info, nil, fmt.Errorf("closing object reader: %w", cerr)
	}
	return info, data, nil
}

// read opens bucket/key@versionID and returns info and content, failing on error.
func (e *env) read(bucket, key, versionID string) (storage.ObjectInfo, []byte) {
	e.t.Helper()
	info, data, err := readObject(e.ctx, e.b, bucket, key, versionID)
	e.noErr(fmt.Sprintf("OpenObject(%s/%q, version %q)", bucket, key, versionID), err)
	return info, data
}

// wantContent reads bucket/key@versionID and checks its content.
func (e *env) wantContent(bucket, key, versionID string, want []byte) storage.ObjectInfo {
	e.t.Helper()
	info, got := e.read(bucket, key, versionID)
	if info.DeleteMarker {
		e.t.Errorf("OpenObject(%s/%q, version %q): got a delete marker, want object content", bucket, key, versionID)
		return info
	}
	if !bytes.Equal(got, want) {
		e.t.Errorf("OpenObject(%s/%q, version %q): content mismatch: %s", bucket, key, versionID, describeBytesDiff(want, got))
	}
	if info.Size != int64(len(want)) {
		e.t.Errorf("OpenObject(%s/%q, version %q): Size = %d, want %d", bucket, key, versionID, info.Size, len(want))
	}
	return info
}

func (e *env) info(bucket, key, versionID string) storage.ObjectInfo {
	e.t.Helper()
	info, err := e.b.GetObjectInfo(e.ctx, bucket, key, versionID)
	e.noErr(fmt.Sprintf("GetObjectInfo(%s/%q, version %q)", bucket, key, versionID), err)
	return info
}

func (e *env) del(bucket, key, versionID string) storage.DeleteResult {
	e.t.Helper()
	res, err := e.b.DeleteObject(e.ctx, bucket, key, versionID)
	e.noErr(fmt.Sprintf("DeleteObject(%s/%q, version %q)", bucket, key, versionID), err)
	return res
}

// describeBytesDiff summarises how two byte slices differ.
func describeBytesDiff(want, got []byte) string {
	n := min(len(want), len(got))
	i := 0
	for i < n && want[i] == got[i] {
		i++
	}
	short := func(b []byte) string {
		if len(b) > 64 {
			return fmt.Sprintf("%q...(%d bytes)", b[:64], len(b))
		}
		return fmt.Sprintf("%q", b)
	}
	if len(want) <= 64 && len(got) <= 64 {
		return fmt.Sprintf("got %s, want %s", short(got), short(want))
	}
	return fmt.Sprintf("got %d bytes (md5 %s), want %d bytes (md5 %s); first difference at offset %d",
		len(got), etagOf(got), len(want), etagOf(want), i)
}

// --- walks ---

func (e *env) walkObjects(bucket, prefix, after string) []storage.ObjectInfo {
	e.t.Helper()
	var out []storage.ObjectInfo
	err := e.b.WalkObjects(e.ctx, bucket, prefix, after, func(o storage.ObjectInfo) error {
		out = append(out, o)
		return nil
	})
	e.noErr(fmt.Sprintf("WalkObjects(%s, prefix %q, after %q)", bucket, prefix, after), err)
	return out
}

func (e *env) walkKeys(bucket, prefix, after string) []string {
	e.t.Helper()
	return keysOf(e.walkObjects(bucket, prefix, after))
}

func keysOf(infos []storage.ObjectInfo) []string {
	out := make([]string, len(infos))
	for i, o := range infos {
		out[i] = o.Key
	}
	return out
}

func (e *env) walkVersions(bucket, prefix, keyMarker, versionIDMarker string) []storage.ObjectInfo {
	e.t.Helper()
	var out []storage.ObjectInfo
	err := e.b.WalkVersions(e.ctx, bucket, prefix, keyMarker, versionIDMarker, func(o storage.ObjectInfo) error {
		out = append(out, o)
		return nil
	})
	e.noErr(fmt.Sprintf("WalkVersions(%s, prefix %q, keyMarker %q, versionIDMarker %q)", bucket, prefix, keyMarker, versionIDMarker), err)
	return out
}

// vref identifies one version (or delete marker) in expectations.
type vref struct {
	Key          string
	VersionID    string
	DeleteMarker bool
	IsLatest     bool
}

func (v vref) String() string {
	s := fmt.Sprintf("%q@%s", v.Key, v.VersionID)
	if v.DeleteMarker {
		s += "(marker)"
	}
	if v.IsLatest {
		s += "(latest)"
	}
	return s
}

func refsOf(infos []storage.ObjectInfo) []vref {
	out := make([]vref, len(infos))
	for i, o := range infos {
		out[i] = vref{Key: o.Key, VersionID: o.VersionID, DeleteMarker: o.DeleteMarker, IsLatest: o.IsLatest}
	}
	return out
}

// history returns the versions of key, newest first.
func (e *env) history(bucket, key string) []vref {
	e.t.Helper()
	var out []vref
	for _, v := range e.walkVersions(bucket, key, "", "") {
		if v.Key == key {
			out = append(out, vref{Key: v.Key, VersionID: v.VersionID, DeleteMarker: v.DeleteMarker, IsLatest: v.IsLatest})
		}
	}
	return out
}

// wantHistory checks the versions of key (newest first). IsLatest is
// expected on the first entry only.
func (e *env) wantHistory(bucket, key string, want ...vref) {
	e.t.Helper()
	for i := range want {
		want[i].Key = key
		want[i].IsLatest = i == 0
	}
	got := e.history(bucket, key)
	if !slices.Equal(got, want) {
		e.t.Errorf("versions of %s/%q (newest first):\n got  %v\n want %v", bucket, key, got, want)
	}
}

// deleteAllVersions permanently removes every version and delete marker
// and aborts every multipart upload in bucket.
func (e *env) deleteAllVersions(bucket string) {
	e.t.Helper()
	for _, v := range e.walkVersions(bucket, "", "", "") {
		e.del(bucket, v.Key, v.VersionID)
	}
	ups, err := e.b.ListMultipartUploads(e.ctx, bucket)
	e.noErr("ListMultipartUploads", err)
	for _, u := range ups {
		e.noErr("AbortMultipartUpload", e.b.AbortMultipartUpload(e.ctx, bucket, u.Key, u.UploadID))
	}
}

// --- multipart ---

func uploadPart(ctx context.Context, b storage.Backend, bucket, key, uploadID string, n int, data []byte, meta storage.PartMeta) (storage.PartInfo, error) {
	w, err := b.NewPartWriter(ctx, bucket, key, uploadID, n)
	if err != nil {
		return storage.PartInfo{}, err
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			_ = w.Abort()
			return storage.PartInfo{}, fmt.Errorf("write: %w", err)
		}
	}
	info, err := w.Commit(ctx, meta)
	if err != nil {
		_ = w.Abort()
		return storage.PartInfo{}, err
	}
	return info, nil
}

func (e *env) part(bucket, key, uploadID string, n int, data []byte) storage.PartInfo {
	e.t.Helper()
	info, err := uploadPart(e.ctx, e.b, bucket, key, uploadID, n, data, storage.PartMeta{ETag: etagOf(data)})
	e.noErr(fmt.Sprintf("upload part %d of %s/%q", n, bucket, key), err)
	return info
}

func (e *env) createUpload(bucket, key string, meta storage.ObjectMeta) storage.UploadInfo {
	e.t.Helper()
	u, err := e.b.CreateMultipartUpload(e.ctx, bucket, key, meta, "")
	e.noErr(fmt.Sprintf("CreateMultipartUpload(%s/%q)", bucket, key), err)
	return u
}

// --- data ---

// randBytes returns n deterministic pseudo-random bytes for seed.
func randBytes(n int, seed uint64) []byte {
	var s [32]byte
	for i := range 8 {
		s[i] = byte(seed >> (8 * i))
	}
	buf := make([]byte, n)
	_, _ = rand.NewChaCha8(s).Read(buf) // never fails
	return buf
}

// --- time ---

// timeSlack tolerates backends that truncate stored times (e.g. to seconds).
const timeSlack = 2 * time.Second

// checkTime verifies a backend-assigned timestamp lies between start and
// now (with timeSlack).
func (e *env) checkTime(what string, got, start time.Time) {
	e.t.Helper()
	if got.IsZero() {
		e.t.Errorf("%s is the zero time, want a time set by the backend", what)
		return
	}
	now := time.Now()
	if got.Before(start.Add(-timeSlack)) || got.After(now.Add(timeSlack)) {
		e.t.Errorf("%s = %s, want between %s and %s", what, got.Format(time.RFC3339Nano),
			start.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}
}

// --- version IDs ---

// maxOpaqueVersionID is the longest version ID the S3 layer accepts from
// clients (and so the longest a backend may pass through).
const maxOpaqueVersionID = 1024

// checkVersionID verifies a generated version ID is valid: non-empty, never
// "null", at most 64 characters and URL-safe (docs/ARCHITECTURE.md). With
// Options.OpaqueVersionIDs it applies the S3 layer's rule instead: at most
// 1024 printable, non-space ASCII characters.
func (e *env) checkVersionID(what, id string) {
	e.t.Helper()
	switch {
	case id == "":
		e.t.Errorf("%s: empty version ID", what)
	case id == storage.NullVersionID:
		e.t.Errorf("%s: version ID is %q, want a generated ID", what, id)
	case e.suite.opaqueVersionIDs:
		if len(id) > maxOpaqueVersionID {
			e.t.Errorf("%s: version ID %.64q... is %d characters, want <= %d", what, id, len(id), maxOpaqueVersionID)
		} else if i := strings.IndexFunc(id, func(r rune) bool { return r <= ' ' || r > '~' }); i >= 0 {
			e.t.Errorf("%s: version ID %q has a character at offset %d that is not printable, non-space ASCII", what, id, i)
		}
	case len(id) > 64:
		e.t.Errorf("%s: version ID %q is %d characters, want <= 64", what, id, len(id))
	case url.QueryEscape(id) != id:
		e.t.Errorf("%s: version ID %q is not URL-safe", what, id)
	}
}

// --- deep comparison ---

var timeType = reflect.TypeFor[time.Time]()

// diffValues appends a description of every difference between want and
// got to out. Times compare with Equal; nil and empty maps/slices are
// equivalent (backends may normalise one into the other).
func diffValues(path string, want, got reflect.Value, out *[]string) {
	if want.Type() == timeType {
		w, g := want.Interface().(time.Time), got.Interface().(time.Time)
		if !w.Equal(g) {
			*out = append(*out, fmt.Sprintf("%s: got %s, want %s", path, fmtTime(g), fmtTime(w)))
		}
		return
	}
	switch want.Kind() {
	case reflect.Struct:
		for i := 0; i < want.NumField(); i++ {
			f := want.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			p := f.Name
			if path != "" {
				p = path + "." + f.Name
			}
			if f.Anonymous {
				p = path // report promoted fields by their own names
			}
			diffValues(p, want.Field(i), got.Field(i), out)
		}
	case reflect.Map:
		if !mapsEqualNorm(want, got) {
			*out = append(*out, fmt.Sprintf("%s: got %v, want %v", path, fmtValue(got), fmtValue(want)))
		}
	case reflect.Slice:
		equal := want.Len() == got.Len()
		for i := 0; equal && i < want.Len(); i++ {
			var sub []string
			diffValues("", want.Index(i), got.Index(i), &sub)
			equal = len(sub) == 0
		}
		if !equal {
			*out = append(*out, fmt.Sprintf("%s: got %v, want %v", path, fmtValue(got), fmtValue(want)))
		}
	default:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			*out = append(*out, fmt.Sprintf("%s: got %#v, want %#v", path, got.Interface(), want.Interface()))
		}
	}
}

func mapsEqualNorm(want, got reflect.Value) bool {
	if want.Len() != got.Len() {
		return false
	}
	it := want.MapRange()
	for it.Next() {
		gv := got.MapIndex(it.Key())
		if !gv.IsValid() {
			return false
		}
		var sub []string
		diffValues("", it.Value(), gv, &sub)
		if len(sub) > 0 {
			return false
		}
	}
	return true
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "zero time"
	}
	return t.Format(time.RFC3339Nano)
}

func fmtValue(v reflect.Value) string {
	if v.Kind() == reflect.Map && v.IsNil() {
		return "nil map"
	}
	if v.Kind() == reflect.Slice && v.IsNil() {
		return "nil slice"
	}
	return fmt.Sprintf("%v", v.Interface())
}

// diffMeta lists the differences between two ObjectMeta values.
func diffMeta(want, got storage.ObjectMeta) []string {
	var out []string
	diffValues("", reflect.ValueOf(want), reflect.ValueOf(got), &out)
	return out
}

// wantMeta reports every metadata field that did not round-trip.
func (e *env) wantMeta(what string, want, got storage.ObjectMeta) {
	e.t.Helper()
	if d := diffMeta(want, got); len(d) > 0 {
		e.t.Errorf("%s: metadata mismatch:\n  %s", what, strings.Join(d, "\n  "))
	}
}

// wantInfo compares two ObjectInfo values field by field (times with Equal).
func (e *env) wantInfo(what string, want, got storage.ObjectInfo) {
	e.t.Helper()
	var out []string
	diffValues("", reflect.ValueOf(want), reflect.ValueOf(got), &out)
	if len(out) > 0 {
		e.t.Errorf("%s: ObjectInfo mismatch:\n  %s", what, strings.Join(out, "\n  "))
	}
}

// fullMeta returns metadata with every field set to a non-zero value, so
// round-trip tests catch any field a backend forgets to persist. RetainUntil
// has nanosecond precision and a non-UTC zone: backends persist metadata
// verbatim, so it must compare Equal after a round trip.
func fullMeta() storage.ObjectMeta {
	return storage.ObjectMeta{
		ETag:               "9e107d9d372bb6826bd81d3542a419d6",
		ContentType:        "text/plain; charset=utf-8",
		ContentEncoding:    "gzip",
		ContentDisposition: `attachment; filename="résumé.txt"`,
		ContentLanguage:    "de-CH",
		CacheControl:       "max-age=60, private",
		Expires:            "Wed, 21 Oct 2037 07:28:00 GMT",
		UserMeta:           map[string]string{"color": "blue", "empty": "", "unicode": "日本語", "with-dash": "a=b; c"},
		Tags:               map[string]string{"project": "gos3", "k 2": "v=2&x"},
		StorageClass:       "REDUCED_REDUNDANCY",
		ACL:                "public-read",
		Checksums:          map[string]string{"CRC32": "NhCmhg==", "SHA256": "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=", "CRC64NVME": "AAAAAAAAAAA="},
		ChecksumType:       "COMPOSITE",
		PartSizes:          []int64{5 << 20, 5 << 20, 1},
		PartChecksums: []map[string]string{
			{"CRC32": "AAAAAA=="},
			{"CRC32": "BBBBBB==", "SHA256": "x"},
			{"CRC32": "CCCCCC=="},
		},
		SSE:             "aws:kms",
		SSEKMSKeyID:     "arn:aws:kms:us-east-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		RetentionMode:   "COMPLIANCE",
		RetainUntil:     time.Date(2031, 2, 3, 4, 5, 6, 123456789, time.FixedZone("UTC+5:30", 5*3600+1800)),
		LegalHold:       true,
		WebsiteRedirect: "/redirected/page.html",
	}
}
