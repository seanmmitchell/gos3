package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// clockBackend wraps a storage.Backend and reports test-controlled
// LastModified / Initiated times, so that rules can be evaluated against
// precise timestamps while the wrapped backend uses the wall clock.
type clockBackend struct {
	storage.Backend
	mu        sync.Mutex
	times     map[string]time.Time // bucket\x00key\x00versionID -> LastModified
	initiated map[string]time.Time // uploadID -> Initiated

	// now, when set, stamps delete markers created through DeleteObject
	// (i.e. by the worker) with the fake clock.
	now func() time.Time
	// failDelete makes DeleteObject fail for keys with this prefix.
	failDelete string
	// afterWalk, when set, runs once after the first walk or upload
	// listing returns, simulating concurrent clients.
	afterWalk     func()
	afterWalkOnce sync.Once
	// walks counts WalkObjects/WalkVersions calls.
	walks atomic.Int64
	// beforeDelete, when set, runs at the start of every DeleteObject and
	// DeleteObjectIf call (before the conditional check), simulating a
	// client request that commits between the worker's decision and its
	// delete.
	beforeDelete func(bucket, key, versionID string)
}

func (c *clockBackend) hook() {
	if c.afterWalk != nil {
		c.afterWalkOnce.Do(c.afterWalk)
	}
}

func newClockBackend() *clockBackend {
	return &clockBackend{Backend: memory.New(), times: map[string]time.Time{}, initiated: map[string]time.Time{}}
}

func vkey(bucket, key, versionID string) string { return bucket + "\x00" + key + "\x00" + versionID }

func (c *clockBackend) setTime(bucket, key, versionID string, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.times[vkey(bucket, key, versionID)] = t
}

func (c *clockBackend) fix(o storage.ObjectInfo) storage.ObjectInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.times[vkey(o.Bucket, o.Key, o.VersionID)]; ok {
		o.LastModified = t
	}
	return o
}

func (c *clockBackend) GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error) {
	o, err := c.Backend.GetObjectInfo(ctx, bucket, key, versionID)
	if err != nil {
		return o, err
	}
	return c.fix(o), nil
}

func (c *clockBackend) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	c.walks.Add(1)
	err := c.Backend.WalkObjects(ctx, bucket, prefix, after, func(o storage.ObjectInfo) error { return fn(c.fix(o)) })
	c.hook()
	return err
}

func (c *clockBackend) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	c.walks.Add(1)
	err := c.Backend.WalkVersions(ctx, bucket, prefix, km, vm, func(o storage.ObjectInfo) error { return fn(c.fix(o)) })
	c.hook()
	return err
}

func (c *clockBackend) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	us, err := c.Backend.ListMultipartUploads(ctx, bucket)
	c.hook()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range us {
		if t, ok := c.initiated[us[i].UploadID]; ok {
			us[i].Initiated = t
		}
	}
	return us, err
}

func (c *clockBackend) DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error) {
	if c.failDelete != "" && strings.HasPrefix(key, c.failDelete) {
		return storage.DeleteResult{}, errors.New("injected delete failure")
	}
	if c.beforeDelete != nil {
		c.beforeDelete(bucket, key, versionID)
	}
	res, err := c.Backend.DeleteObject(ctx, bucket, key, versionID)
	c.stampMarker(bucket, key, versionID, res, err)
	return res, err
}

// DeleteObjectIf passes the wrapped backend's version info to check with
// the test-controlled LastModified times.
func (c *clockBackend) DeleteObjectIf(ctx context.Context, bucket, key, versionID string, check func(storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if c.failDelete != "" && strings.HasPrefix(key, c.failDelete) {
		return storage.DeleteResult{}, errors.New("injected delete failure")
	}
	if c.beforeDelete != nil {
		c.beforeDelete(bucket, key, versionID)
	}
	cd := c.Backend.(storage.ConditionalDeleter)
	res, err := cd.DeleteObjectIf(ctx, bucket, key, versionID, func(o storage.ObjectInfo) error { return check(c.fix(o)) })
	c.stampMarker(bucket, key, versionID, res, err)
	return res, err
}

// DeleteVersionIf passes the wrapped backend's version info and newer
// versions to check with the test-controlled LastModified times.
func (c *clockBackend) DeleteVersionIf(ctx context.Context, bucket, key, versionID string, limit int, check func(storage.ObjectInfo, []storage.ObjectInfo) error) (storage.DeleteResult, error) {
	if c.failDelete != "" && strings.HasPrefix(key, c.failDelete) {
		return storage.DeleteResult{}, errors.New("injected delete failure")
	}
	if c.beforeDelete != nil {
		c.beforeDelete(bucket, key, versionID)
	}
	nd := c.Backend.(storage.NewerVersionsDeleter)
	return nd.DeleteVersionIf(ctx, bucket, key, versionID, limit, func(o storage.ObjectInfo, newer []storage.ObjectInfo) error {
		fixed := make([]storage.ObjectInfo, len(newer))
		for i := range newer {
			fixed[i] = c.fix(newer[i])
		}
		return check(c.fix(o), fixed)
	})
}

// stampMarker stamps a delete marker created by the worker with the fake
// clock.
func (c *clockBackend) stampMarker(bucket, key, versionID string, res storage.DeleteResult, err error) {
	if err == nil && versionID == "" && res.DeleteMarker && c.now != nil {
		c.setTime(bucket, key, res.VersionID, c.now())
	}
}

// plainBackend hides the optional extensions of the wrapped backend.
type plainBackend struct{ storage.Backend }

// env is a test fixture: a clock backend, a fake clock and a Worker.
type env struct {
	t   *testing.T
	ctx context.Context
	b   *clockBackend
	w   *Worker

	mu  sync.Mutex
	now time.Time
	log bytes.Buffer
}

// base is the reference time objects are created at in most tests.
var base = utc(2024, 1, 10, 12, 0, 0)

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), b: newClockBackend(), now: base}
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &e.log, mu: &e.mu}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	e.b.now = e.clock
	e.w = NewWorker(e.b, Options{Logger: logger, Now: e.clock})
	t.Cleanup(func() { e.w.Stop(); _ = e.b.Close() })
	return e
}

