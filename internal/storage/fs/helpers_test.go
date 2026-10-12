package fs

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

var ctx = context.Background()

// openTest opens a backend in dir (a new temp dir when "") and closes it at
// the end of the test.
func openTest(t testing.TB, dir string) *Backend {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	b, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Tests do not need durability across power loss; skipping the
	// metadata fsync keeps them fast. A few tests use openDurable.
	b.db.NoSync = true
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// openDurable is openTest with full durability (metadata and data fsync).
func openDurable(t testing.TB, dir string) *Backend {
	t.Helper()
	b, err := Open(dir, Options{FsyncData: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// waitRecovery waits for the background recovery pass to finish.
func waitRecovery(t testing.TB, b *Backend) {
	t.Helper()
	select {
	case <-b.recoveryDone:
	case <-time.After(30 * time.Second):
		t.Fatal("recovery did not finish")
	}
}

func newBucket(t testing.TB, versioning storage.VersioningStatus) (*Backend, string) {
	t.Helper()
	b := openTest(t, "")
	mkBucket(t, b, "bkt", versioning)
	return b, "bkt"
}

func mkBucket(t testing.TB, b *Backend, name string, versioning storage.VersioningStatus) {
	t.Helper()
	if err := b.CreateBucket(ctx, name, storage.CreateBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if versioning != storage.VersioningUnversioned {
		if err := b.SetBucketVersioning(ctx, name, versioning); err != nil {
			t.Fatal(err)
		}
	}
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func putOpts(t testing.TB, b storage.Backend, bucket, key, data string, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	t.Helper()
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	return w.Commit(ctx, storage.ObjectMeta{ETag: md5hex(data), ContentType: "text/plain"}, opts)
}

func put(t testing.TB, b storage.Backend, bucket, key, data string) storage.ObjectInfo {
	t.Helper()
	oi, err := putOpts(t, b, bucket, key, data, storage.CommitOptions{})
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return oi
}

func read(t testing.TB, b storage.Backend, bucket, key, vid string) string {
	t.Helper()
	_, r, err := b.OpenObject(ctx, bucket, key, vid)
	if err != nil {
		t.Fatalf("open %s@%s: %v", key, vid, err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type ver struct {
	key, id string
	dm      bool
	latest  bool
}

func versions(t testing.TB, b storage.Backend, bucket, prefix, km, vm string) []ver {
	t.Helper()
	var out []ver
	err := b.WalkVersions(ctx, bucket, prefix, km, vm, func(o storage.ObjectInfo) error {
		out = append(out, ver{o.Key, o.VersionID, o.DeleteMarker, o.IsLatest})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keysOf(t testing.TB, b storage.Backend, bucket, prefix, after string) []string {
	t.Helper()
	var out []string
	err := b.WalkObjects(ctx, bucket, prefix, after, func(o storage.ObjectInfo) error {
		if !o.IsLatest || o.DeleteMarker {
			t.Errorf("WalkObjects yielded non-current %+v", o)
		}
		out = append(out, o.Key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// listFiles returns the paths (relative to dir) of all regular files below dir.
func listFiles(t testing.TB, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func dataFiles(t testing.TB, b *Backend) []string { return listFiles(t, b.dataDir) }
func tmpFiles(t testing.TB, b *Backend) []string  { return listFiles(t, b.tmpDir) }

func eq[T comparable](t testing.TB, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func joinVers(vs []ver) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		s := v.key + "@" + v.id
		if v.dm {
			s += "(dm)"
		}
		if v.latest {
			s += "*"
		}
		parts[i] = s
	}
	return strings.Join(parts, " ")
}
