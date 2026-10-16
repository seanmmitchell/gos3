package resticsnap

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// Regressions found in review (the first five are its reproductions).

// A rebuilt stage (its stamp lost) with partial snapshots: the unchanged
// keys' placeholders are in every backup of the run, so none is read as
// zeros.
func TestRegressRebuiltStagePartials(t *testing.T) {
	for _, changed := range [][]string{{"k1", "k3", "k5"}, {"k1", "k2"}} {
		e := newEnv(t, "mem", Options{Incremental: true})
		e.createBucket("b1", "", "")
		want := map[string]string{"k1": "one", "k2": "two", "k3": "three", "k4": "four", "k5": "five", "k6": "six"}
		for k, v := range want {
			e.put("b1", k, v, storage.ObjectMeta{})
		}
		e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
		e.snapshot("b1")
		if err := os.RemoveAll(e.svc.stageDir); err != nil {
			t.Fatal(err)
		}
		for _, k := range changed {
			want[k] = "8 bytes" + k[1:]
			e.put("b1", k, want[k], storage.ObjectMeta{})
		}
		e.svc.spool = 10
		st := e.snapshot("b1")
		if st.Materialized != int64(len(changed)) || st.Partials != len(changed)-1 {
			t.Fatalf("changed %v: status %+v", changed, st)
		}
		e.checkSnapshot("b1", st.Snapshot, want)
	}
}

