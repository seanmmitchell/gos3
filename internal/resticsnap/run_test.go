package resticsnap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// trickyObjects are keys that need encoding or collision handling, with
// their content.
var trickyObjects = map[string]string{
	"a.txt":         "alpha",
	"A.txt":         "ALPHA, upper case",
	"dir":           "an object next to a prefix",
	"dir/":          "a folder marker",
	"dir/b.txt":     "bravo",
	"dir/sub/c.txt": "charlie",
	"x:y?.txt":      "reserved characters",
	"CON":           "a device name",
	"trail.":        "a trailing dot",
	" lead":         "a leading space",
	"100%":          "a percent sign",
	"empty":         "",
	"deep//x":       "an empty segment",
}

// A full snapshot of a memory bucket, then incremental ones: only changed
// keys are read, the others are taken from the parent snapshot.
func TestSnapshotFullAndIncremental(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("photos", "", "")
	want := maps.Clone(trickyObjects)
	for k, v := range want {
		e.put("photos", k, v, storage.ObjectMeta{ContentType: "text/plain", UserMeta: map[string]string{"k": "v"}})
	}
	cfg := e.configure("photos", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	if cfg.Target.Kind != TargetBacking || !strings.HasPrefix(cfg.Target.Repo, repoPrefix) {
		t.Fatalf("target = %+v", cfg.Target)
	}

	st := e.snapshot("photos")
	if st.Keys != int64(len(want)) || st.Materialized != int64(len(want)) || st.Partials != 0 {
		t.Fatalf("first run status %+v", st)
	}
	entries := e.checkSnapshot("photos", st.Snapshot, want)
	for _, en := range entries {
		if en.Meta == nil || en.Meta.ContentType != "text/plain" || en.Meta.UserMeta["k"] != "v" || en.VersionID == "" || en.Fingerprint == "" {
			t.Fatalf("manifest entry %+v", en)
		}
	}
	first := st.Snapshot

	// Nothing changed: nothing is read.
	st = e.snapshot("photos")
	if st.Materialized != 0 || st.Keys != int64(len(want)) {
		t.Fatalf("unchanged run status %+v", st)
	}
	e.checkSnapshot("photos", st.Snapshot, want)

	// One overwrite of the same size (in the same second: its file's
	// modification time is bumped), one new key, one deleted key.
	want["a.txt"] = "ALPHA"
	e.put("photos", "a.txt", want["a.txt"], storage.ObjectMeta{})
	want["new.txt"] = "news"
	e.put("photos", "new.txt", want["new.txt"], storage.ObjectMeta{})
	delete(want, "dir/sub/c.txt")
	if _, err := e.router.DeleteObject(ctx, "photos", "dir/sub/c.txt", ""); err != nil {
		t.Fatal(err)
	}
	st = e.snapshot("photos")
	if st.Materialized != 2 || st.Keys != int64(len(want)) {
		t.Fatalf("changed run status %+v", st)
	}
	e.checkSnapshot("photos", st.Snapshot, want)

	snaps, err := e.svc.Snapshots(ctx, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 || snaps[0].ID != st.Snapshot || snaps[2].ID != first {
		t.Fatalf("snapshots = %+v", snaps)
	}
	for _, sn := range snaps {
		if sn.Host != SnapshotHost || !sn.HasTag(TagSnapshot) || !sn.HasTag(restic.BucketTag("photos")) || sn.HasTag(TagPartial) {
			t.Fatalf("snapshot %+v", sn)
		}
	}
	// Placeholders: the unchanged run read no file.
	if s := snaps[1].Summary; s == nil || s.FilesNew+s.FilesChanged > 2 {
		t.Fatalf("unchanged run summary %+v", snaps[1].Summary)
	}

	var status statusDoc
	raw, err := e.svc.Status(ctx, "photos")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status.LastSnapshot != st.Snapshot || status.LastError != "" || status.LastSuccessAt.IsZero() || !status.NextRunAt.IsZero() || status.Running != "" {
		t.Fatalf("status %s", raw)
	}
	info, err := e.svc.Repository(ctx, "photos")
	if err != nil || info == nil || !info.Initialized || !strings.Contains(info.Repository, cfg.Target.Repo) {
		t.Fatalf("repository = %+v, %v", info, err)
	}
}

// A snapshot run through a fresh stage (the stamp lost) rebuilds the tree
// from the parent's manifest and still reads only what changed.
func TestSnapshotRebuildsStage(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	e.createBucket("b1", "", "")
	want := map[string]string{"k1": "one", "k2": "two", "d/k3": "three"}
	for k, v := range want {
		e.put("b1", k, v, storage.ObjectMeta{})
	}
	e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	e.snapshot("b1")
	if err := os.RemoveAll(e.svc.stageDir); err != nil {
		t.Fatal(err)
	}
	want["k2"] = "two, changed"
	e.put("b1", "k2", want["k2"], storage.ObjectMeta{})
	st := e.snapshot("b1")
	if st.Materialized != 1 {
		t.Fatalf("status %+v", st)
	}
	e.checkSnapshot("b1", st.Snapshot, want)
}

// A tiny spool budget chains partial snapshots, which are forgotten once
// the complete snapshot is saved.
func TestSnapshotPartialChain(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true, SpoolBytes: 10})
	e.createBucket("big", "", "")
	want := map[string]string{}
	for _, k := range []string{"o1", "o2", "o3", "o4", "o5", "o6"} {
		want[k] = "8 bytes" + k[1:]
		e.put("big", k, want[k], storage.ObjectMeta{})
	}
	e.configure("big", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("big")
	if st.Partials != 5 || st.Materialized != 6 || st.Forgotten != 5 {
		t.Fatalf("status %+v", st)
	}
	e.checkSnapshot("big", st.Snapshot, want)
	all, err := e.svc.rt.Snapshots(context.Background(), e.location("big"), restic.SnapshotFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != st.Snapshot {
		t.Fatalf("repository holds %+v", all)
	}
	// Incremental runs chain partial snapshots too.
	want["o2"], want["o5"] = "changed2", "changed5"
	e.put("big", "o2", want["o2"], storage.ObjectMeta{})
	e.put("big", "o5", want["o5"], storage.ObjectMeta{})
	st = e.snapshot("big")
	if st.Partials != 1 || st.Materialized != 2 {
		t.Fatalf("incremental status %+v", st)
	}
	e.checkSnapshot("big", st.Snapshot, want)
}

// Without incremental runs, every key is read every time.
func TestSnapshotFull(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: false})
	e.createBucket("b1", "", "")
	want := map[string]string{"k1": "one", "k2": "two"}
	for k, v := range want {
		e.put("b1", k, v, storage.ObjectMeta{})
	}
	e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	e.snapshot("b1")
	st := e.snapshot("b1")
	if st.Materialized != 2 {
		t.Fatalf("status %+v", st)
	}
	e.checkSnapshot("b1", st.Snapshot, want)
}