type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) setNow(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = t
}

func (e *env) logs() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.log.String()
}

func (e *env) bucket(name string, versioning storage.VersioningStatus, lock bool) {
	e.t.Helper()
	if err := e.b.CreateBucket(e.ctx, name, storage.CreateBucketOptions{ObjectLockEnabled: lock}); err != nil {
		e.t.Fatal(err)
	}
	if versioning != storage.VersioningUnversioned && !lock {
		if err := e.b.SetBucketVersioning(e.ctx, name, versioning); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) lifecycle(bucket string, rules ...string) {
	e.t.Helper()
	c, err := Parse([]byte(doc(rules...)))
	if err != nil {
		e.t.Fatalf("Parse: %v", err)
	}
	data, err := c.Marshal()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.b.PutBucketConfig(e.ctx, bucket, storage.ConfigLifecycle, data); err != nil {
		e.t.Fatal(err)
	}
}

// put writes key with size bytes and meta, reported as last modified at.
func (e *env) put(bucket, key string, size int, at time.Time, meta storage.ObjectMeta) storage.ObjectInfo {
	e.t.Helper()
	w, err := e.b.NewObjectWriter(e.ctx, bucket, key)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("x"), size)); err != nil {
		e.t.Fatal(err)
	}
	if meta.ETag == "" {
		meta.ETag = fmt.Sprintf("etag-%s-%d", key, at.UnixNano())
	}
	info, err := w.Commit(e.ctx, meta, storage.CommitOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.b.setTime(bucket, key, info.VersionID, at)
	info.LastModified = at
	return info
}

// del deletes key without a version ID (delete marker in versioned
// buckets), reported as created at.
func (e *env) del(bucket, key string, at time.Time) storage.DeleteResult {
	e.t.Helper()
	res, err := e.b.Backend.DeleteObject(e.ctx, bucket, key, "")
	if err != nil {
		e.t.Fatal(err)
	}
	if res.DeleteMarker {
		e.b.setTime(bucket, key, res.VersionID, at)
	}
	return res
}

func (e *env) upload(bucket, key string, at time.Time) storage.UploadInfo {
	e.t.Helper()
	u, err := e.b.CreateMultipartUpload(e.ctx, bucket, key, storage.ObjectMeta{}, "")
	if err != nil {
		e.t.Fatal(err)
	}
	e.b.mu.Lock()
	e.b.initiated[u.UploadID] = at
	e.b.mu.Unlock()
	return u
}

