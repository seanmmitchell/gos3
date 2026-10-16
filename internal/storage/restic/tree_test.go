package restic

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// Tags and host of the test's tree snapshots.
const (
	tagTree  = "gos3-snapshot"
	treeHost = "gos3-snapshot"
)

var treeTags = []string{tagTree, BucketTag("bucket"), BucketIDTag("bid1")}

// treeGuard admits tree snapshots of the test's bucket only.
func treeGuard(s Snapshot) bool {
	return s.Host == treeHost && s.HasTag(tagTree) && s.HasTag(BucketIDTag("bid1")) && !s.HasTag(TagObject)
}

// writeTree writes files (by slash-separated path) below root.
func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// placeholders replaces the files (by slash-separated path) below root by
// placeholders of their size and modification time.
func placeholders(t *testing.T, root string, names []string) {
	t.Helper()
	for _, name := range names {
		p := filepath.Join(root, filepath.FromSlash(name))
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := MakePlaceholder(p, fi.Size(), fi.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
}

// dumpFile returns the content of the file name of snapshot.
func dumpFile(t *testing.T, rt *Runtime, loc Location, snapshot, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := rt.DumpFile(t.Context(), loc, snapshot, name, &buf); err != nil {
		t.Fatalf("dump %s: %v", name, err)
	}
	return buf.Bytes()
}

// checkFiles checks that snapshot holds files (by slash-separated path).
func checkFiles(t *testing.T, rt *Runtime, loc Location, snapshot string, files map[string][]byte) {
	t.Helper()
	for name, want := range files {
		if got := dumpFile(t, rt, loc, snapshot, "/"+name); !bytes.Equal(got, want) {
			t.Fatalf("%s of snapshot %s: %d bytes differ from the original %d", name, snapshot[:8], len(got), len(want))
		}
	}
}

// dumpTar returns the files and directories of the tar archive of the
// directory name of snapshot.
func dumpTar(t *testing.T, rt *Runtime, loc Location, snapshot, name string) (files map[string][]byte, dirs []string) {
	t.Helper()
	var buf bytes.Buffer
	if err := rt.DumpTar(t.Context(), loc, snapshot, name, &buf); err != nil {
		t.Fatalf("dump tar %s: %v", name, err)
	}
	files = map[string][]byte{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar of %s: %v", name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			dirs = append(dirs, hdr.Name)
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			files[hdr.Name] = data
		}
	}
	return files, dirs
}

// checkSummary compares the file counters of sum.
func checkSummary(t *testing.T, step string, sum Summary, filesNew, changed, unmodified int64) {
	t.Helper()
	if sum.FilesNew != filesNew || sum.FilesChanged != changed || sum.FilesUnmodified != unmodified ||
		sum.TotalFilesProcessed != filesNew+changed+unmodified || !ValidSnapshot(sum.SnapshotID) {
		t.Fatalf("%s: summary %+v, want %d new, %d changed, %d unmodified files", step, sum, filesNew, changed, unmodified)
	}
}

// randomBytes returns n random bytes.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// treeScenario backs up a staged tree three times, like whole-bucket
// snapshots do: in full; with every file replaced by a placeholder of its
// size and modification time (nothing may be read, and the snapshot must
// hold the original content); and with one file changed (and its
// modification time bumped), one added and one removed. It returns the
// snapshots' IDs and the last snapshot's files. strict checks counters
// that only the fake reports exactly.
func treeScenario(t *testing.T, rt *Runtime, loc Location, strict bool) ([3]string, map[string][]byte) {
	ctx := t.Context()
	stage := filepath.Join(t.TempDir(), "stage")
	files := map[string][]byte{
		"a.txt":            []byte("alpha"),
		"empty":            {},
		"big.bin":          randomBytes(3<<20 + 12345), // several restic chunks
		"sub/c.txt":        bytes.Repeat([]byte("c"), 70000),
		"sub/deeper/d.txt": []byte("delta"),
	}
	writeTree(t, stage, files)
	var total int64
	for _, data := range files {
		total += int64(len(data))
	}
	when := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	o := TreeOptions{Tags: treeTags, Host: treeHost, Time: when, IgnoreInode: true, IgnoreCtime: true}
	s1, err := rt.BackupTree(ctx, loc, stage, o)
	if err != nil {
		t.Fatal(err)
	}
	checkSummary(t, "full backup", s1, 5, 0, 0)
	if s1.TotalBytesProcessed != total || s1.DataAdded == 0 {
		t.Fatalf("full backup: summary %+v, want %d bytes processed", s1, total)
	}
	if strict && (s1.DirsNew != 2 || s1.DataAdded != total) {
		t.Fatalf("full backup: summary %+v", s1)
	}

	// Placeholders: nothing is read, the content is the parent's.
	placeholders(t, stage, slices.Collect(maps.Keys(files)))
	if got, err := os.ReadFile(filepath.Join(stage, "big.bin")); err != nil || len(bytes.Trim(got, "\x00")) != 0 {
		t.Fatalf("big.bin is not a zero placeholder (%v)", err)
	}
	o.Parent, o.Time = s1.SnapshotID, when.Add(time.Hour)
	s2, err := rt.BackupTree(ctx, loc, stage, o)
	if err != nil {
		t.Fatal(err)
	}
	checkSummary(t, "placeholder backup", s2, 0, 0, 5)
	if strict && (s2.DataAdded != 0 || s2.DirsUnmodified != 2) {
		t.Fatalf("placeholder backup: summary %+v", s2)
	}
	checkFiles(t, rt, loc, s2.SnapshotID, files)
	if d, err := rt.Diff(ctx, loc, s1.SnapshotID, s2.SnapshotID); err != nil || len(d) != 0 {
		t.Fatalf("diff of the full and the placeholder backup = %v, %v", d, err)
	}

	// One file changed, one added, one removed; the others stay
	// placeholders of the parent's files.
	a := filepath.Join(stage, "a.txt")
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	writeTree(t, stage, map[string][]byte{"a.txt": []byte("alpha, changed"), "new.txt": []byte("news")})
	if err := os.Chtimes(a, time.Time{}, fi.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(stage, "sub", "deeper", "d.txt")); err != nil {
		t.Fatal(err)
	}
	o.Parent, o.Time = s2.SnapshotID, when.Add(2*time.Hour)
	s3, err := rt.BackupTree(ctx, loc, stage, o)
	if err != nil {
		t.Fatal(err)
	}
	checkSummary(t, "changed backup", s3, 1, 1, 3)
	want := maps.Clone(files)
	want["a.txt"], want["new.txt"] = []byte("alpha, changed"), []byte("news")
	delete(want, "sub/deeper/d.txt")
	checkFiles(t, rt, loc, s3.SnapshotID, want)
	d, err := rt.Diff(ctx, loc, s2.SnapshotID, s3.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(d, func(x, y DiffEntry) int { return strings.Compare(x.Path, y.Path) })
	wantDiff := []DiffEntry{{"/a.txt", "M"}, {"/new.txt", "+"}, {"/sub/deeper/d.txt", "-"}}
	if !slices.Equal(d, wantDiff) {
		t.Fatalf("diff = %v, want %v", d, wantDiff)
	}

	// Tar archives of a directory and of the whole snapshot.
	sub, dirs := dumpTar(t, rt, loc, s3.SnapshotID, "/sub")
	if len(sub) != 1 || !bytes.Equal(sub["sub/c.txt"], want["sub/c.txt"]) || !slices.Contains(dirs, "sub/deeper/") {
		t.Fatalf("tar of /sub: files %v, directories %v", slices.Collect(maps.Keys(sub)), dirs)
	}
	all, _ := dumpTar(t, rt, loc, s3.SnapshotID, "/")
	if len(all) != len(want) {
		t.Fatalf("tar of the snapshot: files %v", slices.Collect(maps.Keys(all)))
	}
	for name, data := range want {
		if !bytes.Equal(all[name], data) {
			t.Fatalf("tar of the snapshot: %s differs", name)
		}
	}

	// The listing.
	snaps, err := rt.Snapshots(ctx, loc, SnapshotFilter{Host: treeHost, Tags: treeTags[:2]})
	if err != nil {
		t.Fatal(err)
	}
	ids := [3]string{s1.SnapshotID, s2.SnapshotID, s3.SnapshotID}
	if len(snaps) != 3 {
		t.Fatalf("listed %d snapshots, want 3", len(snaps))
	}
	for i, s := range snaps {
		parent := ""
		if i > 0 {
			parent = ids[i-1]
		}
		if s.ID != ids[i] || !strings.HasPrefix(s.ID, s.ShortID) || s.ShortID == "" || !s.Time.Equal(when.Add(time.Duration(i)*time.Hour)) ||
			s.Host != treeHost || s.Parent != parent || len(s.Paths) != 1 || !treeGuard(s) {
			t.Fatalf("snapshot %d: %+v", i, s)
		}
		if s.Summary == nil || s.Summary.SnapshotID != s.ID || s.Summary.TotalFilesProcessed != 5 {
			t.Fatalf("snapshot %d summary: %+v", i, s.Summary)
		}
	}
	return ids, want
}

// retentionScenario archives an object next to the tree snapshots ids
// (whose last holds files), checks the listing's filters and forgets the
// first two tree snapshots, refusing the object's, then prunes.
func retentionScenario(t *testing.T, rt *Runtime, loc Location, ids [3]string, files map[string][]byte) {
	ctx := t.Context()
	a := rt.NewArchiverWith(ctx, loc, ArchiveOptions{Bucket: "bucket", BucketID: "bid1", Key: "k"})
	if _, err := io.WriteString(a, "object data"); err != nil {
		t.Fatal(err)
	}
	object, err := a.Finish(nil)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := rt.Snapshots(ctx, loc, SnapshotFilter{Tags: []string{TagObject, BucketIDTag("bid1")}})
	if err != nil || len(objects) != 1 || objects[0].ID != object || !objects[0].HasTag(KeyTag("k")) || !objects[0].HasTag(BucketTag("bucket")) || objects[0].Host != "gos3" {
		t.Fatalf("object snapshots = %+v, %v", objects, err)
	}
	if none, err := rt.Snapshots(ctx, loc, SnapshotFilter{Tags: []string{TagObject, tagTree}}); err != nil || len(none) != 0 {
		t.Fatalf("tags must all match: %+v, %v", none, err)
	}
	if hosts, err := rt.Snapshots(ctx, loc, SnapshotFilter{Host: "gos3"}); err != nil || len(hosts) != 1 || hosts[0].ID != object {
		t.Fatalf("host filter: %+v, %v", hosts, err)
	}
	missing := strings.Repeat("0", 64)
	byID, err := rt.Snapshots(ctx, loc, SnapshotFilter{IDs: []string{ids[2], missing, ids[0]}})
	if err != nil || len(byID) != 2 || byID[0].ID != ids[0] || byID[1].ID != ids[2] {
		t.Fatalf("snapshots by ID: %+v, %v", byID, err)
	}

	count := func() int {
		all, err := rt.Snapshots(ctx, loc, SnapshotFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return len(all)
	}
	if err := rt.Forget(ctx, loc, []string{ids[0], object}, treeGuard); !errors.Is(err, ErrForgetRefused) || !strings.Contains(err.Error(), object) {
		t.Fatalf("forget of an object snapshot: %v", err)
	}
	if err := rt.Forget(ctx, loc, []string{ids[0], missing}, treeGuard); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("forget of a missing snapshot: %v", err)
	}
	if err := rt.Forget(ctx, loc, []string{ids[0]}, nil); err == nil {
		t.Fatal("forget without a guard")
	}
	if err := rt.Forget(ctx, loc, []string{ids[0][:8]}, treeGuard); err == nil {
		t.Fatal("forget of a short ID")
	}
	if n := count(); n != 4 {
		t.Fatalf("%d snapshots after refused forgets, want 4", n)
	}
	if err := rt.Forget(ctx, loc, []string{ids[0], ids[1], ids[0]}, treeGuard); err != nil {
		t.Fatal(err)
	}
	if err := rt.Forget(ctx, loc, nil, treeGuard); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("%d snapshots after forget, want 2", n)
	}
	if err := rt.Prune(ctx, loc, PruneOptions{MaxUnused: "0", RetryLock: time.Minute}); err != nil {
		t.Fatal(err)
	}
	// The last snapshot keeps the content it took from the forgotten ones.
	checkFiles(t, rt, loc, ids[2], files)
	if got := readAll(t, rt, loc, object, int64(len("object data"))); got != "object data" {
		t.Fatalf("object after prune = %q", got)
	}
}

func TestTreeSnapshots(t *testing.T) {
	for _, kind := range repoKinds {
		t.Run(kind.name, func(t *testing.T) {
			log := restictest.Install(t, "current")
			loc, _ := kind.location(t)
			rt := &Runtime{TempDir: t.TempDir()}
			if err := rt.Prepare(t.Context(), "bid1", loc, &memRecords{}); err != nil {
				t.Fatal(err)
			}
			ids, files := treeScenario(t, rt, loc, true)
			retentionScenario(t, rt, loc, ids, files)
			if n := restictest.Calls(t, log, "forget"); n != 1 {
				t.Fatalf("forget ran %d times", n)
			}
			if n := restictest.Calls(t, log, "prune"); n != 1 {
				t.Fatalf("prune ran %d times", n)
			}
		})
	}
}

// TestRealResticTreeSnapshots runs the tree scenarios against the real
// restic (skipped when it is not on PATH). It is the spike gating
// incremental whole-bucket snapshots: a tree whose files were replaced by
// sparse placeholders of the same size and modification time backs up
// without reading them (--parent --ignore-inode --ignore-ctime), the new
// snapshot holds the original content, and a "."-relative backup, diff
// --json and dump --archive tar work.
func TestRealResticTreeSnapshots(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("install restic >= 0.17.1 to run integration")
	}
	for _, kind := range repoKinds {
		t.Run(kind.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			loc, _ := kind.location(t)
			rt := &Runtime{TempDir: t.TempDir()}
			if err := rt.PrepareWith(ctx, "bid1", "bucket", loc, &memRecords{}); err != nil {
				t.Fatal(err)
			}
			ids, files := treeScenario(t, rt, loc, false)
			retentionScenario(t, rt, loc, ids, files)
		})
	}
}