// A restic that reads the placeholders fails the verification: the run
// forgets the snapshot, stages the real content and backs up again. When
// that cannot fit in the spool budget, the job fails with a hint.
func TestSnapshotVerification(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	e.createBucket("v1", "", "")
	want := map[string]string{"k1": "one", "k2": "two", "d/k3": "three", "k4": "four"}
	for k, v := range want {
		e.put("v1", k, v, storage.ObjectMeta{})
	}
	e.configure("v1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	e.snapshot("v1")
	t.Setenv(restictest.EnvReread, "1")
	want["k2"] = "two, changed"
	e.put("v1", "k2", want["k2"], storage.ObjectMeta{})
	before := restictest.Calls(t, e.log, "diff")
	st := e.snapshot("v1")
	if st.Materialized != 4 {
		t.Fatalf("status %+v", st)
	}
	if n := restictest.Calls(t, e.log, "diff") - before; n != 1 {
		t.Fatalf("diff ran %d times", n)
	}
	e.checkSnapshot("v1", st.Snapshot, want)
	all, err := e.svc.rt.Snapshots(context.Background(), e.location("v1"), restic.SnapshotFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("repository holds %+v (%v)", all, err)
	}

	// The real content of the placeholders exceeds the spool budget.
	e.svc.spool = 4
	e.put("v1", "k1", "one, changed", storage.ObjectMeta{})
	j, err := e.svc.RunNow(context.Background(), "v1", "test")
	if err != nil {
		t.Fatal(err)
	}
	j = e.wait(j.ID)
	if j.State != jobs.StateFailed || !strings.Contains(j.LastError, "incremental: false") {
		t.Fatalf("job %s: %s", j.State, j.LastError)
	}
	var status statusDoc
	raw, _ := e.svc.Status(context.Background(), "v1")
	if err := json.Unmarshal(raw, &status); err != nil || !strings.Contains(status.LastError, "incremental: false") {
		t.Fatalf("status %s", raw)
	}
	// Nothing unverified is left behind.
	all, err = e.svc.rt.Snapshots(context.Background(), e.location("v1"), restic.SnapshotFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("repository holds %+v (%v)", all, err)
	}
}

// Snapshots of an fs bucket go to its own repository by default, next to
// its per-version archive, and are tagged with its ID; a consistent
// snapshot of a versioned bucket holds the versions current at its start.
func TestSnapshotBucketTarget(t *testing.T) {
	e := newEnv(t, "disk", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("files", "", storage.VersioningEnabled)
	e.put("files", "k", "old", storage.ObjectMeta{})
	e.put("files", "k", "new", storage.ObjectMeta{})
	e.put("files", "gone", "x", storage.ObjectMeta{})
	if _, err := e.router.DeleteObject(ctx, "files", "gone", ""); err != nil {
		t.Fatal(err)
	}
	cfg := e.configure("files", `{"enabled":false}`)
	if cfg.Target != (Target{Kind: TargetBucket}) || !cfg.Consistent {
		t.Fatalf("config %+v", cfg)
	}
	st := e.snapshot("files")
	entries := e.checkSnapshot("files", st.Snapshot, map[string]string{"k": "new"})
	if len(entries) != 2 || !entries[0].DeleteMarker || entries[0].Key != "gone" {
		t.Fatalf("entries %+v", entries)
	}
	snaps, err := e.svc.Snapshots(ctx, "files")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots %+v, %v", snaps, err)
	}
	hasID := false
	for _, tag := range snaps[0].Tags {
		hasID = hasID || strings.HasPrefix(tag, restic.TagBucketIDPrefix)
	}
	info, err := e.svc.Repository(ctx, "files")
	if err != nil || info == nil || info.BucketID == "" || !hasID {
		t.Fatalf("repository %+v (%v), tags %v", info, err, snaps[0].Tags)
	}
	// Hold the only worker, so that the job stays active.
	e.svc.sem <- struct{}{}
	j, err := e.svc.RunNow(ctx, "files", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RunNow(ctx, "files", "test"); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("second RunNow: %v", err)
	}
	raw, err := e.svc.Status(ctx, "files")
	if err != nil || !strings.Contains(string(raw), `"running":"`+j.ID+`"`) {
		t.Fatalf("status %s, %v", raw, err)
	}
	<-e.svc.sem
	if j = e.wait(j.ID); j.State != jobs.StateSucceeded {
		t.Fatalf("job %s: %s", j.State, j.LastError)
	}
}

// The stage of an interrupted run (no stamp, stale partial snapshots) is
// recovered by the next run.
func TestSnapshotStalePartials(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	e.createBucket("b1", "", "")
	e.put("b1", "k", "data", storage.ObjectMeta{})
	e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	loc := e.location("b1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.rt.BackupTree(context.Background(), loc, dir, restic.TreeOptions{
		Host: SnapshotHost, Tags: []string{TagSnapshot, restic.BucketTag("b1"), TagPartial},
	}); err != nil {
		t.Fatal(err)
	}
	st := e.snapshot("b1")
	if st.Forgotten != 1 {
		t.Fatalf("status %+v", st)
	}
	e.checkSnapshot("b1", st.Snapshot, map[string]string{"k": "data"})
}

