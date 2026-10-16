package fs

import (
	"bytes"
	"encoding/json"
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// wantNoRepository fails unless err says that the repository does not
// exist yet (errors.ErrUnsupported and fs.ErrNotExist).
func wantNoRepository(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("%s = %v, want no repository yet", what, err)
	}
}

func TestResticHostBucket(t *testing.T) {
	log := installFakeRestic(t, "host")
	b := openTest(t, "")
	mkBucket(t, b, "arc", storage.VersioningEnabled)
	id := mustBucketID(t, b, "arc")
	if _, _, err := b.ResticRepository(ctx, "nope", true); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("unknown bucket = %v", err)
	}
	// Without create nothing is created.
	_, _, err := b.ResticRepository(ctx, "arc", false)
	wantNoRepository(t, "lookup before create", err)
	if _, err := os.Stat(filepath.Join(b.sysDir, keysDirName, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a lookup created the bucket's key: %v", err)
	}

	info, loc, err := b.ResticRepository(ctx, "arc", true)
	if err != nil {
		t.Fatal(err)
	}
	key, err := b.bucketKey(id, false)
	if err != nil {
		t.Fatal(err)
	}
	record := b.recoveryPath(id)
	if info.BucketID != id || info.Repository != b.resticRepository(id) || loc.Repository != info.Repository ||
		!info.Initialized || info.RecordLocation != record ||
		info.PasswordSource != "field password of "+record+" (hex of "+filepath.Join(b.sysDir, "keys", id)+")" {
		t.Fatalf("info = %+v", info)
	}
	if loc.Password != restic.PasswordFromKey(key) {
		t.Fatal("the location's password is not the bucket's")
	}
	raw, _ := json.Marshal(info)
	if strings.Contains(string(raw), loc.Password) {
		t.Fatalf("the info discloses the password: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(info.Repository, "config")); err != nil {
		t.Fatalf("repository not initialized: %v", err)
	}
	// The record holds the password (as for archived versions) and the
	// bucket's name.
	rec, _, err := readRecovery(record)
	if err != nil || rec.Password != loc.Password || !rec.Initialized || rec.Bucket != "arc" {
		t.Fatalf("record = %+v, %v", rec, err)
	}

	// A lookup finds the same repository; archived versions use it.
	again, loc2, err := b.ResticRepository(ctx, "arc", false)
	if err != nil || again.Repository != info.Repository || !again.Initialized || loc2.Password != loc.Password {
		t.Fatalf("lookup = %+v, %v", again, err)
	}
	if _, err := putRestic(t, b, "arc", "k", "archived", storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	eq(t, "snapshots", snapshotCount(t, b, "arc"), 1)
	eq(t, "init", resticCalls(t, log, "init"), 1)
}

func TestResticHostAux(t *testing.T) {
	log := installFakeRestic(t, "host")
	dir := t.TempDir()
	b := openTest(t, dir)
	for _, name := range []string{"", "Snap", "a/b", "..", "a_b", strings.Repeat("a", 65)} {
		if _, _, err := b.ResticAuxRepository(ctx, name, true); !errors.Is(err, storage.ErrInvalidName) {
			t.Fatalf("name %q = %v", name, err)
		}
	}
	_, _, err := b.ResticAuxRepository(ctx, "snap-1", false)
	wantNoRepository(t, "lookup before create", err)
	keyPath := filepath.Join(b.sysDir, "keys", "aux", "snap-1")
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a lookup created the key: %v", err)
	}

	info, loc, err := b.ResticAuxRepository(ctx, "snap-1", true)
	if err != nil {
		t.Fatal(err)
	}
	record := keyPath + ".restic.json"
	repo := filepath.Join(b.sysDir, "restic-aux", "snap-1")
	if info.BucketID != "" || info.Repository != repo || loc.Repository != repo || !info.Initialized ||
		info.RecordLocation != record || info.PasswordSource != "field password of "+record+" (hex of "+keyPath+")" {
		t.Fatalf("info = %+v", info)
	}
	key, err := os.ReadFile(keyPath)
	if err != nil || len(key) != 32 || restic.PasswordFromKey(key) != loc.Password {
		t.Fatalf("key = %x, %v", key, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("key mode = %v, %v", fi.Mode(), err)
		}
	}
	rec, _, err := readRecovery(record)
	if err != nil || rec.Password != loc.Password || rec.Repository != repo || !rec.Initialized {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(repo, "config")); err != nil {
		t.Fatalf("repository not initialized: %v", err)
	}

	// Created once: again, and after a restart, the same key.
	if _, loc2, err := b.ResticAuxRepository(ctx, "snap-1", true); err != nil || loc2.Password != loc.Password {
		t.Fatalf("again = %v", err)
	}
	_ = b.Close()
	b = openTest(t, dir)
	if got, loc2, err := b.ResticAuxRepository(ctx, "snap-1", false); err != nil || !got.Initialized || loc2.Password != loc.Password {
		t.Fatalf("lookup after a restart = %+v, %v", got, err)
	}
	if _, loc2, err := b.ResticAuxRepository(ctx, "snap-1", true); err != nil || loc2.Password != loc.Password {
		t.Fatalf("create after a restart = %v", err)
	}
	if now, _ := os.ReadFile(keyPath); !bytes.Equal(now, key) {
		t.Fatal("the key was rewritten")
	}
	eq(t, "init", resticCalls(t, log, "init"), 1)
	// Aux repositories are independent of each other.
	_, other, err := b.ResticAuxRepository(ctx, "snap-2", true)
	if err != nil || other.Password == loc.Password || other.Repository == loc.Repository {
		t.Fatalf("second aux repository = %+v, %v", other, err)
	}

	// Never regenerated: with the key lost, the repository's record or
	// data refuse a new key.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		if _, _, err := b.ResticAuxRepository(ctx, "snap-1", create); !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "restore it") {
			t.Fatalf("lost key (create=%v) = %v", create, err)
		}
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.ResticAuxRepository(ctx, "snap-1", true); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("lost key and record = %v", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key was regenerated: %v", err)
	}
}