// A restic that reads the placeholders anyway reports them changed (and
// stores zeros): the condition whole-bucket snapshots verify.
func TestTreeReread(t *testing.T) {
	restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir()}
	if err := rt.Prepare(t.Context(), "bid1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	files := map[string][]byte{"a": []byte("alpha"), "b": []byte("beta"), "e": {}}
	writeTree(t, stage, files)
	o := TreeOptions{IgnoreInode: true, IgnoreCtime: true}
	s1, err := rt.BackupTree(t.Context(), loc, stage, o)
	if err != nil {
		t.Fatal(err)
	}
	placeholders(t, stage, []string{"a", "b", "e"})
	t.Setenv(restictest.EnvReread, "1")
	o.Parent = s1.SnapshotID
	s2, err := rt.BackupTree(t.Context(), loc, stage, o)
	if err != nil {
		t.Fatal(err)
	}
	checkSummary(t, "reread", s2, 0, 2, 1)
	if got := dumpFile(t, rt, loc, s2.SnapshotID, "/a"); !bytes.Equal(got, make([]byte, 5)) {
		t.Fatalf("reread placeholder = %q", got)
	}
	// Without both ignore options the fake reads every file too.
	t.Setenv(restictest.EnvReread, "")
	writeTree(t, stage, files)
	s3, err := rt.BackupTree(t.Context(), loc, stage, TreeOptions{Parent: s1.SnapshotID, IgnoreInode: true})
	if err != nil {
		t.Fatal(err)
	}
	if s3.FilesNew != 0 || s3.FilesUnmodified+s3.FilesChanged != 3 || s3.DataAdded != 0 {
		t.Fatalf("backup without --ignore-ctime: %+v", s3)
	}
}