// versions returns "versionID[:dm]" for every version of key, newest first.
func (e *env) versions(bucket, key string) []storage.ObjectInfo {
	e.t.Helper()
	var out []storage.ObjectInfo
	err := e.b.WalkVersions(e.ctx, bucket, key, "", "", func(o storage.ObjectInfo) error {
		if o.Key == key {
			out = append(out, o)
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

// shape describes the versions of a key, newest first: "o" for an object
// version, "d" for a delete marker; "" when the key has no versions.
func (e *env) shape(bucket, key string) string {
	var b strings.Builder
	for _, v := range e.versions(bucket, key) {
		if v.DeleteMarker {
			b.WriteByte('d')
		} else {
			b.WriteByte('o')
		}
	}
	return b.String()
}

func (e *env) run() Stats {
	e.t.Helper()
	st, err := e.w.RunOnce(e.ctx)
	if err != nil {
		e.t.Fatalf("RunOnce: %v", err)
	}
	return st
}

func (e *env) expectStats(got, want Stats) {
	e.t.Helper()
	if got != want {
		e.t.Fatalf("stats = %+v, want %+v\nlogs:\n%s", got, want, e.logs())
	}
}

const (
	fltAll = `<Filter/><Status>Enabled</Status>`
)

func TestWorkerUnversionedExpirationDays(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b", `<ID>days</ID>`+fltAll+`<Expiration><Days>2</Days></Expiration>`)
	e.put("b", "old", 1, base, storage.ObjectMeta{})                   // expires 2024-01-13 00:00
	e.put("b", "new", 1, base.Add(24*time.Hour), storage.ObjectMeta{}) // expires 2024-01-14 00:00

	e.setNow(utc(2024, 1, 12, 23, 59, 59))
	e.expectStats(e.run(), Stats{Buckets: 1})
	if e.shape("b", "old") != "o" {
		t.Fatal("expired too early")
	}

	e.setNow(utc(2024, 1, 13, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, Expired: 1})
	if e.shape("b", "old") != "" || e.shape("b", "new") != "o" {
		t.Fatalf("old=%q new=%q", e.shape("b", "old"), e.shape("b", "new"))
	}
	if !strings.Contains(e.logs(), "lifecycle: object expired") || !strings.Contains(e.logs(), "key=old") ||
		!strings.Contains(e.logs(), "rule=days") {
		t.Fatalf("missing info log:\n%s", e.logs())
	}

	e.setNow(utc(2024, 1, 14, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, Expired: 1})
	e.expectStats(e.run(), Stats{Buckets: 1})
}

func TestWorkerExpirationDate(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b", fltAll+`<Expiration><Date>2024-02-01T00:00:00Z</Date></Expiration>`)
	e.put("b", "a", 1, base, storage.ObjectMeta{})

	e.setNow(utc(2024, 1, 31, 23, 59, 59))
	e.expectStats(e.run(), Stats{Buckets: 1})
	e.setNow(utc(2024, 2, 1, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, Expired: 1})

	// Objects created after the date expire right away on the next pass.
	e.put("b", "late", 1, utc(2024, 3, 1, 0, 0, 0), storage.ObjectMeta{})
	e.setNow(utc(2024, 3, 1, 0, 0, 1))
	e.expectStats(e.run(), Stats{Buckets: 1, Expired: 1})
}

func TestWorkerFilters(t *testing.T) {
	type obj struct {
		key  string
		size int
		tags map[string]string
	}
	objs := []obj{
		{"logs/a", 5, nil},
		{"logs/b", 50, map[string]string{"env": "dev"}},
		{"logs/c", 500, map[string]string{"env": "dev", "team": "x"}},
		{"data/a", 5, map[string]string{"env": "dev", "team": "x"}},
		{"data/b", 0, nil},
	}
	tests := []struct {
		name    string
		rule    string
		expired []string
	}{
		{"legacy prefix", `<Prefix>logs/</Prefix><Status>Enabled</Status>`, []string{"logs/a", "logs/b", "logs/c"}},
		{"filter prefix", `<Filter><Prefix>data/</Prefix></Filter><Status>Enabled</Status>`, []string{"data/a", "data/b"}},
		{"empty prefix", `<Filter><Prefix></Prefix></Filter><Status>Enabled</Status>`, []string{"logs/a", "logs/b", "logs/c", "data/a", "data/b"}},
		{"tag", `<Filter><Tag><Key>env</Key><Value>dev</Value></Tag></Filter><Status>Enabled</Status>`, []string{"logs/b", "logs/c", "data/a"}},
		{"tag value mismatch", `<Filter><Tag><Key>env</Key><Value>prod</Value></Tag></Filter><Status>Enabled</Status>`, nil},
		{"size greater", `<Filter><ObjectSizeGreaterThan>5</ObjectSizeGreaterThan></Filter><Status>Enabled</Status>`, []string{"logs/b", "logs/c"}},
		{"size less", `<Filter><ObjectSizeLessThan>50</ObjectSizeLessThan></Filter><Status>Enabled</Status>`, []string{"logs/a", "data/a", "data/b"}},
		{"and prefix tags", `<Filter><And><Prefix>logs/</Prefix><Tag><Key>env</Key><Value>dev</Value></Tag><Tag><Key>team</Key><Value>x</Value></Tag></And></Filter><Status>Enabled</Status>`, []string{"logs/c"}},
		{"and size range", `<Filter><And><ObjectSizeGreaterThan>4</ObjectSizeGreaterThan><ObjectSizeLessThan>51</ObjectSizeLessThan></And></Filter><Status>Enabled</Status>`, []string{"logs/a", "logs/b", "data/a"}},
		{"and everything", `<Filter><And><Prefix>logs/</Prefix><Tag><Key>env</Key><Value>dev</Value></Tag><ObjectSizeGreaterThan>10</ObjectSizeGreaterThan><ObjectSizeLessThan>100</ObjectSizeLessThan></And></Filter><Status>Enabled</Status>`, []string{"logs/b"}},
		{"disabled", `<Filter/><Status>Disabled</Status>`, nil},
	}
	for _, versioned := range []bool{false, true} {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("%s/versioned=%v", tc.name, versioned), func(t *testing.T) {
				e := newEnv(t)
				vs := storage.VersioningUnversioned
				if versioned {
					vs = storage.VersioningEnabled
				}
				e.bucket("b", vs, false)
				e.lifecycle("b", tc.rule+`<Expiration><Days>1</Days></Expiration>`)
				for _, o := range objs {
					e.put("b", o.key, o.size, base, storage.ObjectMeta{Tags: o.tags})
				}
				e.setNow(base.Add(72 * time.Hour))
				st := e.run()
				e.expectStats(st, Stats{Buckets: 1, Expired: len(tc.expired)})
				want := map[string]bool{}
				for _, k := range tc.expired {
					want[k] = true
				}
				for _, o := range objs {
					got := e.shape("b", o.key)
					exp := "o"
					switch {
					case want[o.key] && versioned:
						exp = "do"
					case want[o.key]:
						exp = ""
					}
					if got != exp {
						t.Errorf("%s: shape %q, want %q", o.key, got, exp)
					}
				}
			})
		}
	}
}

func TestWorkerVersionedExpirationAddsDeleteMarker(t *testing.T) {
	for _, status := range []storage.VersioningStatus{storage.VersioningEnabled, storage.VersioningSuspended} {
		t.Run(string(status), func(t *testing.T) {
			e := newEnv(t)
			e.bucket("b", storage.VersioningEnabled, false)
			e.put("b", "k", 1, base, storage.ObjectMeta{}) // a real version ID
			if status == storage.VersioningSuspended {
				if err := e.b.SetBucketVersioning(e.ctx, "b", status); err != nil {
					t.Fatal(err)
				}
			}
			e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration>`)
			e.setNow(base.Add(48 * time.Hour))
			e.expectStats(e.run(), Stats{Buckets: 1, Expired: 1})
			vs := e.versions("b", "k")
			if len(vs) != 2 || !vs[0].DeleteMarker || vs[1].DeleteMarker {
				t.Fatalf("versions: %+v", vs)
			}
			if status == storage.VersioningSuspended && vs[0].VersionID != storage.NullVersionID {
				t.Fatalf("suspended bucket marker version %q", vs[0].VersionID)
			}
			if status == storage.VersioningEnabled && vs[0].VersionID == storage.NullVersionID {
				t.Fatal("enabled bucket marker must have a real version ID")
			}
			// The new delete marker is not removed in the same pass, and the
			// next pass leaves it alone because a noncurrent version
			// remains.
			e.expectStats(e.run(), Stats{Buckets: 1})
			if e.shape("b", "k") != "do" {
				t.Fatalf("shape %q", e.shape("b", "k"))
			}
		})
	}
}

func TestWorkerSuspendedNullVersionReplaced(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.put("b", "k", 1, base, storage.ObjectMeta{})
	if err := e.b.SetBucketVersioning(e.ctx, "b", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	e.put("b", "k", 2, base.Add(time.Hour), storage.ObjectMeta{}) // null version
	e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration>`)
	e.setNow(base.Add(72 * time.Hour))
	e.expectStats(e.run(), Stats{Buckets: 1, Expired: 1})
	vs := e.versions("b", "k")
	if len(vs) != 2 || !vs[0].DeleteMarker || vs[0].VersionID != storage.NullVersionID || vs[1].VersionID == storage.NullVersionID {
		t.Fatalf("versions: %+v", vs)
	}
}

func TestWorkerExpiredObjectDeleteMarker(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", `<ID>eodm</ID><Filter><Prefix>m/</Prefix></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>`)

	// Lone delete marker: removed.
	e.del("b", "m/lone", base)
	// Delete marker with a noncurrent version: kept.
	e.put("b", "m/kept", 1, base, storage.ObjectMeta{})
	e.del("b", "m/kept", base)
	// Lone delete marker outside the prefix: kept.
	e.del("b", "other", base)
	// Two delete markers only: the latest is not the only version.
	e.del("b", "m/two", base)
	e.del("b", "m/two", base)
	// Live object: untouched.
	e.put("b", "m/live", 1, base, storage.ObjectMeta{})

	e.expectStats(e.run(), Stats{Buckets: 1, DeleteMarkersRemoved: 1})
	for key, want := range map[string]string{"m/lone": "", "m/kept": "do", "other": "d", "m/two": "dd", "m/live": "o"} {
		if got := e.shape("b", key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	if !strings.Contains(e.logs(), "expired object delete marker removed") {
		t.Fatalf("logs:\n%s", e.logs())
	}

	// ExpiredObjectDeleteMarker=false does nothing.
	e2 := newEnv(t)
	e2.bucket("b", storage.VersioningEnabled, false)
	e2.lifecycle("b", fltAll+`<Expiration><ExpiredObjectDeleteMarker>false</ExpiredObjectDeleteMarker></Expiration>`)
	e2.del("b", "x", base)
	e2.expectStats(e2.run(), Stats{Buckets: 1})
	if e2.shape("b", "x") != "d" {
		t.Fatal("marker removed with ExpiredObjectDeleteMarker=false")
	}
}

func TestWorkerDaysRemovesExpiredDeleteMarkers(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", fltAll+`<Expiration><Days>3</Days></Expiration>`)
	e.del("b", "k", base) // lone marker, "expires" 2024-01-14 00:00

	e.setNow(utc(2024, 1, 13, 23, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1})
	e.setNow(utc(2024, 1, 14, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, DeleteMarkersRemoved: 1})
	if e.shape("b", "k") != "" {
		t.Fatal("marker not removed")
	}

	// Rules with tag filters never select delete markers.
	e2 := newEnv(t)
	e2.bucket("b", storage.VersioningEnabled, false)
	e2.lifecycle("b", `<Filter><Tag><Key>a</Key><Value>b</Value></Tag></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`)
	e2.del("b", "k", base)
	e2.setNow(base.AddDate(1, 0, 0))
	e2.expectStats(e2.run(), Stats{Buckets: 1})
}

func TestWorkerNoncurrentExpiration(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", `<ID>nc</ID>`+fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>2</NoncurrentDays></NoncurrentVersionExpiration>`)
	day := 24 * time.Hour
	v1 := e.put("b", "k", 1, base, storage.ObjectMeta{})            // noncurrent since base+1d -> due 2024-01-14
	v2 := e.put("b", "k", 1, base.Add(day), storage.ObjectMeta{})   // noncurrent since base+3d -> due 2024-01-16
	v3 := e.put("b", "k", 1, base.Add(3*day), storage.ObjectMeta{}) // current
	e.put("b", "solo", 1, base, storage.ObjectMeta{})               // current only: never touched

	e.setNow(utc(2024, 1, 13, 23, 59, 0))
	e.expectStats(e.run(), Stats{Buckets: 1})

	e.setNow(utc(2024, 1, 14, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 1})
	vs := e.versions("b", "k")
	if len(vs) != 2 || vs[0].VersionID != v3.VersionID || vs[1].VersionID != v2.VersionID {
		t.Fatalf("after first expiry: %+v (v1 %s)", vs, v1.VersionID)
	}

	e.setNow(utc(2024, 1, 16, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 1})
	if e.shape("b", "k") != "o" || e.shape("b", "solo") != "o" {
		t.Fatalf("shapes %q %q", e.shape("b", "k"), e.shape("b", "solo"))
	}
	if !strings.Contains(e.logs(), "noncurrent version expired") || !strings.Contains(e.logs(), "versionId="+v1.VersionID) {
		t.Fatalf("logs:\n%s", e.logs())
	}
}

func TestWorkerNewerNoncurrentVersions(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>2</NewerNoncurrentVersions></NoncurrentVersionExpiration>`)
	var ids []string
	for i := range 6 {
		ids = append(ids, e.put("b", "k", 1, base.Add(time.Duration(i)*time.Hour), storage.ObjectMeta{}).VersionID)
	}
	e.setNow(base.AddDate(0, 0, 10))
	// 6 versions: 1 current, 5 noncurrent; keep the 2 newest noncurrent.
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 3})
	vs := e.versions("b", "k")
	if len(vs) != 3 || vs[0].VersionID != ids[5] || vs[1].VersionID != ids[4] || vs[2].VersionID != ids[3] {
		t.Fatalf("versions: %+v", vs)
	}
	e.expectStats(e.run(), Stats{Buckets: 1})
}

func TestWorkerNoncurrentDeleteMarkersAndCleanup(t *testing.T) {
	// Noncurrent versions (including noncurrent delete markers) expire;
	// once only the current delete marker remains, ExpiredObjectDeleteMarker
	// removes it in the same pass.
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b",
		`<ID>nc</ID>`+fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`,
		`<ID>eodm</ID>`+fltAll+`<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>`)
	e.put("b", "k", 1, base, storage.ObjectMeta{})
	e.del("b", "k", base.Add(time.Hour))
	e.put("b", "k", 1, base.Add(2*time.Hour), storage.ObjectMeta{})
	e.del("b", "k", base.Add(3*time.Hour))
	if e.shape("b", "k") != "dodo" {
		t.Fatalf("setup shape %q", e.shape("b", "k"))
	}
	e.setNow(base.AddDate(0, 0, 5))
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 3, DeleteMarkersRemoved: 1})
	if e.shape("b", "k") != "" {
		t.Fatalf("shape %q", e.shape("b", "k"))
	}
}

func TestWorkerObjectLockProtection(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, true)
	e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
	retain := base.AddDate(0, 0, 30)
	e.put("b", "retained", 1, base, storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: retain})
	e.put("b", "retained", 1, base.Add(time.Hour), storage.ObjectMeta{})
	e.put("b", "held", 1, base, storage.ObjectMeta{LegalHold: true})
	e.put("b", "held", 1, base.Add(time.Hour), storage.ObjectMeta{})
	e.put("b", "free", 1, base, storage.ObjectMeta{})
	e.put("b", "free", 1, base.Add(time.Hour), storage.ObjectMeta{})

	e.setNow(base.AddDate(0, 0, 10))
	// Current versions get delete markers (allowed under object lock);
	// only the unprotected noncurrent version is removed.
	e.expectStats(e.run(), Stats{Buckets: 1, Expired: 3, NoncurrentExpired: 1})
	for key, want := range map[string]string{"retained": "doo", "held": "doo", "free": "do"} {
		if got := e.shape("b", key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	// Once the retention period ends, the versions can go (except legal
	// hold); delete markers left alone are then removed by the Days rule.
	e.setNow(retain.Add(time.Second))
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 4, DeleteMarkersRemoved: 2})
	for key, want := range map[string]string{"retained": "", "held": "do", "free": ""} {
		if got := e.shape("b", key); got != want {
			t.Errorf("after retention %s: %q, want %q", key, got, want)
		}
	}
	// Releasing the legal hold lets the last version expire.
	vs := e.versions("b", "held")
	if _, err := e.b.UpdateObjectMeta(e.ctx, "b", "held", vs[1].VersionID, func(m *storage.ObjectMeta) error {
		m.LegalHold = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 1, DeleteMarkersRemoved: 1})
	if got := e.shape("b", "held"); got != "" {
		t.Fatalf("held: %q", got)
	}
}

func TestWorkerProtectedUnversionedAndSuspended(t *testing.T) {
	// Defensive: object lock metadata on null versions blocks their
	// permanent removal by Expiration.
	e := newEnv(t)
	e.bucket("u", storage.VersioningUnversioned, false)
	e.lifecycle("u", fltAll+`<Expiration><Days>1</Days></Expiration>`)
	e.put("u", "held", 1, base, storage.ObjectMeta{LegalHold: true})
	e.put("u", "ret", 1, base, storage.ObjectMeta{RetainUntil: base.AddDate(1, 0, 0)})
	e.bucket("s", storage.VersioningEnabled, false)
	e.put("s", "k", 1, base, storage.ObjectMeta{})
	if err := e.b.SetBucketVersioning(e.ctx, "s", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	e.put("s", "k", 1, base, storage.ObjectMeta{LegalHold: true}) // null version
	e.lifecycle("s", fltAll+`<Expiration><Days>1</Days></Expiration>`)

	e.setNow(base.AddDate(0, 0, 5))
	e.expectStats(e.run(), Stats{Buckets: 2})
	if e.shape("u", "held") != "o" || e.shape("u", "ret") != "o" || e.shape("s", "k") != "oo" {
		t.Fatal("protected versions were removed")
	}
}

func TestWorkerAbortIncompleteMultipartUpload(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b",
		`<ID>abort</ID><Filter><Prefix>up/</Prefix></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>2</DaysAfterInitiation></AbortIncompleteMultipartUpload>`,
		`<ID>sz</ID><Filter><ObjectSizeGreaterThan>100</ObjectSizeGreaterThan></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>5</DaysAfterInitiation></AbortIncompleteMultipartUpload>`)
	old := e.upload("b", "up/old", base)                   // due 2024-01-13 by "abort"
	e.upload("b", "up/new", base.AddDate(0, 0, 1))         // due 2024-01-14
	other := e.upload("b", "other", base)                  // due 2024-01-16 by "sz" (size ignored)
	e.put("b", "up/object", 1, base, storage.ObjectMeta{}) // objects are not affected

	e.setNow(utc(2024, 1, 13, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, UploadsAborted: 1})
	if _, err := e.b.GetMultipartUpload(e.ctx, "b", "up/old", old.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("old upload: %v", err)
	}
	e.setNow(utc(2024, 1, 16, 0, 0, 0))
	e.expectStats(e.run(), Stats{Buckets: 1, UploadsAborted: 2})
	if _, err := e.b.GetMultipartUpload(e.ctx, "b", "other", other.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Fatalf("other upload: %v", err)
	}
	if e.shape("b", "up/object") != "o" {
		t.Fatal("object removed by abort rule")
	}
	if !strings.Contains(e.logs(), "incomplete multipart upload aborted") {
		t.Fatalf("logs:\n%s", e.logs())
	}
}

func TestWorkerSkipsBucketsAndCountsErrors(t *testing.T) {
	e := newEnv(t)
	e.bucket("none", storage.VersioningUnversioned, false) // no configuration
	e.put("none", "k", 1, base, storage.ObjectMeta{})
	e.bucket("bad", storage.VersioningUnversioned, false)
	if err := e.b.PutBucketConfig(e.ctx, "bad", storage.ConfigLifecycle, []byte("<garbage")); err != nil {
		t.Fatal(err)
	}
	e.bucket("disabled", storage.VersioningUnversioned, false)
	e.lifecycle("disabled", `<Filter/><Status>Disabled</Status><Expiration><Days>1</Days></Expiration>`)
	e.put("disabled", "k", 1, base, storage.ObjectMeta{})
	e.bucket("fail", storage.VersioningUnversioned, false)
	e.lifecycle("fail", fltAll+`<Expiration><Days>1</Days></Expiration>`)
	e.put("fail", "bad/1", 1, base, storage.ObjectMeta{})
	e.put("fail", "ok", 1, base, storage.ObjectMeta{})
	e.b.failDelete = "bad/"

	e.setNow(base.AddDate(0, 0, 5))
	e.expectStats(e.run(), Stats{Buckets: 2, Expired: 1, Errors: 2})
	if e.shape("none", "k") != "o" || e.shape("disabled", "k") != "o" || e.shape("fail", "bad/1") != "o" || e.shape("fail", "ok") != "" {
		t.Fatal("unexpected bucket contents")
	}
	if !strings.Contains(e.logs(), "level=WARN") {
		t.Fatalf("expected warnings:\n%s", e.logs())
	}
	if s := e.w.Status(); s.LastStats.Errors != 2 || s.Running || s.LastError != "" || !s.Enabled || s.Interval != DefaultInterval {
		t.Fatalf("status %+v", s)
	}
}

func TestWorkerManyKeysBatched(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprintf("versioned=%v", versioned), func(t *testing.T) {
			e := newEnv(t)
			vs := storage.VersioningUnversioned
			if versioned {
				vs = storage.VersioningEnabled
			}
			e.bucket("b", vs, false)
			e.lifecycle("b",
				`<Filter><Prefix>p/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`,
				`<Filter><Prefix>p/x</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`)
			const n = 2*walkBatch + 37
			for i := range n {
				e.put("b", fmt.Sprintf("p/%05d", i), 0, base, storage.ObjectMeta{})
				if versioned && i%7 == 0 {
					// Some keys have several versions so that batches
					// end at varying positions.
					e.put("b", fmt.Sprintf("p/%05d", i), 0, base, storage.ObjectMeta{})
				}
			}
			e.put("b", "q/outside", 0, base, storage.ObjectMeta{})
			e.setNow(base.AddDate(0, 0, 3))
			e.b.walks.Store(0)
			e.expectStats(e.run(), Stats{Buckets: 1, Expired: n})
			// The walk is suspended every walkBatch versions.
			if w := e.b.walks.Load(); w < 3 {
				t.Fatalf("walks = %d, want batched walks", w)
			}
			if e.shape("b", "q/outside") != "o" {
				t.Fatal("key outside the prefix expired")
			}
			// Everything already has a delete marker (or is gone).
			e.expectStats(e.run(), Stats{Buckets: 1})
		})
	}
}