// A snapshot that failed its verification and could not be forgotten is
// tagged partial: the next run forgets it and does not use it as parent.
func TestRegressUnverifiedNotParent(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	m, err := jobs.Open(jobs.Options{
		Runners: map[string]jobs.Runner{KindSnapshot: e.svc.Runner(KindSnapshot)},
		Logger:  slog.New(slog.DiscardHandler), RetryMin: time.Hour, RetryMax: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	e.svc.mu.Lock()
	e.svc.mgr = m
	e.svc.mu.Unlock()
	e.mgr = m

	e.createBucket("v1", "", "")
	want := map[string]string{"k1": "one", "k2": "two", "d/k3": "three", "k4": "four"}
	for k, v := range want {
		e.put("v1", k, v, storage.ObjectMeta{})
	}
	e.configure("v1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	first := e.snapshot("v1").Snapshot

	t.Setenv(restictest.EnvReread, "1")
	t.Setenv(restictest.EnvFail, "forget")
	want["k2"] = "two, changed"
	e.put("v1", "k2", want["k2"], storage.ObjectMeta{})
	j, err := e.svc.RunNow(context.Background(), "v1", "test")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if j, _ = m.Get(j.ID); j.State == jobs.StateWaiting || j.State.Finished() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
	}
	if j.State != jobs.StateWaiting || !strings.Contains(j.LastError, "forget") {
		t.Fatalf("first job %s: %s", j.State, j.LastError)
	}
	if err := m.Cancel(j.ID); err != nil {
		t.Fatal(err)
	}
	t.Setenv(restictest.EnvReread, "")
	t.Setenv(restictest.EnvFail, "")
	// The unverified snapshot is there, but tagged partial.
	all, err := e.svc.rt.Snapshots(context.Background(), e.location("v1"), restic.SnapshotFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("repository holds %+v (%v)", all, err)
	}
	for _, sn := range all {
		if sn.ID != first && !sn.HasTag(TagPartial) {
			t.Fatalf("unverified snapshot %+v is not tagged partial", sn)
		}
	}
	if listed, err := e.svc.Snapshots(context.Background(), "v1"); err != nil || len(listed) != 2 || listed[1].ID != first {
		t.Fatalf("Snapshots = %+v, %v", listed, err)
	}

	st := e.snapshot("v1")
	if st.Forgotten != 1 || st.Materialized != 1 {
		t.Fatalf("status %+v", st)
	}
	e.checkSnapshot("v1", st.Snapshot, want)
}

// A snapshot recorded as unverified (its promotion failed and could not be
// forgotten) is not listed, not restored, not a parent, and forgotten by
// the next run.
func TestRegressUnverifiedRecorded(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("u1", "", "")
	e.put("u1", "k", "data", storage.ObjectMeta{})
	e.configure("u1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	bad := e.snapshot("u1").Snapshot
	e.svc.updateState("u1", func(st *bucketState) { st.Unverified = []string{bad} })
	e.svc.invalidate("u1")
	if listed, err := e.svc.Snapshots(ctx, "u1"); err != nil || len(listed) != 0 {
		t.Fatalf("Snapshots = %+v, %v", listed, err)
	}
	if _, err := e.svc.Restore(ctx, "u1", bad, json.RawMessage(`{"target":{"bucket":"dst","create":{}}}`), "x"); !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Fatalf("restore of an unverified snapshot: %v", err)
	}
	st := e.snapshot("u1")
	if st.Forgotten != 1 || st.Materialized != 1 {
		t.Fatalf("status %+v (the unverified snapshot was a parent?)", st)
	}
	if u := e.svc.stateOf("u1").Unverified; len(u) != 0 {
		t.Fatalf("unverified %v", u)
	}
	e.checkSnapshot("u1", st.Snapshot, map[string]string{"k": "data"})
}

// A full run reads every object: each file's time differs from the
// parent's, so restic cannot take the parent's content.
func TestRegressFullRunReads(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	e.createBucket("b1", "", "")
	want := map[string]string{"k1": "one", "k2": "two", "k3": "three"}
	for k, v := range want {
		e.put("b1", k, v, storage.ObjectMeta{})
	}
	e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	prev := e.snapshot("b1")
	e.svc.incremental = false
	for range 2 {
		st := e.snapshot("b1")
		if st.Materialized != 3 {
			t.Fatalf("status %+v", st)
		}
		_, before := e.manifest("b1", prev.Snapshot)
		entries := e.checkSnapshot("b1", st.Snapshot, want)
		for i, en := range entries {
			if en.MTime == before[i].MTime {
				t.Fatalf("%s keeps the parent's time %d", en.Key, en.MTime)
			}
		}
		snaps, err := e.svc.Snapshots(context.Background(), "b1")
		if err != nil || snaps[0].Summary == nil || snaps[0].Summary.FilesUnmodified != 0 {
			t.Fatalf("full run summary %+v (%v)", snaps[0].Summary, err)
		}
		prev = st
	}
}

// A failed run leaves no object data or listing in the stage.
func TestRegressNoPlaintextLeft(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	e.createBucket("b1", "", "")
	e.put("b1", "secret", "top secret content", storage.ObjectMeta{})
	e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	t.Setenv(restictest.EnvFail, "backup")
	j, err := e.svc.RunNow(context.Background(), "b1", "test")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(5 * time.Millisecond) {
		if j, _ = e.mgr.Get(j.ID); j.Failures > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout")
		}
	}
	_ = e.mgr.Cancel(j.ID)
	e.wait(j.ID)
	checkNoSecret(t, e.svc.stageDir, "top secret")
}

// checkNoSecret fails when a file below dir holds secret, or a work
// directory is left.
func checkNoSecret(t *testing.T, dir, secret string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "work" {
				t.Errorf("work directory %s left", p)
			}
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), secret) {
			t.Errorf("%s holds the object's content", p)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// The stage of a bucket goes with its snapshot configuration, and with the
// bucket.
func TestRegressStageRemoved(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	for _, b := range []string{"b1", "b2"} {
		e.createBucket(b, "", "")
		e.put(b, "k", "data", storage.ObjectMeta{})
		e.configure(b, `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
		e.snapshot(b)
	}
	stage1, stage2 := e.svc.stateOf("b1").Stage, e.svc.stateOf("b2").Stage
	for _, d := range []string{stage1, stage2} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("stage %s: %v", d, err)
		}
	}
	if err := e.svc.DeleteConfig(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage1); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stage of b1 after DeleteConfig: %v", err)
	}
	// A deleted bucket: the router's notification marks it, the next tick
	// removes its stage.
	if _, err := e.router.DeleteObject(ctx, "b2", "k", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.router.DeleteBucket(ctx, "b2"); err != nil {
		t.Fatal(err)
	}
	e.svc.onChange(multi.Change{Kind: multi.ChangeBucketDeleted, Bucket: "b2"})
	e.svc.mu.Lock()
	e.svc.loadedAt = e.svc.now() // not a full reload
	e.svc.mu.Unlock()
	e.svc.tick(ctx)
	if _, err := os.Stat(stage2); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stage of a deleted bucket: %v", err)
	}
	// Its snapshots stay listed.
	if snaps, err := e.svc.Snapshots(ctx, "b2"); err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots of the deleted bucket %+v, %v", snaps, err)
	}
}

// Without a system directory the stage is a private temporary directory,
// removed by Stop.
func TestRegressTempStage(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	svc, err := New(Options{Router: e.router, Incremental: true, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if svc.stageDir != "" {
		t.Fatalf("stage %q before first use", svc.stageDir)
	}
	root, err := svc.ensureStageRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(root), "gos3-restic-stage-") || filepath.Base(root) == "gos3-restic-stage-" {
		t.Fatalf("stage root %s", root)
	}
	fi, err := os.Lstat(root)
	if err != nil || !fi.IsDir() {
		t.Fatalf("stage root: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Fatalf("stage root mode %v", fi.Mode().Perm())
	}
	again, err := svc.ensureStageRoot()
	if err != nil || again != root {
		t.Fatalf("second ensureStageRoot %s, %v", again, err)
	}
	svc.Stop()
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stage root after Stop: %v", err)
	}
}

// A stage root that is not a directory is refused.
func TestRegressStageRootChecked(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Options{Router: e.router, StageDir: file, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ensureStageRoot(); err == nil {
		t.Fatal("a file as the stage root")
	}
	if runtime.GOOS != "windows" {
		loose := filepath.Join(t.TempDir(), "loose")
		if err := os.Mkdir(loose, 0o755); err != nil {
			t.Fatal(err)
		}
		svc, err := New(Options{Router: e.router, StageDir: loose, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ensureStageRoot(); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(loose); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("loose stage root not tightened: %v %v", fi.Mode(), err)
		}
	}
}

// A defaulted prune follows the bucket's archive scope when the run
// prunes; an explicit one does not.
func TestRegressPruneDefaultScope(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	for _, b := range []string{"def", "explicit"} {
		e.createBucket(b, "", "")
		e.put(b, "k", "v", storage.ObjectMeta{})
	}
	e.configure("def", `{"enabled":false,"prune":{"interval":"1h"},"target":{"kind":"backing","backing":"disk"}}`)
	e.configure("explicit", `{"enabled":false,"prune":{"enabled":true,"interval":"1h"},"target":{"kind":"backing","backing":"disk"}}`)
	e.snapshot("def")
	e.snapshot("explicit")
	// The archive scope becomes "all" after the configuration was stored.
	for _, b := range []string{"def", "explicit"} {
		if err := e.router.PutBucketConfig(ctx, b, storage.ConfigEncryptionProvider, []byte(`{"provider":"restic","scope":"all"}`)); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	e.snapshot("def")
	if n := restictest.Calls(t, e.log, "prune"); n != 0 {
		t.Fatalf("defaulted prune ran %d times with scope all", n)
	}
	e.snapshot("explicit")
	if n := restictest.Calls(t, e.log, "prune"); n != 1 {
		t.Fatalf("explicit prune ran %d times", n)
	}
	// A stored defaulted configuration keeps saying so.
	c, err := e.svc.loadConfig(ctx, "def")
	if err != nil || !c.Prune.Default || !c.Prune.Enabled {
		t.Fatalf("stored %+v, %v", c.Prune, err)
	}
	// "default": true keeps following the scope (now off).
	c2 := e.configure("def", `{"enabled":false,"prune":{"enabled":true,"default":true},"target":{"kind":"backing","backing":"disk"}}`)
	if !c2.Prune.Default || c2.Prune.Enabled {
		t.Fatalf("defaulted PUT %+v", c2.Prune)
	}
}

// A restore records its progress synchronously in its state directory.
func TestRegressRestoreDurableCheckpoint(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	dir := t.TempDir()
	m, err := jobs.Open(jobs.Options{
		Dir:     dir,
		Runners: map[string]jobs.Runner{KindSnapshot: e.svc.Runner(KindSnapshot), KindRestore: e.svc.Runner(KindRestore)},
		Logger:  slog.New(slog.DiscardHandler), RetryMin: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	e.svc.mu.Lock()
	e.svc.mgr = m
	e.svc.mu.Unlock()
	e.mgr = m
	e.createBucket("src", "", "")
	e.put("src", "k", "v", storage.ObjectMeta{})
	e.configure("src", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("src")
	j := e.restore("src", st.Snapshot, `{"target":{"bucket":"dst","create":{}}}`)
	if j.State != jobs.StateSucceeded {
		t.Fatalf("restore: %s", j.LastError)
	}
	raw, err := os.ReadFile(filepath.Join(dir, j.ID, durableCheckpointName))
	if err != nil {
		t.Fatal(err)
	}
	var cp restoreCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil || !cp.Created || !cp.Started {
		t.Fatalf("durable checkpoint %s (%v)", raw, err)
	}
	if data, _ := e.read("dst", "k"); data != "v" {
		t.Fatalf("restored %q", data)
	}
}

// A promoted snapshot is listed with the summary of the backup the run
// verified, without the summary tag.
func TestRegressPromotedSummary(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	e.createBucket("b1", "", "")
	e.put("b1", "k1", "one", storage.ObjectMeta{})
	e.put("b1", "k2", "two", storage.ObjectMeta{})
	e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	e.snapshot("b1")
	e.put("b1", "k2", "two, changed", storage.ObjectMeta{})
	e.snapshot("b1")
	snaps, err := e.svc.Snapshots(context.Background(), "b1")
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots %+v, %v", snaps, err)
	}
	first, second := snaps[1].Summary, snaps[0].Summary
	if first == nil || first.FilesNew != 4 || second == nil || second.FilesChanged != 3 || second.FilesUnmodified != 1 ||
		second.SnapshotID != snaps[0].ID || second.TotalFilesProcessed != 4 {
		t.Fatalf("summaries %+v, %+v", first, second)
	}
	for _, sn := range snaps {
		if slices.ContainsFunc(sn.Tags, func(t string) bool { return strings.HasPrefix(t, tagSummaryPrefix) }) {
			t.Fatalf("tags %v", sn.Tags)
		}
	}
	all, err := e.svc.rt.Snapshots(context.Background(), e.location("b1"), restic.SnapshotFilter{})
	if err != nil || len(all) != 2 || all[1].Summary.FilesUnmodified != 4 {
		t.Fatalf("restic's own summary of the promotion %+v (%v)", all, err)
	}
}