// An incomplete snapshot is reported as a failure that names it.
func TestTreeIncomplete(t *testing.T) {
	restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir()}
	if err := rt.Prepare(t.Context(), "bid1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	writeTree(t, stage, map[string][]byte{"a": []byte("alpha")})
	t.Setenv(restictest.EnvIncomplete, "1")
	sum, err := rt.BackupTree(t.Context(), loc, stage, TreeOptions{})
	if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("incomplete backup: %v", err)
	}
	if !ValidSnapshot(sum.SnapshotID) || sum.FilesNew != 1 {
		t.Fatalf("incomplete backup summary: %+v", sum)
	}
	if err := rt.Forget(t.Context(), loc, []string{sum.SnapshotID}, func(Snapshot) bool { return true }); err != nil {
		t.Fatal(err)
	}
	t.Setenv(restictest.EnvFail, "backup")
	if sum, err := rt.BackupTree(t.Context(), loc, stage, TreeOptions{}); !errors.Is(err, storage.ErrEncryptionUnavailable) ||
		strings.Contains(err.Error(), password) || sum.SnapshotID != "" {
		t.Fatalf("failed backup: %+v %v", sum, err)
	}
}

// Invalid arguments are refused before restic runs.
func TestTreeArguments(t *testing.T) {
	log := restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir()}
	ctx := t.Context()
	stage := t.TempDir()
	file := filepath.Join(stage, "file")
	writeTree(t, stage, map[string][]byte{"file": []byte("x")})
	id := strings.Repeat("ab", 32)
	for name, err := range map[string]error{
		"missing dir": func() error {
			_, err := rt.BackupTree(ctx, loc, filepath.Join(stage, "nope"), TreeOptions{})
			return err
		}(),
		"file as dir": func() error { _, err := rt.BackupTree(ctx, loc, file, TreeOptions{}); return err }(),
		"short parent": func() error {
			_, err := rt.BackupTree(ctx, loc, stage, TreeOptions{Parent: id[:8]})
			return err
		}(),
		"comma tag":      func() error { _, err := rt.BackupTree(ctx, loc, stage, TreeOptions{Tags: []string{"a,b"}}); return err }(),
		"space tag":      func() error { _, err := rt.BackupTree(ctx, loc, stage, TreeOptions{Tags: []string{"a b"}}); return err }(),
		"control host":   func() error { _, err := rt.BackupTree(ctx, loc, stage, TreeOptions{Host: "a\nb"}); return err }(),
		"filter tag":     func() error { _, err := rt.Snapshots(ctx, loc, SnapshotFilter{Tags: []string{""}}); return err }(),
		"filter ID":      func() error { _, err := rt.Snapshots(ctx, loc, SnapshotFilter{IDs: []string{"latest"}}); return err }(),
		"diff ID":        func() error { _, err := rt.Diff(ctx, loc, id, "latest"); return err }(),
		"dump root file": rt.DumpFile(ctx, loc, id, "/", io.Discard),
		"dump relative":  rt.DumpFile(ctx, loc, id, "a/b", io.Discard),
		"dump unclean":   rt.DumpTar(ctx, loc, id, "/a/../b", io.Discard),
		"dump trailing":  rt.DumpTar(ctx, loc, id, "/a/", io.Discard),
		"dump ID":        rt.DumpTar(ctx, loc, "latest", "/", io.Discard),
		"max unused":     rt.Prune(ctx, loc, PruneOptions{MaxUnused: "--dry-run"}),
		"negative wait":  rt.Prune(ctx, loc, PruneOptions{RetryLock: -time.Second}),
		"forget ID":      rt.Forget(ctx, loc, []string{"latest"}, treeGuard),
		"bucket ID tag": func() error {
			a := rt.NewArchiverWith(ctx, loc, ArchiveOptions{Bucket: "b", BucketID: "a,b", Key: "k"})
			_, err := a.Write([]byte("x"))
			a.Abort()
			return err
		}(),
	} {
		if err == nil || errors.Is(err, storage.ErrEncryptionUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(raw) != 0 {
		t.Fatalf("restic ran: %q", raw)
	}
}

// failingWriter fails every write.
type failingWriter struct{}

var errDestination = errors.New("client went away")

func (failingWriter) Write([]byte) (int, error) { return 0, errDestination }

// A failing destination is reported as such, not as a restic failure.
func TestDumpDestinationFails(t *testing.T) {
	restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir()}
	if err := rt.Prepare(t.Context(), "bid1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	writeTree(t, stage, map[string][]byte{"big": randomBytes(1 << 20)})
	sum, err := rt.BackupTree(t.Context(), loc, stage, TreeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, dump := range []func(io.Writer) error{
		func(w io.Writer) error { return rt.DumpFile(t.Context(), loc, sum.SnapshotID, "/big", w) },
		func(w io.Writer) error { return rt.DumpTar(t.Context(), loc, sum.SnapshotID, "/", w) },
	} {
		if err := dump(failingWriter{}); !errors.Is(err, errDestination) || errors.Is(err, storage.ErrEncryptionUnavailable) {
			t.Fatalf("dump to a failing writer: %v", err)
		}
	}
	err = rt.DumpFile(t.Context(), loc, sum.SnapshotID, "/missing", io.Discard)
	if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("dump of a missing file: %v", err)
	}
}

// The bucket name is recorded; preparing again with another name only
// rewrites the record.
func TestPrepareWithBucket(t *testing.T) {
	log := restictest.Install(t, "current")
	loc, _ := localRepo(t)
	records := &memRecords{}
	rt := &Runtime{}
	if err := rt.PrepareWith(t.Context(), "bid1", "bucket", loc, records); err != nil {
		t.Fatal(err)
	}
	if rec := records.m["bid1"]; !rec.Initialized || rec.Bucket != "bucket" || rec.Repository != loc.Repository {
		t.Fatalf("record = %+v", rec)
	}
	stores := records.stores
	if err := rt.PrepareWith(t.Context(), "bid1", "bucket", loc, records); err != nil || records.stores != stores {
		t.Fatalf("prepared again: %v, %d record writes", err, records.stores-stores)
	}
	if err := rt.PrepareWith(t.Context(), "bid1", "renamed", loc, records); err != nil {
		t.Fatal(err)
	}
	if rec := records.m["bid1"]; !rec.Initialized || rec.Bucket != "renamed" {
		t.Fatalf("record after rename = %+v", rec)
	}
	// A restarted server preparing without a name keeps the recorded one.
	if err := new(Runtime).Prepare(t.Context(), "bid1", loc, records); err != nil {
		t.Fatal(err)
	}
	if rec := records.m["bid1"]; !rec.Initialized || rec.Bucket != "renamed" {
		t.Fatalf("record after Prepare = %+v", rec)
	}
	if n := restictest.Calls(t, log, "cat config"); n != 2 {
		t.Fatalf("cat config ran %d times", n)
	}
	raw, err := json.Marshal(records.m["bid1"])
	if err != nil || string(raw) != `{"initialized":true,"repository":`+mustJSON(t, loc.Repository)+`,"bucket":"renamed"}` {
		t.Fatalf("record JSON = %s", raw)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Prune waits for locks as long as asked.
func TestPruneRetryLock(t *testing.T) {
	restictest.Install(t, "current")
	rt := &Runtime{}
	loc := Location{Repository: "/repo", Password: "pw"}
	cmd, err := rt.command(t.Context(), loc, cmdOptions{retryLock: 5 * time.Minute}, "prune")
	if err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(cmd.Args[1:], " "); !strings.Contains(args, "--retry-lock 5m0s ") {
		t.Fatalf("args = %q", args)
	}
}

// Restic's JSON records decode into Summary and Snapshot (samples of
// restic 0.17's output).
func TestDecodeResticJSON(t *testing.T) {
	id := strings.Repeat("1f", 32)
	parent := strings.Repeat("2e", 32)
	var out snapshotOutput
	backup := `{"message_type":"status","seconds_elapsed":1,"percent_done":0.5,"total_files":5,"files_done":2,"total_bytes":100,"bytes_done":50,"current_files":["/a"]}
{"message_type":"verbose_status","action":"unchanged","item":"/a","duration":0,"data_size":0,"data_size_in_repo":0,"metadata_size":0,"metadata_size_in_repo":0,"total_files":0}
{"message_type":"summary","files_new":1,"files_changed":2,"files_unmodified":3,"dirs_new":4,"dirs_changed":5,"dirs_unmodified":6,"data_blobs":7,"tree_blobs":8,"data_added":9,"data_added_packed":10,"total_files_processed":11,"total_bytes_processed":12,"total_duration":0.5,"backup_start":"2024-06-15T12:00:00.1+02:00","backup_end":"2024-06-15T12:00:00.6+02:00","snapshot_id":"` + id + `"}`
	if _, err := out.Write([]byte(backup)); err != nil {
		t.Fatal(err)
	}
	if err := out.consume(); err != nil {
		t.Fatal(err)
	}
	want := Summary{FilesNew: 1, FilesChanged: 2, FilesUnmodified: 3, DirsNew: 4, DirsChanged: 5, DirsUnmodified: 6,
		DataAdded: 9, TotalFilesProcessed: 11, TotalBytesProcessed: 12, SnapshotID: id}
	if out.snapshot != id || out.summary != want {
		t.Fatalf("summary = %+v (%q)", out.summary, out.snapshot)
	}

	list := `[{"time":"2024-06-15T14:00:00.123456789+02:00","parent":"` + parent + `","tree":"` + parent + `","paths":["/srv/stage"],"hostname":"gos3-snapshot","username":"gos3","uid":1000,"gid":1000,"tags":["gos3-snapshot","gos3-bucket:YnVja2V0"],"program_version":"restic 0.17.3","summary":{"backup_start":"2024-06-15T14:00:01+02:00","backup_end":"2024-06-15T14:00:02+02:00","files_new":5,"files_changed":0,"files_unmodified":0,"dirs_new":2,"dirs_changed":0,"dirs_unmodified":0,"data_blobs":6,"tree_blobs":3,"data_added":300,"data_added_packed":200,"total_files_processed":5,"total_bytes_processed":300},"id":"` + id + `","short_id":"1f1f1f1f"},
{"time":"2023-01-01T00:00:00Z","tree":"` + parent + `","paths":["/object"],"hostname":"gos3","id":"` + parent + `","short_id":"2e2e2e2e"}]`
	var listed []resticSnapshot
	if err := json.Unmarshal([]byte(list), &listed); err != nil {
		t.Fatal(err)
	}
	s := listed[0].snapshot()
	if s.ID != id || s.ShortID != "1f1f1f1f" || !s.Time.Equal(time.Date(2024, 6, 15, 12, 0, 0, 123456789, time.UTC)) ||
		s.Host != "gos3-snapshot" || s.Parent != parent || !slices.Equal(s.Paths, []string{"/srv/stage"}) ||
		!s.HasTag(BucketTag("bucket")) || s.Summary == nil ||
		*s.Summary != (Summary{FilesNew: 5, DirsNew: 2, DataAdded: 300, TotalFilesProcessed: 5, TotalBytesProcessed: 300, SnapshotID: id}) {
		t.Fatalf("snapshot = %+v (summary %+v)", s, s.Summary)
	}
	if old := listed[1].snapshot(); old.Summary != nil || len(old.Tags) != 0 || old.Parent != "" {
		t.Fatalf("snapshot without summary = %+v", old)
	}
}

func TestTags(t *testing.T) {
	if BucketTag("bucket") != "gos3-bucket:YnVja2V0" || KeyTag("a/b") != "gos3-key:YS9i" || BucketIDTag("x1") != "gos3-bucket-id:x1" {
		t.Fatal("tag encoding changed: existing archives would not be found")
	}
	for tag, want := range map[string]bool{"gos3-object": true, "a:b=c/d": true, "": false, "a,b": false, "a b": false, "a\tb": false, "a\x00": false} {
		if ValidTag(tag) != want {
			t.Fatalf("ValidTag(%q) = %v", tag, !want)
		}
	}
}
