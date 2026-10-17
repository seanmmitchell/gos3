package fs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// newResticBucket opens a backend with a versioned bucket "arc" whose
// writes go to restic, and the fake restic's log.
func newResticBucket(t *testing.T) (*Backend, string) {
	t.Helper()
	log := installFakeRestic(t, "current")
	b := openTest(t, "")
	mkBucket(t, b, "arc", storage.VersioningEnabled)
	if err := b.PrepareEncryption(ctx, "arc", storage.EncryptionRestic); err != nil {
		t.Fatal(err)
	}
	return b, log
}

// putRestic writes data to bucket/key as a restic version.
func putRestic(t *testing.T, b *Backend, bucket, key, data string, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	t.Helper()
	w, err := b.NewObjectWriterWith(ctx, bucket, key, storage.WriteOptions{Encryption: storage.EncryptionRestic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		_ = w.Abort()
		return storage.ObjectInfo{}, err
	}
	return w.Commit(ctx, storage.ObjectMeta{ETag: md5hex(data), SSE: "AES256"}, opts)
}

func TestResticArchiveStreamsAndPreparesOnce(t *testing.T) {
	b, log := newResticBucket(t)
	recovery := filepath.Join(b.sysDir, "keys", mustBucketID(t, b, "arc")+".restic.json")
	var before os.FileInfo
	for i, data := range []string{"first version", strings.Repeat("second ", 50000), "third"} {
		if err := b.PrepareEncryption(ctx, "arc", storage.EncryptionRestic); err != nil {
			t.Fatal(err)
		}
		info, err := putRestic(t, b, "arc", "k", data, storage.CommitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "encryption", info.Encryption, storage.EncryptionRestic)
		eq(t, "content", read(t, b, "arc", "k", info.VersionID), data)
		if i == 0 {
			// The first write recorded the bucket's name.
			rec, _, err := readRecovery(recovery)
			if err != nil || rec.Bucket != "arc" {
				t.Fatalf("recovery record = %+v, %v", rec, err)
			}
			if before, err = os.Stat(recovery); err != nil {
				t.Fatal(err)
			}
		}
	}
	// K5: the repository was opened (and initialized) once for all writes,
	// and the unchanged recovery record was not rewritten after the first
	// write named the bucket.
	eq(t, "cat config", resticCalls(t, log, "cat config"), 1)
	eq(t, "init", resticCalls(t, log, "init"), 1)
	eq(t, "backup", resticCalls(t, log, "backup"), 3)
	after, err := os.Stat(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("recovery record rewritten")
	}
	// G1-f: restic runs with a persistent cache below the system directory.
	if _, err := os.Stat(filepath.Join(b.sysDir, resticCacheDir)); err != nil {
		t.Fatalf("restic cache directory: %v", err)
	}
	if files := tmpFiles(t, b); len(files) != 0 {
		t.Fatalf("temporary files left: %v", files)
	}
}

func mustBucketID(t *testing.T, b *Backend, bucket string) string {
	t.Helper()
	id, err := b.encryptionBucketID(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// G1-g: empty objects never reach restic.
func TestResticEmptyObjectNeedsNoRestic(t *testing.T) {
	b, log := newResticBucket(t)
	info, err := putRestic(t, b, "arc", "empty", "", storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "encryption", info.Encryption, storage.EncryptionRestic)
	eq(t, "backup", resticCalls(t, log, "backup"), 0)
	eq(t, "data files", len(dataFiles(t, b)), 0)
	eq(t, "content", read(t, b, "arc", "empty", ""), "")
	eq(t, "dump", resticCalls(t, log, "dump"), 0)
}

// G1-b: a write refused by its preconditions archives nothing.
func TestResticRefusedWriteLeavesNoSnapshot(t *testing.T) {
	b, _ := newResticBucket(t)
	if _, err := putRestic(t, b, "arc", "k", "one", storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	eq(t, "snapshots", snapshotCount(t, b, "arc"), 1)
	_, err := putRestic(t, b, "arc", "k", "two", storage.CommitOptions{IfNoneMatch: true})
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("If-None-Match: %v", err)
	}
	_, err = putRestic(t, b, "arc", "k", "three", storage.CommitOptions{IfMatch: "nope"})
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("If-Match: %v", err)
	}
	eq(t, "snapshots", snapshotCount(t, b, "arc"), 1)
	// An aborted write leaves no snapshot either.
	w, err := b.NewObjectWriterWith(ctx, "arc", "k", storage.WriteOptions{Encryption: storage.EncryptionRestic})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "aborted")
	_ = w.Abort()
	eq(t, "snapshots", snapshotCount(t, b, "arc"), 1)
	eq(t, "content", read(t, b, "arc", "k", ""), "one")
}

// G1-d: opening restores nothing; concurrent readers share one restore,
// which is removed with the last reader.
func TestResticReadsAreLazyAndShared(t *testing.T) {
	b, log := newResticBucket(t)
	data := strings.Repeat("0123456789", 100000)
	if _, err := putRestic(t, b, "arc", "k", data, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	_, rd, err := b.OpenObject(ctx, "arc", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	_ = rd.Close()
	eq(t, "dump after open/seek/close", resticCalls(t, log, "dump"), 0)

	t.Setenv("FAKERESTIC_DUMP_DELAY", "300ms")
	const readers = 4
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	start := make(chan struct{})
	opened := make([]io.ReadSeekCloser, readers)
	for i := range opened {
		if _, opened[i], err = b.OpenObject(ctx, "arc", "k", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range opened {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			off := int64(i * 1000)
			buf := make([]byte, 1000)
			if _, err := r.(io.ReaderAt).ReadAt(buf, off); err != nil {
				errs <- err
				return
			}
			if string(buf) != data[off:off+1000] {
				errs <- errors.New("wrong range content")
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	eq(t, "dumps for concurrent readers", resticCalls(t, log, "dump"), 1)
	if len(tmpFiles(t, b)) != 1 {
		t.Fatalf("want one shared spool, have %v", tmpFiles(t, b))
	}
	for _, r := range opened {
		_ = r.Close()
	}
	if files := tmpFiles(t, b); len(files) != 0 {
		t.Fatalf("spool not removed: %v", files)
	}
	// A later reader restores again; Prepare reports the restore.
	_, rd, err = b.OpenObject(ctx, "arc", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	if err := rd.(storage.PreparingReader).Prepare(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rd)
	if err != nil || string(got) != data {
		t.Fatalf("read after prepare: %v", err)
	}
	eq(t, "dumps", resticCalls(t, log, "dump"), 2)
}

// G1-a, K4: restic failures wrap storage.ErrEncryptionUnavailable and carry
// restic's redacted diagnostics; an ended request reports its context.
func TestResticFailuresAreClassifiedAndRedacted(t *testing.T) {
	b, _ := newResticBucket(t)
	if _, err := putRestic(t, b, "arc", "k", "kept", storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg, err := b.resticSettings(mustBucketID(t, b, "arc"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKERESTIC_FAIL", "backup")
	_, err = putRestic(t, b, "arc", "k", "lost", storage.CommitOptions{})
	if !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("backup failure not classified: %v", err)
	}
	if !strings.Contains(err.Error(), "unable to save snapshot") || strings.Contains(err.Error(), cfg.Password) {
		t.Fatalf("diagnostics missing or not redacted: %v", err)
	}
	t.Setenv("FAKERESTIC_FAIL", "dump")
	_, rd, err := b.OpenObject(ctx, "arc", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rd)
	_ = rd.Close()
	if !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("dump failure not classified: %v", err)
	}
	t.Setenv("FAKERESTIC_FAIL", "")
	t.Setenv("FAKERESTIC_DUMP_DELAY", "5s")
	cctx, cancel := context.WithCancel(ctx)
	_, rd, err = b.OpenObject(cctx, "arc", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	err = rd.(storage.PreparingReader).Prepare()
	_ = rd.Close()
	if !errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("canceled read: %v", err)
	}
}

func TestResticVersionCheck(t *testing.T) {
	installFakeRestic(t, "old")
	t.Setenv("FAKERESTIC_VERSION", "0.16.4")
	b := openTest(t, "")
	mkBucket(t, b, "arc", "")
	err := b.PrepareEncryption(ctx, "arc", storage.EncryptionRestic)
	if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "0.17.1") {
		t.Fatalf("old restic accepted: %v", err)
	}
}

// G1-h: a multipart completion whose parts change is retried without
// archiving the discarded attempt, and refused completions archive nothing.
func TestResticMultipartArchivesOnce(t *testing.T) {
	b, _ := newResticBucket(t)
	up, err := b.CreateMultipartUpload(ctx, "arc", "k", storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionRestic}, "")
	if err != nil {
		t.Fatal(err)
	}
	uploadPart(t, b, "arc", "k", up.UploadID, 1, "old-1|")
	uploadPart(t, b, "arc", "k", up.UploadID, 2, "old-2")
	calls := 0
	afterPartsCopied = func(id string) {
		calls++
		if calls == 1 {
			uploadPart(t, b, "arc", "k", id, 2, "new-2")
		}
	}
	t.Cleanup(func() { afterPartsCopied = nil })
	wopts := storage.WriteOptions{Encryption: storage.EncryptionRestic}
	meta := storage.ObjectMeta{ETag: "x-2", SSE: "AES256"}
	if _, err := b.CompleteMultipartUploadWith(ctx, "arc", "k", up.UploadID, []int{1, 2}, meta, storage.CommitOptions{IfNoneMatch: true}, wopts); err != nil {
		t.Fatal(err)
	}
	eq(t, "attempts", calls, 2)
	eq(t, "content", read(t, b, "arc", "k", ""), "old-1|new-2")
	eq(t, "snapshots", snapshotCount(t, b, "arc"), 1)
	afterPartsCopied = nil

	up2, _ := b.CreateMultipartUpload(ctx, "arc", "k", storage.ObjectMeta{SSE: "AES256", Encryption: storage.EncryptionRestic}, "")
	uploadPart(t, b, "arc", "k", up2.UploadID, 1, "again")
	_, err = b.CompleteMultipartUploadWith(ctx, "arc", "k", up2.UploadID, []int{1}, meta, storage.CommitOptions{IfNoneMatch: true}, wopts)
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("If-None-Match completion: %v", err)
	}
	eq(t, "snapshots", snapshotCount(t, b, "arc"), 1)
}

// G3-b: a failed initialization leaves an uninitialized recovery record and
// is retried by the next write.
func TestResticFailedPreparationIsRetried(t *testing.T) {
	installFakeRestic(t, "current")
	b := openTest(t, "")
	mkBucket(t, b, "arc", "")
	t.Setenv("FAKERESTIC_FAIL", "init")
	if err := b.PrepareEncryption(ctx, "arc", storage.EncryptionRestic); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("failed init: %v", err)
	}
	rec, _, err := readRecovery(b.recoveryPath(mustBucketID(t, b, "arc")))
	if err != nil || rec.Repository == "" || rec.Initialized {
		t.Fatalf("recovery record before init: %+v, %v", rec, err)
	}
	t.Setenv("FAKERESTIC_FAIL", "")
	if err := b.PrepareEncryption(ctx, "arc", storage.EncryptionRestic); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ = readRecovery(b.recoveryPath(mustBucketID(t, b, "arc"))); !rec.Initialized {
		t.Fatal("recovery record not marked initialized")
	}
}

// A recovery record of a repository on S3 (from the removed per-backing
// restic S3 settings) fails closed instead of starting a new local
// repository.
func TestResticS3RecoveryRecordFailsClosed(t *testing.T) {
	log := installFakeRestic(t, "current")
	b := openTest(t, "")
	mkBucket(t, b, "arc", "")
	id := mustBucketID(t, b, "arc")
	if err := os.MkdirAll(filepath.Dir(b.recoveryPath(id)), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.recoveryPath(id), []byte(`{"initialized":true,"repository":"s3:https://s3.example.test/archives/`+id+`","password":"x"}`), filePerm); err != nil {
		t.Fatal(err)
	}
	err := b.PrepareEncryption(ctx, "arc", storage.EncryptionRestic)
	if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "S3 settings that no longer exist") {
		t.Fatalf("prepare with an S3 recovery record: %v", err)
	}
	if err := b.ValidateEncryptionSettings(ctx, "arc", storage.EncryptionSettings{Provider: storage.EncryptionRestic}); !errors.Is(err, storage.ErrEncryptionUnavailable) {
		t.Fatalf("validate with an S3 recovery record: %v", err)
	}
	if n := resticCalls(t, log, "init"); n != 0 {
		t.Fatalf("restic init ran %d times", n)
	}
	if _, err := os.Stat(b.resticRepository(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local repository created: %v", err)
	}
}

// A replica receives restic copies without the S3 layer having prepared
// its bucket: the version writer creates the key and repository.
func TestResticImportPreparesBucket(t *testing.T) {
	log := installFakeRestic(t, "current")
	b := openTest(t, "")
	mkBucket(t, b, "replica", storage.VersioningEnabled)
	w, err := b.NewVersionWriterWith(ctx, "replica", "k", storage.WriteOptions{Encryption: storage.EncryptionRestic})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, "copied")
	info, err := w.Commit(ctx, storage.ObjectInfo{Key: "k", VersionID: "v1", LastModified: time.Now(), ObjectMeta: storage.ObjectMeta{ETag: md5hex("copied")}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "encryption", info.Encryption, storage.EncryptionRestic)
	eq(t, "content", read(t, b, "replica", "k", "v1"), "copied")
	eq(t, "init", resticCalls(t, log, "init"), 1)
}
