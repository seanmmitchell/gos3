package s3

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	storagefs "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// Restic repositories follow the bucket's placement: the primary, a replica
// and a move target each archive into a repository on their own backing.
func TestResticInheritsReplicaAndMoveBackings(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("install restic >= 0.17.1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var backings []multi.Backing
	for _, name := range []string{"source", "replica", "target"} {
		dir := t.TempDir()
		b, err := storagefs.Open(dir, storagefs.Options{})
		if err != nil {
			t.Fatal(err)
		}
		backings = append(backings, multi.Backing{Name: name, Kind: "fs", Path: dir, Backend: b})
	}
	r, err := multi.Open(ctx, multi.Options{Backings: backings, HealthInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.CreateBucket(ctx, "archive", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetBucketVersioning(ctx, "archive", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := r.PutBucketConfig(ctx, "archive", storage.ConfigEncryptionProvider, []byte(`{"provider":"restic"}`)); err != nil {
		t.Fatal(err)
	}
	w, err := r.NewObjectWriterWith(ctx, "archive", "key", storage.WriteOptions{Encryption: storage.EncryptionRestic})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("placement chooses repository storage")
	_, _ = w.Write(data)
	info, err := w.Commit(ctx, storage.ObjectMeta{SSE: "AES256"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetReplication(ctx, "archive", multi.ModeActivePassive, []string{"replica"}); err != nil {
		t.Fatal(err)
	}
	wait := func(primary string) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			p, e := r.Placement("archive")
			if e == nil && p.Primary == primary && p.Move == nil {
				ready := true
				for _, m := range p.Members {
					ready = ready && m.State == multi.StateInSync
				}
				if ready {
					return
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("placement did not finish")
	}
	wait("source")
	read := func(b storage.Backend) {
		t.Helper()
		got, rd, e := b.OpenObject(ctx, "archive", "key", info.VersionID)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := io.ReadAll(rd)
		_ = rd.Close()
		if e != nil || !bytes.Equal(raw, data) || got.Encryption != storage.EncryptionRestic {
			t.Fatal("restic copy lost data or encoding")
		}
	}
	read(backings[1].Backend)
	// Every backing holding a copy keeps its own repository.
	repository := func(b multi.Backing) {
		t.Helper()
		files, err := filepath.Glob(filepath.Join(b.Path, ".gos3", "restic", "*", "config"))
		if err != nil || len(files) != 1 {
			t.Fatalf("backing %s has no repository of its own: %v %v", b.Name, files, err)
		}
	}
	repository(backings[0])
	repository(backings[1])
	if _, err := r.Move(ctx, "archive", "", "target", nil); err != nil {
		t.Fatal(err)
	}
	wait("target")
	read(r)
	read(backings[2].Backend)
	repository(backings[2])
	raw, err := backings[2].Backend.GetBucketConfig(ctx, "archive", storage.ConfigEncryptionProvider)
	if err != nil || string(raw) != `{"provider":"restic"}` {
		t.Fatal("bucket replication lost the provider selection")
	}
}