// walkSizeBackend records the largest number of versions a single
// WalkVersions call delivered.
type walkSizeBackend struct {
	*clockBackend
	mu      sync.Mutex
	maxWalk int
}

func (w *walkSizeBackend) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	n := 0
	err := w.clockBackend.WalkVersions(ctx, bucket, prefix, km, vm, func(o storage.ObjectInfo) error {
		n++
		return fn(o)
	})
	w.mu.Lock()
	w.maxWalk = max(w.maxWalk, n)
	w.mu.Unlock()
	return err
}

// TestWorkerManyVersionsOfOneKey: the versions of a key are walked in
// batches like any others, so a pass over a key with many versions does not
// hold all of them (issue #33), and the rules still see every version in
// order across the batches.
func TestWorkerManyVersionsOfOneKey(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", `<ID>nc</ID><Filter><Prefix>k</Prefix></Filter><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>3</NewerNoncurrentVersions></NoncurrentVersionExpiration>`,
		`<ID>m</ID><Filter><Prefix>m</Prefix></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
	const n = 2*walkBatch + 5
	var ids []string
	for i := range n {
		ids = append(ids, e.put("b", "k", 0, base.Add(time.Duration(i)*time.Minute), storage.ObjectMeta{}).VersionID)
	}
	// "m" ends in a lone delete marker after its versions expire: it must
	// be removed although its versions span batches.
	for i := range walkBatch {
		e.put("b", "m", 0, base.Add(time.Duration(i)*time.Minute), storage.ObjectMeta{})
	}
	e.del("b", "m", base.Add(time.Duration(walkBatch)*time.Minute))
	e.put("b", "z", 0, base, storage.ObjectMeta{})
	e.setNow(base.AddDate(0, 0, 10))

	w := &walkSizeBackend{clockBackend: e.b}
	e.w = NewWorker(w, Options{Logger: e.w.log, Now: e.clock})
	t.Cleanup(e.w.Stop)
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: n - 4 + walkBatch, DeleteMarkersRemoved: 1})
	if w.maxWalk > walkBatch+1 {
		t.Errorf("a walk delivered %d versions, want at most %d", w.maxWalk, walkBatch+1)
	}
	vs := e.versions("b", "k")
	if len(vs) != 4 {
		t.Fatalf("k: %d versions left, want 4", len(vs))
	}
	for i, v := range vs {
		if want := ids[n-1-i]; v.VersionID != want {
			t.Errorf("k: version %d is %s, want %s", i, v.VersionID, want)
		}
	}
	if got := e.shape("b", "m"); got != "" {
		t.Errorf("m: %q, want no versions", got)
	}
	if got := e.shape("b", "z"); got != "o" {
		t.Errorf("z: %q, want it kept", got)
	}
	e.expectStats(e.run(), Stats{Buckets: 1})
}

func TestWorkerContextCancel(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration>`)
	e.put("b", "k", 1, base, storage.ObjectMeta{})
	e.setNow(base.AddDate(0, 0, 5))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.w.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if e.shape("b", "k") != "o" {
		t.Fatal("work done after cancel")
	}
	if s := e.w.Status(); s.LastError == "" {
		t.Fatal("status should record the error")
	}
}

