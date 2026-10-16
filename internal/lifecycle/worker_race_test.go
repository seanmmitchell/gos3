package lifecycle

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// Regression tests for the check-then-delete race: a client request that
// commits after the worker re-checked a version but before its delete must
// never be undone by the delete. beforeDelete injects that request.

func TestWorkerDeleteRaceOverwriteUnversioned(t *testing.T) {
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.lifecycle("b", `<Filter><Tag><Key>expire</Key><Value>yes</Value></Tag></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`)
	e.put("b", "k", 1, base, storage.ObjectMeta{Tags: map[string]string{"expire": "yes"}})
	now := base.AddDate(0, 0, 3)
	e.setNow(now)
	var fresh storage.ObjectInfo
	e.b.beforeDelete = func(bucket, key, _ string) {
		e.b.beforeDelete = nil
		fresh = e.put("b", "k", 7, now, storage.ObjectMeta{}) // untagged: exempt from the rule
	}
	e.expectStats(e.run(), Stats{Buckets: 1})
	o, err := e.b.GetObjectInfo(e.ctx, "b", "k", "")
	if err != nil || o.ETag != fresh.ETag {
		t.Fatalf("object written before the delete was removed: %+v %v", o, err)
	}
}

func TestWorkerDeleteRaceOverwriteEnabled(t *testing.T) {
	// In a versioned bucket the race would hide the new object behind a
	// delete marker.
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", fltAll+`<Expiration><Days>1</Days></Expiration>`)
	e.put("b", "k", 1, base, storage.ObjectMeta{})
	now := base.AddDate(0, 0, 3)
	e.setNow(now)
	e.b.beforeDelete = func(bucket, key, _ string) {
		e.b.beforeDelete = nil
		e.put("b", "k", 7, now, storage.ObjectMeta{})
	}
	e.expectStats(e.run(), Stats{Buckets: 1})
	if got := e.shape("b", "k"); got != "oo" {
		t.Fatalf("shape %q, want the new version to stay current (oo)", got)
	}
}

