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

func TestResticInheritsReplicaAndMoveBackings(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("install restic >= 0.17.1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	remote := newTestEnv(t)
	remote.createBucket("repositories")
	var backings []multi.Backing
	for i, name := range []string{"source", "replica", "target"} {
		dir := t.TempDir()
		var cfg *storage.ResticS3
		if i != 1 {
			cfg = &storage.ResticS3{Endpoint: remote.srv.URL, Bucket: "repositories", Prefix: name, AccessKey: testRoot.AccessKey, SecretKey: testRoot.SecretKey}
		}
		b, err := storagefs.Open(dir, storagefs.Options{ResticS3: cfg})
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
	// Replica uses its local backing, although the source stores in remote S3.
	files, err := filepath.Glob(filepath.Join(backings[1].Path, ".gos3", "restic", "*", "config"))
	if err != nil || len(files) != 1 {
		t.Fatal("replica inherited source S3 settings instead of its local backing")
	}
	if _, err := r.Move(ctx, "archive", "", "target"); err != nil {
		t.Fatal(err)
	}
	wait("target")
	read(r)
	read(backings[2].Backend)
	for _, prefix := range []string{"source/", "target/"} {
		count := 0
		err := remote.backend.WalkObjects(ctx, "repositories", prefix, "", func(i storage.ObjectInfo) error { count++; return nil })
		if err != nil || count == 0 {
			t.Fatalf("missing backing repository prefix %s: %v", prefix, err)
		}
	}
	raw, err := backings[2].Backend.GetBucketConfig(ctx, "archive", storage.ConfigEncryptionProvider)
	if err != nil || string(raw) != `{"provider":"restic"}` {
		t.Fatal("bucket replication copied storage settings or credentials")
	}
}
