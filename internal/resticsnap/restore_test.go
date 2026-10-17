package resticsnap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// restore restores snapshot of bucket with the request req and waits.
func (e *testEnv) restore(bucket, snapshot, req string) jobs.Job {
	e.t.Helper()
	j, err := e.svc.Restore(context.Background(), bucket, snapshot, json.RawMessage(req), "test")
	if err != nil {
		e.t.Fatalf("Restore(%s): %v", req, err)
	}
	return e.wait(j.ID)
}

// read returns the content and info of the current version of bucket/key.
func (e *testEnv) read(bucket, key string) (string, storage.ObjectInfo) {
	e.t.Helper()
	oi, rc, err := e.router.OpenObject(context.Background(), bucket, key, "")
	if err != nil {
		e.t.Fatalf("open %s/%s: %v", bucket, key, err)
	}
	if rc == nil {
		return "", oi
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data), oi
}

func TestPrefixRestoreDoesNotInstallBucketPolicy(t *testing.T) {
	for _, snapshotPrefix := range []string{"", "public/"} {
		t.Run("snapshot-prefix="+snapshotPrefix, func(t *testing.T) {
			e := newEnv(t, "mem", Options{Incremental: true})
			e.createBucket("src", "", storage.VersioningEnabled)
			e.put("src", "public/file", "public", storage.ObjectMeta{})
			policy := []byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::src/*"}]}`)
			if err := e.router.PutBucketConfig(t.Context(), "src", storage.ConfigPolicy, policy); err != nil {
				t.Fatal(err)
			}
			e.configure("src", `{"enabled":false,"prefix":"`+snapshotPrefix+`","target":{"kind":"backing","backing":"disk"}}`)
			st := e.snapshot("src")
			prefix := "public/"
			if snapshotPrefix != "" {
				prefix = ""
			}
			j := e.restore("src", st.Snapshot, `{"target":{"bucket":"dst","create":{"backing":"mem"}},"prefix":"`+prefix+`"}`)
			if j.State != jobs.StateSucceeded {
				t.Fatal(j.LastError)
			}
			if _, err := e.router.GetBucketConfig(t.Context(), "dst", storage.ConfigPolicy); !errors.Is(err, storage.ErrConfigNotFound) {
				t.Fatalf("prefix restore installed policy: %v", err)
			}
			if !strings.Contains(string(j.Status), "bucket policy not restored") {
				t.Fatalf("missing warning: %s", j.Status)
			}
			if data, _ := e.read("dst", "public/file"); data != "public" {
				t.Fatal(data)
			}
		})
	}
}

// A snapshot restored into a new bucket: content, metadata, version IDs
// and modification times (the memory backend imports versions), delete
// markers, versioning and configurations.
func TestRestoreRoundTrip(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("src", "", storage.VersioningEnabled)
	meta := storage.ObjectMeta{
		ContentType: "image/png", CacheControl: "max-age=60",
		UserMeta: map[string]string{"camera": "x100"}, Tags: map[string]string{"t": "1"},
	}
	want := maps.Clone(trickyObjects)
	infos := map[string]storage.ObjectInfo{}
	for k, v := range want {
		infos[k] = e.put("src", k, v, meta)
	}
	e.put("src", "a.txt", "old version", storage.ObjectMeta{})
	infos["a.txt"] = e.put("src", "a.txt", want["a.txt"], meta)
	e.put("src", "deleted", "gone", storage.ObjectMeta{})
	dm, err := e.router.DeleteObject(ctx, "src", "deleted", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.router.PutBucketConfig(ctx, "src", storage.ConfigCORS, []byte("<CORSConfiguration></CORSConfiguration>")); err != nil {
		t.Fatal(err)
	}
	e.configure("src", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("src")

	j := e.restore("src", st.Snapshot, `{"target":{"bucket":"dst","create":{"backing":"mem"}}}`)
	if j.State != jobs.StateSucceeded {
		t.Fatalf("restore: %s: %s", j.State, j.LastError)
	}
	if j.Labels["source"] != "src" || j.Labels["target"] != "dst" || len(j.Labels) != 2 {
		t.Fatalf("labels %v", j.Labels)
	}
	var rs jobStatus
	if err := json.Unmarshal(j.Status, &rs); err != nil || rs.Keys != int64(len(want)+1) || rs.Snapshot != st.Snapshot {
		t.Fatalf("restore status %s (%v)", j.Status, err)
	}
	for k, v := range want {
		data, oi := e.read("dst", k)
		src := infos[k]
		if data != v || oi.VersionID != src.VersionID || !oi.LastModified.Equal(src.LastModified) || oi.ETag != src.ETag ||
			oi.ContentType != "image/png" || oi.CacheControl != "max-age=60" || oi.UserMeta["camera"] != "x100" || oi.Tags["t"] != "1" {
			t.Fatalf("restored %q = %q %+v, want %q %+v", k, data, oi, v, src)
		}
	}
	if _, oi := e.read("dst", "deleted"); !oi.DeleteMarker || oi.VersionID != dm.VersionID {
		t.Fatalf("restored delete marker %+v, want version %s", oi, dm.VersionID)
	}
	info, err := e.router.GetBucket(ctx, "dst")
	if err != nil || info.Versioning != storage.VersioningEnabled {
		t.Fatalf("restored bucket %+v, %v", info, err)
	}
	if _, err := e.router.GetBucketConfig(ctx, "dst", storage.ConfigCORS); err != nil {
		t.Fatalf("cors: %v", err)
	}
	if _, err := e.router.GetBucketConfig(ctx, "dst", storage.ConfigResticSnapshots); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("restic-snapshots copied: %v", err)
	}

	// A non-empty target needs onExisting merge; merging again skips what
	// is there.
	if _, err := e.svc.Restore(ctx, "src", st.Snapshot, json.RawMessage(`{"target":{"bucket":"dst"}}`), "test"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("restore into a non-empty bucket: %v", err)
	}
	j = e.restore("src", st.Snapshot, `{"target":{"bucket":"dst"},"onExisting":"merge","configs":"none"}`)
	if err := json.Unmarshal(j.Status, &rs); err != nil || j.State != jobs.StateSucceeded || rs.Materialized != 0 || rs.Keys != int64(len(want)+1) {
		t.Fatalf("merge restore %s: %s %s", j.State, j.LastError, j.Status)
	}

	// A prefix restores a subtree.
	j = e.restore("src", st.Snapshot, `{"target":{"bucket":"sub","create":{}},"prefix":"dir/"}`)
	if j.State != jobs.StateSucceeded {
		t.Fatalf("prefix restore: %s", j.LastError)
	}
	var keys []string
	if err := e.router.WalkObjects(ctx, "sub", "", "", func(oi storage.ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || keys[0] != "dir/" || keys[1] != "dir/b.txt" || keys[2] != "dir/sub/c.txt" {
		t.Fatalf("prefix restore holds %v", keys)
	}
}

// Restores are validated before they are queued.
func TestRestoreValidation(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("src", "", "")
	e.put("src", "k", "v", storage.ObjectMeta{})
	e.configure("src", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("src")
	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	for name, c := range map[string]struct {
		id, req string
		want    error
	}{
		"unknown field":   {st.Snapshot, `{"target":{"bucket":"dst","create":{}},"x":1}`, jobs.ErrInvalidSpec},
		"no target":       {st.Snapshot, `{"target":{"bucket":"nope"}}`, jobs.ErrInvalidSpec},
		"bad name":        {st.Snapshot, `{"target":{"bucket":"Bad_Name","create":{}}}`, jobs.ErrInvalidSpec},
		"bad mode":        {st.Snapshot, `{"target":{"bucket":"dst","create":{}},"onExisting":"adopt"}`, jobs.ErrInvalidSpec},
		"unknown backing": {st.Snapshot, `{"target":{"bucket":"dst","create":{"backing":"x"}}}`, jobs.ErrInvalidSpec},
		"missing":         {missing, `{"target":{"bucket":"dst","create":{}}}`, restic.ErrSnapshotNotFound},
		"short ID":        {st.Snapshot[:8], `{"target":{"bucket":"dst","create":{}}}`, jobs.ErrInvalidSpec},
	} {
		if _, err := e.svc.Restore(ctx, "src", c.id, json.RawMessage(c.req), "test"); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	// Another bucket's snapshot is not this bucket's.
	e.createBucket("other", "", "")
	e.configure("other", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	if _, err := e.svc.Restore(ctx, "other", st.Snapshot, json.RawMessage(`{"target":{"bucket":"dst","create":{}}}`), "test"); err == nil {
		t.Fatal("restore of another bucket's snapshot")
	}
	// A deleted bucket's snapshots stay restorable.
	if _, err := e.router.DeleteObject(ctx, "src", "k", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.router.DeleteBucket(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	j := e.restore("src", st.Snapshot, `{"target":{"bucket":"src","create":{}}}`)
	if j.State != jobs.StateSucceeded {
		t.Fatalf("restore of a deleted bucket: %s", j.LastError)
	}
	if data, _ := e.read("src", "k"); data != "v" {
		t.Fatalf("restored %q", data)
	}
}

// A merge restore never replaces a target version under object lock: a
// snapshot's null version of a key must not overwrite the target's locked
// null version (H-05).
func TestRestoreMergeKeepsLockedVersions(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("src", "", "")
	e.put("src", "k", "attacker", storage.ObjectMeta{})
	e.put("src", "free", "restored", storage.ObjectMeta{})
	e.configure("src", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("src")

	e.createBucket("vault", "", "")
	until := time.Now().Add(time.Hour)
	locked := e.put("vault", "k", "kept", storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: until})
	if err := e.router.SetBucketVersioning(ctx, "vault", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	j := e.restore("src", st.Snapshot, `{"target":{"bucket":"vault"},"onExisting":"merge","configs":"none"}`)
	if j.State != jobs.StateSucceeded {
		t.Fatalf("merge restore: %s: %s", j.State, j.LastError)
	}
	data, oi := e.read("vault", "k")
	if data != "kept" || oi.VersionID != locked.VersionID || oi.RetentionMode != "COMPLIANCE" {
		t.Fatalf("locked version after the restore: %q %+v", data, oi)
	}
	if data, _ := e.read("vault", "free"); data != "restored" {
		t.Fatalf("unlocked key not restored: %q", data)
	}
	var rs jobStatus
	if err := json.Unmarshal(j.Status, &rs); err != nil || len(rs.Warnings) != 1 || !strings.Contains(rs.Warnings[0], "COMPLIANCE") {
		t.Fatalf("restore status %s (%v)", j.Status, err)
	}
}