func TestWorkerDeleteRaceSuspendedNullReplaced(t *testing.T) {
	// A noncurrent null version is replaced by a new current null version
	// (PUT in a suspended bucket) just before the noncurrent expiration
	// deletes version "null".
	e := newEnv(t)
	e.bucket("b", storage.VersioningUnversioned, false)
	e.put("b", "k", 1, base, storage.ObjectMeta{}) // null
	if err := e.b.SetBucketVersioning(e.ctx, "b", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	e.put("b", "k", 1, base.Add(time.Hour), storage.ObjectMeta{}) // null becomes noncurrent
	if err := e.b.SetBucketVersioning(e.ctx, "b", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	e.lifecycle("b", fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
	now := base.AddDate(0, 0, 3)
	e.setNow(now)
	var fresh storage.ObjectInfo
	e.b.beforeDelete = func(bucket, key, vid string) {
		e.b.beforeDelete = nil
		fresh = e.put("b", "k", 7, now, storage.ObjectMeta{})
	}
	e.expectStats(e.run(), Stats{Buckets: 1})
	o, err := e.b.GetObjectInfo(e.ctx, "b", "k", "")
	if err != nil || o.ETag != fresh.ETag || o.VersionID != storage.NullVersionID {
		t.Fatalf("current null version written before the delete was removed: %+v %v (shape %q)", o, err, e.shape("b", "k"))
	}
}

func TestWorkerDeleteRaceLockedMeanwhile(t *testing.T) {
	for _, lock := range []string{"compliance", "legal-hold"} {
		t.Run(lock, func(t *testing.T) {
			e := newEnv(t)
			e.bucket("b", storage.VersioningEnabled, true)
			e.lifecycle("b", fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
			old := e.put("b", "k", 1, base, storage.ObjectMeta{})
			e.put("b", "k", 1, base.Add(time.Hour), storage.ObjectMeta{})
			now := base.AddDate(0, 0, 3)
			e.setNow(now)
			e.b.beforeDelete = func(bucket, key, vid string) {
				e.b.beforeDelete = nil
				if _, err := e.b.UpdateObjectMeta(e.ctx, "b", "k", old.VersionID, func(m *storage.ObjectMeta) error {
					if lock == "legal-hold" {
						m.LegalHold = true
					} else {
						m.RetentionMode, m.RetainUntil = "COMPLIANCE", now.AddDate(1, 0, 0)
					}
					return nil
				}); err != nil {
					t.Error(err)
				}
			}
			e.expectStats(e.run(), Stats{Buckets: 1})
			if _, err := e.b.GetObjectInfo(e.ctx, "b", "k", old.VersionID); err != nil {
				t.Fatalf("version locked before the delete was removed: %v", err)
			}
		})
	}
}

func TestWorkerDeleteRaceRetaggedNoncurrent(t *testing.T) {
	// A noncurrent version re-tagged out of the rule's filter is kept.
	e := newEnv(t)
	e.bucket("b", storage.VersioningEnabled, false)
	e.lifecycle("b", `<Filter><Tag><Key>tmp</Key><Value>1</Value></Tag></Filter><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
	old := e.put("b", "k", 1, base, storage.ObjectMeta{Tags: map[string]string{"tmp": "1"}})
	e.put("b", "k", 1, base.Add(time.Hour), storage.ObjectMeta{})
	e.setNow(base.AddDate(0, 0, 3))
	e.b.beforeDelete = func(bucket, key, vid string) {
		e.b.beforeDelete = nil
		if _, err := e.b.UpdateObjectMeta(e.ctx, "b", "k", old.VersionID, func(m *storage.ObjectMeta) error {
			m.Tags = nil
			return nil
		}); err != nil {
			t.Error(err)
		}
	}
	e.expectStats(e.run(), Stats{Buckets: 1})
	if _, err := e.b.GetObjectInfo(e.ctx, "b", "k", old.VersionID); err != nil {
		t.Fatalf("re-tagged version removed: %v", err)
	}
}

// TestWorkerNoncurrentRuleRecheckedAtDelete: NewerNoncurrentVersions and
// NoncurrentDays are evaluated again, atomically with the delete, against
// the versions newer than the expiring one at that time. A newer version
// removed after the walk can stop the delete (issue #35).
func TestWorkerNoncurrentRuleRecheckedAtDelete(t *testing.T) {
	day := 24 * time.Hour
	t.Run("NewerNoncurrentVersions", func(t *testing.T) {
		e := newEnv(t)
		e.bucket("b", storage.VersioningEnabled, false)
		e.lifecycle("b", fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays><NewerNoncurrentVersions>1</NewerNoncurrentVersions></NoncurrentVersionExpiration>`)
		oldest := e.put("b", "k", 1, base, storage.ObjectMeta{})
		middle := e.put("b", "k", 1, base.Add(time.Hour), storage.ObjectMeta{})
		cur := e.put("b", "k", 1, base.Add(2*time.Hour), storage.ObjectMeta{})
		e.setNow(base.Add(10 * day))
		// The walk saw one newer noncurrent version above oldest; it is
		// gone before the delete, so oldest is the only one left to keep.
		e.b.afterWalk = func() {
			if _, err := e.b.Backend.DeleteObject(e.ctx, "b", "k", middle.VersionID); err != nil {
				t.Error(err)
			}
		}
		e.expectStats(e.run(), Stats{Buckets: 1})
		vs := e.versions("b", "k")
		if len(vs) != 2 || vs[0].VersionID != cur.VersionID || vs[1].VersionID != oldest.VersionID {
			t.Fatalf("versions %+v, want current %s and oldest %s", vs, cur.VersionID, oldest.VersionID)
		}
	})
	t.Run("NoncurrentDays", func(t *testing.T) {
		e := newEnv(t)
		e.bucket("b", storage.VersioningEnabled, false)
		e.lifecycle("b", fltAll+`<NoncurrentVersionExpiration><NoncurrentDays>3</NoncurrentDays></NoncurrentVersionExpiration>`)
		// Expiry rounds up to the next midnight UTC (base is at noon).
		v1 := e.put("b", "k", 1, base, storage.ObjectMeta{})
		v2 := e.put("b", "k", 1, base.Add(day), storage.ObjectMeta{}) // v1 noncurrent since: due 2024-01-15
		v3 := e.put("b", "k", 1, base.Add(4*day), storage.ObjectMeta{})
		e.setNow(utc(2024, 1, 15, 0, 0, 0))
		// Without v2, v1 is noncurrent since v3 was written: due 2024-01-18.
		e.b.afterWalk = func() {
			if _, err := e.b.Backend.DeleteObject(e.ctx, "b", "k", v2.VersionID); err != nil {
				t.Error(err)
			}
		}
		e.expectStats(e.run(), Stats{Buckets: 1})
		vs := e.versions("b", "k")
		if len(vs) != 2 || vs[0].VersionID != v3.VersionID || vs[1].VersionID != v1.VersionID {
			t.Fatalf("versions %+v, want %s and %s", vs, v3.VersionID, v1.VersionID)
		}
		e.setNow(utc(2024, 1, 17, 23, 59, 0))
		e.expectStats(e.run(), Stats{Buckets: 1})
		e.setNow(utc(2024, 1, 18, 0, 0, 0))
		e.expectStats(e.run(), Stats{Buckets: 1, NoncurrentExpired: 1})
	})
}

func TestWorkerWithoutConditionalDeleter(t *testing.T) {
	// A backend that cannot delete conditionally only gets the actions a
	// race cannot turn into data loss: delete markers in versioning-enabled
	// buckets and upload aborts. Permanent deletes are skipped.
	e := newEnv(t)
	e.w = NewWorker(plainBackend{e.b}, Options{Logger: e.w.log, Now: e.clock})
	e.bucket("u", storage.VersioningUnversioned, false)
	e.lifecycle("u", fltAll+`<Expiration><Days>1</Days></Expiration><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>`)
	e.put("u", "k", 1, base, storage.ObjectMeta{})
	u := e.upload("u", "up", base)
	e.bucket("v", storage.VersioningEnabled, false)
	e.lifecycle("v", fltAll+`<Expiration><Days>1</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)
	e.put("v", "k", 1, base, storage.ObjectMeta{})
	e.put("v", "k", 1, base.Add(time.Hour), storage.ObjectMeta{})
	e.setNow(base.AddDate(0, 0, 3))

	e.expectStats(e.run(), Stats{Buckets: 2, Expired: 1, UploadsAborted: 1})
	if got := e.shape("u", "k"); got != "o" {
		t.Errorf("unversioned object: %q, want it kept", got)
	}
	if got := e.shape("v", "k"); got != "doo" {
		t.Errorf("versioned key: %q, want a delete marker and both versions kept", got)
	}
	if _, err := e.b.GetMultipartUpload(e.ctx, "u", "up", u.UploadID); !errors.Is(err, storage.ErrUploadNotFound) {
		t.Errorf("upload not aborted: %v", err)
	}
	if !strings.Contains(e.logs(), "cannot delete conditionally") {
		t.Errorf("missing warning:\n%s", e.logs())
	}
}

func TestWorkerAbortsManyUploadsInBatches(t *testing.T) {
	// The abort pass walks uploads in batches through the backend's
	// UploadWalker (here the memory backend's, unwrapped).
	m := memory.New()
	defer m.Close()
	ctx := t.Context()
	if err := m.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(doc(`<Filter><Prefix>tmp/</Prefix></Filter><Status>Enabled</Status><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload>`)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PutBucketConfig(ctx, "b", storage.ConfigLifecycle, data); err != nil {
		t.Fatal(err)
	}
	n := 2*walkBatch + 17
	for i := range n {
		key := fmt.Sprintf("tmp/%d", i%(walkBatch/2)) // several uploads per key
		if _, err := m.CreateMultipartUpload(ctx, "b", key, storage.ObjectMeta{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	keep, err := m.CreateMultipartUpload(ctx, "b", "keep", storage.ObjectMeta{}, "")
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorker(m, Options{Now: func() time.Time { return time.Now().AddDate(0, 0, 2) }})
	st, err := w.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.UploadsAborted != n || st.Errors != 0 {
		t.Fatalf("stats %+v, want %d uploads aborted", st, n)
	}
	ups, err := m.ListMultipartUploads(ctx, "b")
	if err != nil || len(ups) != 1 || ups[0].UploadID != keep.UploadID {
		t.Fatalf("uploads left: %+v %v", ups, err)
	}
}

// TestWorkerConcurrentOverwriteStress is the reviewer's reproduction: an
// exempt (untagged) object whose PUT races with a pass is never deleted.
func TestWorkerConcurrentOverwriteStress(t *testing.T) {
	m := memory.New()
	defer m.Close()
	ctx := t.Context()
	if err := m.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]byte(doc(`<Filter><Tag><Key>expire</Key><Value>yes</Value></Tag></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration>`)))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := c.Marshal()
	if err := m.PutBucketConfig(ctx, "b", storage.ConfigLifecycle, data); err != nil {
		t.Fatal(err)
	}
	put := func(tags map[string]string, etag string) {
		w, err := m.NewObjectWriter(ctx, "b", "k")
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := w.Commit(ctx, storage.ObjectMeta{ETag: etag, Tags: tags}, storage.CommitOptions{}); err != nil {
			t.Error(err)
		}
	}
	w := NewWorker(m, Options{Now: func() time.Time { return time.Now().Add(72 * time.Hour) }})
	for i := range 500 {
		put(map[string]string{"expire": "yes"}, "tagged")
		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := w.RunOnce(ctx); err != nil {
				t.Error(err)
			}
		}()
		put(nil, fmt.Sprint("exempt-", i))
		<-done
		o, err := m.GetObjectInfo(ctx, "b", "k", "")
		if err != nil {
			t.Fatalf("iteration %d: exempt object deleted: %v", i, err)
		}
		if o.ETag != fmt.Sprint("exempt-", i) {
			t.Fatalf("iteration %d: current object %q", i, o.ETag)
		}
	}
}