// The snapshot scenarios against the real restic (skipped when it is not
// on PATH): full, incremental with placeholders (nothing unchanged is
// read), a partial chain, verification and a restore round trip.
func TestRealResticSnapshots(t *testing.T) {
	snapshotScenario(t, newRealEnv(t, "mem", Options{Incremental: true}))
}

// TestSnapshotScenario runs the scenario of TestRealResticSnapshots with
// the fake restic.
func TestSnapshotScenario(t *testing.T) {
	snapshotScenario(t, newEnv(t, "mem", Options{Incremental: true}))
}

// snapshotScenario snapshots a versioned bucket in full, incrementally and
// with a partial snapshot, then restores it.
func snapshotScenario(t *testing.T, e *testEnv) {
	ctx := context.Background()
	e.createBucket("real", "", storage.VersioningEnabled)
	want := maps.Clone(trickyObjects)
	want["big.bin"] = strings.Repeat("0123456789abcdef", 1<<16) // 1 MiB, several chunks
	infos := map[string]storage.ObjectInfo{}
	for k, v := range want {
		infos[k] = e.put("real", k, v, storage.ObjectMeta{ContentType: "text/plain"})
	}
	e.configure("real", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("real")
	if st.Materialized != int64(len(want)) {
		t.Fatalf("first run status %+v", st)
	}
	e.checkSnapshot("real", st.Snapshot, want)

	st = e.snapshot("real")
	if st.Materialized != 0 {
		t.Fatalf("unchanged run status %+v", st)
	}
	snaps, err := e.svc.Snapshots(ctx, "real")
	if err != nil || len(snaps) != 2 || snaps[0].Summary == nil || snaps[0].Summary.FilesNew+snaps[0].Summary.FilesChanged > 2 {
		t.Fatalf("snapshots %+v (%v)", snaps, err)
	}
	e.checkSnapshot("real", st.Snapshot, want)

	want["a.txt"] = "ALPHA"
	infos["a.txt"] = e.put("real", "a.txt", want["a.txt"], storage.ObjectMeta{ContentType: "text/plain"})
	want["new.txt"] = "news"
	infos["new.txt"] = e.put("real", "new.txt", want["new.txt"], storage.ObjectMeta{ContentType: "text/plain"})
	e.svc.spool = 3 // a partial snapshot between the two
	st = e.snapshot("real")
	if st.Materialized != 2 || st.Partials != 1 {
		t.Fatalf("changed run status %+v", st)
	}
	e.checkSnapshot("real", st.Snapshot, want)
	e.svc.spool = DefaultSpoolBytes

	j := e.restore("real", st.Snapshot, `{"target":{"bucket":"real-restored","create":{"backing":"mem"}}}`)
	if j.State != jobs.StateSucceeded {
		t.Fatalf("restore: %s", j.LastError)
	}
	for k, v := range want {
		oi, rc, err := e.router.OpenObject(ctx, "real-restored", k, "")
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		_, err = io.Copy(&b, rc)
		rc.Close()
		if err != nil || b.String() != v || oi.VersionID != infos[k].VersionID || oi.ContentType != "text/plain" {
			t.Fatalf("restored %q: %d bytes, %+v (%v)", k, b.Len(), oi, err)
		}
	}
}

// A prefix limits the snapshot; a run prunes when the prune interval has
// passed since the last prune (the first run starts the clock).
func TestSnapshotPrefixAndPrune(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	e.createBucket("b1", "", "")
	e.put("b1", "in/k1", "one", storage.ObjectMeta{})
	e.put("b1", "in/k2", "two", storage.ObjectMeta{})
	e.put("b1", "out", "three", storage.ObjectMeta{})
	e.configure("b1", `{"enabled":false,"prefix":"in/","prune":{"enabled":true,"interval":"1h"},"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("b1")
	e.checkSnapshot("b1", st.Snapshot, map[string]string{"in/k1": "one", "in/k2": "two"})
	if h, _ := e.manifest("b1", st.Snapshot); h.Prefix != "in/" || h.Keys != 2 || h.Bytes != 6 {
		t.Fatalf("manifest header %+v", h)
	}
	if n := restictest.Calls(t, e.log, "prune"); n != 0 {
		t.Fatalf("first run pruned (%d)", n)
	}
	first := e.svc.stateOf("b1").LastPruneAt
	if first.IsZero() {
		t.Fatal("first run did not start the prune clock")
	}
	e.svc.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	e.snapshot("b1")
	if n := restictest.Calls(t, e.log, "prune"); n != 1 {
		t.Fatalf("second run pruned %d times", n)
	}
	if !e.svc.stateOf("b1").LastPruneAt.After(first) {
		t.Fatal("prune not recorded")
	}
}