func TestWorkerNilBackendAndDefaults(t *testing.T) {
	w := NewWorker(nil, Options{})
	if _, err := w.RunOnce(context.Background()); err == nil {
		t.Fatal("nil backend must fail")
	}
	if w.Status().Interval != DefaultInterval {
		t.Fatal("default interval")
	}
	w.Reconfigure(-1, false)
	if s := w.Status(); s.Interval != DefaultInterval || s.Enabled {
		t.Fatalf("status %+v", s)
	}
	w.Stop() // not started: no-op
}

func TestProtected(t *testing.T) {
	now := base
	cases := []struct {
		o    storage.ObjectInfo
		want bool
	}{
		{storage.ObjectInfo{}, false},
		{storage.ObjectInfo{ObjectMeta: storage.ObjectMeta{LegalHold: true}}, true},
		{storage.ObjectInfo{ObjectMeta: storage.ObjectMeta{RetainUntil: now.Add(time.Second)}}, true},
		{storage.ObjectInfo{ObjectMeta: storage.ObjectMeta{RetainUntil: now}}, false},
		{storage.ObjectInfo{ObjectMeta: storage.ObjectMeta{LegalHold: true}, DeleteMarker: true}, false},
	}
	for i, c := range cases {
		if got := Protected(c.o, now); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

// countingBackend counts ListBuckets calls (one per pass).
type countingBackend struct {
	storage.Backend
	passes atomic.Int64
	block  chan struct{} // when non-nil, ListBuckets waits on it
}

func (c *countingBackend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	c.passes.Add(1)
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.Backend.ListBuckets(ctx)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWorkerSchedule(t *testing.T) {
	cb := &countingBackend{Backend: memory.New()}
	defer cb.Close()
	w := NewWorker(cb, Options{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The first pass runs immediately on Start; the next not before an hour.
	w.Start(ctx)
	w.Start(ctx) // no-op while running
	waitFor(t, "first pass", func() bool { return cb.passes.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := cb.passes.Load(); n != 1 {
		t.Fatalf("passes = %d", n)
	}

	// Shortening the interval takes effect immediately.
	w.Reconfigure(5*time.Millisecond, true)
	waitFor(t, "periodic passes", func() bool { return cb.passes.Load() >= 4 })

	// Disabling stops scheduled passes; RunOnce still works.
	w.Reconfigure(5*time.Millisecond, false)
	time.Sleep(20 * time.Millisecond) // let an in-flight pass finish
	n := cb.passes.Load()
	time.Sleep(30 * time.Millisecond)
	if cb.passes.Load() != n {
		t.Fatal("passes continued while disabled")
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if cb.passes.Load() != n+1 {
		t.Fatal("RunOnce did not run while disabled")
	}

	// Re-enabling after the interval has elapsed runs right away.
	w.Reconfigure(time.Millisecond, true)
	waitFor(t, "pass after re-enable", func() bool { return cb.passes.Load() > n+1 })

	w.Stop()
	w.Stop()
	m := cb.passes.Load()
	time.Sleep(20 * time.Millisecond)
	if cb.passes.Load() != m {
		t.Fatal("passes after Stop")
	}

	// A stopped worker can be started again.
	w.Start(ctx)
	waitFor(t, "pass after restart", func() bool { return cb.passes.Load() > m })
	w.Stop()
}

func TestWorkerStopCancelsPass(t *testing.T) {
	cb := &countingBackend{Backend: memory.New(), block: make(chan struct{})}
	defer cb.Close()
	w := NewWorker(cb, Options{Interval: time.Hour})
	w.Start(context.Background())
	waitFor(t, "pass start", func() bool { return w.Status().Running })
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not cancel the running pass")
	}
	if w.Status().Running {
		t.Fatal("still running after Stop")
	}
}

func TestWorkerParentContextEnds(t *testing.T) {
	cb := &countingBackend{Backend: memory.New()}
	defer cb.Close()
	w := NewWorker(cb, Options{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	waitFor(t, "first pass", func() bool { return cb.passes.Load() == 1 })
	cancel()
	// After the parent context ends, Start launches a new scheduler.
	waitFor(t, "restart", func() bool {
		w.Start(context.Background())
		return cb.passes.Load() >= 2
	})
	w.Stop()
}

func TestWorkerConcurrentRunOnce(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
	for i := range 200 {
		e.put("b", fmt.Sprintf("k%03d", i), 1, base, storage.ObjectMeta{})
	}
	e.setNow(base.AddDate(0, 0, 10))
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total Stats
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := e.w.RunOnce(e.ctx)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total.Expired += st.Expired
			total.NoncurrentExpired += st.NoncurrentExpired
			total.Errors += st.Errors
			mu.Unlock()
		}()
	}
	// Concurrent writers while passes run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 50 {
			e.put("b", fmt.Sprintf("w%03d", i), 1, base.AddDate(0, 0, 10), storage.ObjectMeta{})
		}
	}()
	wg.Wait()
	// Passes are serialized: each key expires exactly once (a delete
	// marker), then its noncurrent version is removed by a later pass.
	if total.Expired != 200 || total.Errors != 0 {
		t.Fatalf("total %+v", total)
	}
	for i := range 200 {
		if s := e.shape("b", fmt.Sprintf("k%03d", i)); s != "do" && s != "d" {
			t.Fatalf("k%03d: %q", i, s)
		}
	}
}

func TestWorkerConcurrentChangesAfterWalk(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b",
		fltAll+`<Expiration><Days>1</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`,
		`<Filter><Prefix>m/</Prefix></Filter><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>`)
	// "gone": its noncurrent version is deleted by someone else.
	gone := e.put("b", "gone", 1, base, storage.ObjectMeta{})
	e.put("b", "gone", 1, base.Add(time.Hour), storage.ObjectMeta{})
	// "over": overwritten after the walk; the new version must survive
	// without a delete marker on top.
	e.put("b", "over", 1, base, storage.ObjectMeta{})
	// "held": a legal hold is placed on its noncurrent version.
	held := e.put("b", "held", 1, base, storage.ObjectMeta{})
	e.put("b", "held", 1, base.Add(time.Hour), storage.ObjectMeta{})
	// "m/lone": a lone delete marker that gets a new version underneath.
	e.del("b", "m/lone", base)
	// "vanish": deleted entirely after the walk.
	vanish := e.put("b", "vanish", 1, base, storage.ObjectMeta{})

	now := base.AddDate(0, 0, 10)
	e.setNow(now)
	e.b.afterWalk = func() {
		if _, err := e.b.Backend.DeleteObject(e.ctx, "b", "gone", gone.VersionID); err != nil {
			t.Error(err)
		}
		e.put("b", "over", 1, now, storage.ObjectMeta{})
		if _, err := e.b.UpdateObjectMeta(e.ctx, "b", "held", held.VersionID, func(m *storage.ObjectMeta) error {
			m.LegalHold = true
			return nil
		}); err != nil {
			t.Error(err)
		}
		e.put("b", "m/lone", 1, base, storage.ObjectMeta{})
		e.del("b", "m/lone", base)
		if _, err := e.b.Backend.DeleteObject(e.ctx, "b", "vanish", vanish.VersionID); err != nil {
			t.Error(err)
		}
	}
	// gone: current expires (marker). over: skipped. held: current expires,
	// noncurrent protected. m/lone: skipped. vanish: skipped.
	e.expectStats(e.run(), Stats{Buckets: 1, Expired: 2})
	for key, want := range map[string]string{"gone": "do", "over": "oo", "held": "doo", "m/lone": "dod", "vanish": ""} {
		if got := e.shape("b", key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
}

func TestWorkerUploadCompletedConcurrently(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b", fltAll+`<AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>`)
	u1 := e.upload("b", "a", base)
	e.upload("b", "b", base)
	e.b.afterWalk = func() {
		if err := e.b.AbortMultipartUpload(e.ctx, "b", "a", u1.UploadID); err != nil {
			t.Error(err)
		}
	}
	e.setNow(base.AddDate(0, 0, 3))
	e.expectStats(e.run(), Stats{Buckets: 1, UploadsAborted: 1})
}

func TestWorkerUnversionedOverwrittenAfterWalk(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration>`)
	e.put("b", "k", 1, base, storage.ObjectMeta{})
	now := base.AddDate(0, 0, 3)
	e.setNow(now)
	e.b.afterWalk = func() { e.put("b", "k", 2, now, storage.ObjectMeta{}) }
	e.expectStats(e.run(), Stats{Buckets: 1})
	if e.shape("b", "k") != "o" {
		t.Fatal("overwritten object was expired")
	}
}

func TestWorkerRetaggedAfterWalk(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(fmt.Sprintf("versioned=%v", versioned), func(t *testing.T) {
			e := newEnv(t)
			vs := storage.VersioningUnversioned
			if versioned {
				vs = storage.VersioningEnabled
			}
			e.bucket("b", vs, false)
			e.lifecycle("b", `<Filter><Tag><Key>tmp</Key><Value>1</Value></Tag></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`)
			o := e.put("b", "k", 1, base, storage.ObjectMeta{Tags: map[string]string{"tmp": "1"}})
			e.setNow(base.AddDate(0, 0, 3))
			e.b.afterWalk = func() {
				if _, err := e.b.UpdateObjectMeta(e.ctx, "b", "k", o.VersionID, func(m *storage.ObjectMeta) error {
					m.Tags = nil
					return nil
				}); err != nil {
					t.Error(err)
				}
			}
			e.expectStats(e.run(), Stats{Buckets: 1})
			if e.shape("b", "k") != "o" {
				t.Fatal("re-tagged object expired")
			}
		})
	}
}

func TestWorkerBucketDeletedDuringPass(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration>`)
	e.put("b", "k", 1, base, storage.ObjectMeta{})
	e.setNow(base.AddDate(0, 0, 3))
	e.b.afterWalk = func() {
		if _, err := e.b.Backend.DeleteObject(e.ctx, "b", "k", ""); err != nil {
			t.Error(err)
		}
		if err := e.b.DeleteBucket(e.ctx, "b"); err != nil {
			t.Error(err)
		}
	}
	e.expectStats(e.run(), Stats{Buckets: 1})
}

// undeleteBackend removes the delete marker of key right after a walk
// delivered it (a client "undeleting" the key mid-walk) and then reports
// IsLatest of the key's remaining entries from the current state, as a
// backend that re-evaluates IsLatest per internal batch would. The walk
// thus yields two "latest" entries for key.
type undeleteBackend struct {
	*clockBackend
	key  string
	once sync.Once
}

func (u *undeleteBackend) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	undeleted := false
	return u.clockBackend.WalkVersions(ctx, bucket, prefix, km, vm, func(o storage.ObjectInfo) error {
		if undeleted && o.Key == u.key {
			cur, err := u.clockBackend.GetObjectInfo(ctx, bucket, o.Key, "")
			o.IsLatest = err == nil && cur.VersionID == o.VersionID
		}
		if err := fn(o); err != nil {
			return err
		}
		if o.Key == u.key && o.DeleteMarker && o.IsLatest {
			var err error
			u.once.Do(func() {
				undeleted = true
				_, err = u.clockBackend.Backend.DeleteObject(ctx, bucket, o.Key, o.VersionID)
			})
			return err
		}
		return nil
	})
}

// TestWorkerInconsistentWalkKeepsRestoredVersion: when a walk reports a
// later version of a key as latest (the key changed under the walk), the
// worker must not treat that version as noncurrent and delete it.
func TestWorkerInconsistentWalkKeepsRestoredVersion(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", `<ID>nc</ID>`+fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
	day := 24 * time.Hour
	v1 := e.put("b", "k", 1, base, storage.ObjectMeta{})
	v2 := e.put("b", "k", 1, base.Add(day), storage.ObjectMeta{})
	e.del("b", "k", base.Add(2*day))
	e.setNow(base.Add(10 * day))

	u := &undeleteBackend{clockBackend: e.b, key: "k"}
	w := NewWorker(u, Options{Logger: slog.New(slog.DiscardHandler), Now: e.clock})
	t.Cleanup(w.Stop)
	st, err := w.RunOnce(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	vs := e.versions("b", "k")
	if len(vs) != 2 || vs[0].VersionID != v2.VersionID || !vs[0].IsLatest || vs[1].VersionID != v1.VersionID {
		t.Fatalf("versions after pass = %+v (stats %+v); want restored v2 %s then v1 %s", vs, st, v2.VersionID, v1.VersionID)
	}
	e.expectStats(st, Stats{Buckets: 1})

	// The next pass sees a consistent state and expires v1 only.
	e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 1})
	if vs := e.versions("b", "k"); len(vs) != 1 || vs[0].VersionID != v2.VersionID {
		t.Fatalf("versions after second pass = %+v", vs)
	}
}
