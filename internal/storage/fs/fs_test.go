package fs

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestOpenErrors(t *testing.T) {
	if _, err := Open("", Options{}); err == nil {
		t.Fatal("empty root accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, Options{}); err == nil {
		t.Fatal("root that is a file accepted")
	}
}

// TestSystemDirPermissions: the system directory and its subdirectories
// are private to the server's user (they hold metadata, and the server
// keeps root credentials next to them).
func TestSystemDirPermissions(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	for _, d := range []string{b.sysDir, b.dataDir, b.tmpDir, b.uploadDir} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode %v, want no group/other access", d, perm)
		}
	}
}

func TestOpenLockedFailsFast(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	start := time.Now()
	b2, err := Open(dir, Options{})
	if err == nil {
		b2.Close()
		t.Fatal("second Open of the same root succeeded")
	}
	if !strings.Contains(err.Error(), "locked by another process") {
		t.Fatalf("unexpected error: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("second Open took %v", d)
	}
	if b.Root() != dir {
		t.Fatalf("Root() = %q, want %q", b.Root(), dir)
	}
	// After Close the root can be opened again.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	openTest(t, dir)
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	b := openDurable(t, dir)
	if err := b.CreateBucket(ctx, "ver", storage.CreateBucketOptions{Region: "r1", Owner: "bob", ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}
	mkBucket(t, b, "plain", "")
	v1 := put(t, b, "ver", "k", "one")
	v2 := put(t, b, "ver", "k", "two")
	dm, _ := b.DeleteObject(ctx, "ver", "gone", "")
	put(t, b, "plain", "a/b", "plain-data")
	put(t, b, "plain", "empty", "")
	if _, err := b.UpdateObjectMeta(ctx, "ver", "k", v1.VersionID, func(m *storage.ObjectMeta) error {
		m.Tags = map[string]string{"t": "1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.PutBucketConfig(ctx, "ver", storage.ConfigLifecycle, []byte("<lc/>")); err != nil {
		t.Fatal(err)
	}
	up, _ := b.CreateMultipartUpload(ctx, "plain", "mp", storage.ObjectMeta{ContentType: "x/y"}, "carol")
	uploadPart(t, b, "plain", "mp", up.UploadID, 2, "part-two")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b = openTest(t, dir)
	bs, err := b.ListBuckets(ctx)
	if err != nil || len(bs) != 2 || bs[0].Name != "plain" || bs[1].Name != "ver" {
		t.Fatalf("buckets after reopen = %+v, %v", bs, err)
	}
	if bi := bs[1]; bi.Region != "r1" || bi.Owner != "bob" || !bi.ObjectLockEnabled || bi.Versioning != storage.VersioningEnabled {
		t.Fatalf("bucket info %+v", bi)
	}
	eq(t, "versions", joinVers(versions(t, b, "ver", "", "", "")),
		fmt.Sprintf("gone@%s(dm)* k@%s* k@%s", dm.VersionID, v2.VersionID, v1.VersionID))
	eq(t, "v1", read(t, b, "ver", "k", v1.VersionID), "one")
	eq(t, "latest", read(t, b, "ver", "k", ""), "two")
	if i, _ := b.GetObjectInfo(ctx, "ver", "k", v1.VersionID); i.Tags["t"] != "1" || !i.LastModified.Equal(v1.LastModified) {
		t.Fatalf("v1 info %+v", i)
	}
	eq(t, "plain", read(t, b, "plain", "a/b", ""), "plain-data")
	eq(t, "empty", read(t, b, "plain", "empty", ""), "")
	if c, err := b.GetBucketConfig(ctx, "ver", storage.ConfigLifecycle); err != nil || string(c) != "<lc/>" {
		t.Fatalf("config = %q, %v", c, err)
	}
	ups, err := b.ListMultipartUploads(ctx, "plain")
	if err != nil || len(ups) != 1 || ups[0].UploadID != up.UploadID || ups[0].Initiator != "carol" || ups[0].Meta.ContentType != "x/y" {
		t.Fatalf("uploads = %+v, %v", ups, err)
	}
	waitRecovery(t, b) // must not remove the live part
	uploadPart(t, b, "plain", "mp", up.UploadID, 1, "part-one-")
	if _, err := b.CompleteMultipartUpload(ctx, "plain", "mp", up.UploadID, []int{1, 2}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	eq(t, "multipart", read(t, b, "plain", "mp", ""), "part-one-part-two")

	// Version IDs keep being unique and ordered after reopen.
	v3 := put(t, b, "ver", "k", "three")
	if v3.VersionID == v1.VersionID || v3.VersionID == v2.VersionID {
		t.Fatal("version ID reused after reopen")
	}
	eq(t, "newest", versions(t, b, "ver", "k", "", "")[0].id, v3.VersionID)
}

func TestRecoveryRemovesOrphans(t *testing.T) {
	dir := t.TempDir()
	b := openTest(t, dir)
	mkBucket(t, b, "bkt", "")
	keep := put(t, b, "bkt", "keep", "keep-data")
	up, _ := b.CreateMultipartUpload(ctx, "bkt", "mp", storage.ObjectMeta{}, "")
	uploadPart(t, b, "bkt", "mp", up.UploadID, 1, "live-part")
	var bucketID string
	for _, f := range dataFiles(t, b) {
		bucketID = strings.Split(f, string(filepath.Separator))[0]
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	sys := filepath.Join(dir, ".gos3")
	mustWrite := func(rel string) string {
		p := filepath.Join(sys, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	junk := []string{
		mustWrite("tmp/w-12345"),
		mustWrite("tmp/nested/dir/file"),
		mustWrite(filepath.Join("data", bucketID, "ab", "ab"+strings.Repeat("0", 30))), // unreferenced data file
		mustWrite(filepath.Join("data", bucketID, "stray")),                            // unexpected file
		mustWrite(filepath.Join("data", "deadbeefdeadbeef", "00", "00ff")),             // deleted bucket
		mustWrite(filepath.Join("data", "stray")),
		mustWrite(filepath.Join("uploads", "gone-upload", "1-abcdef")), // finished upload
		mustWrite(filepath.Join("uploads", up.UploadID, "1-0123456789abcdef")),
		mustWrite(filepath.Join("uploads", up.UploadID, "garbage")),
		mustWrite(filepath.Join("uploads", "stray")),
	}

	var logBuf syncBuffer
	b, err := Open(dir, Options{Logger: slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	// tmp/ is cleaned synchronously by Open.
	eq(t, "tmp files right after Open", len(tmpFiles(t, b)), 0)
	waitRecovery(t, b)
	for _, p := range junk {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived recovery (%v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sys, "data", "deadbeefdeadbeef")); !os.IsNotExist(err) {
		t.Error("deleted bucket directory survived")
	}
	eq(t, "kept object", read(t, b, "bkt", "keep", ""), "keep-data")
	_ = keep
	parts, err := b.ListParts(ctx, "bkt", "mp", up.UploadID)
	if err != nil || len(parts) != 1 {
		t.Fatalf("parts = %+v, %v", parts, err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, "bkt", "mp", up.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatalf("complete after recovery: %v", err)
	}
	eq(t, "multipart", read(t, b, "bkt", "mp", ""), "live-part")
	logs := logBuf.String()
	for _, want := range []string{"removed temporary files", "orphan_data_files=1", "deleted_bucket_dirs=1", "stale_upload_dirs=1", "orphan_part_files=2", "stray_entries=3"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

// TestRecoveryConcurrentWithWriters runs the orphan scan repeatedly while
// objects and parts are being written: it must never remove a file whose
// metadata is about to be (or has been) committed.
func TestRecoveryConcurrentWithWriters(t *testing.T) {
	b, bkt := newBucket(t, storage.VersioningEnabled)
	waitRecovery(t, b)
	up, _ := b.CreateMultipartUpload(ctx, bkt, "mp", storage.ObjectMeta{}, "")
	stop := make(chan struct{})
	scans := make(chan int)
	go func() {
		n := 0
		defer func() { scans <- n }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var st recoveryStats
			b.recoverData(ctx, &st)
			b.recoverUploads(ctx, &st)
			if st.dataFiles+st.partFiles+st.bucketDirs+st.uploadDirs != 0 {
				t.Errorf("scan removed live files: %+v", st)
			}
			n++
		}
	}()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 40 {
				put(t, b, bkt, fmt.Sprintf("w%d/%d", w, i), fmt.Sprintf("data-%d-%d", w, i))
				uploadPart(t, b, bkt, "mp", up.UploadID, w*100+i+1, "part")
			}
		}()
	}
	wg.Wait()
	close(stop)
	if n := <-scans; n == 0 {
		t.Fatal("no scan ran")
	}
	for w := range 4 {
		for i := range 40 {
			eq(t, "content", read(t, b, bkt, fmt.Sprintf("w%d/%d", w, i), ""), fmt.Sprintf("data-%d-%d", w, i))
		}
	}
	parts, _ := b.ListParts(ctx, bkt, "mp", up.UploadID)
	nums := make([]int, len(parts))
	for i, p := range parts {
		nums[i] = p.PartNumber
	}
	if _, err := b.CompleteMultipartUpload(ctx, bkt, "mp", up.UploadID, nums, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	eq(t, "multipart size", len(read(t, b, bkt, "mp", "")), 4*40*len("part"))
}

func TestReaderStableAfterOverwriteAndDelete(t *testing.T) {
	for _, vs := range []storage.VersioningStatus{"", storage.VersioningEnabled} {
		t.Run(string(vs), func(t *testing.T) {
			b, bkt := newBucket(t, vs)
			orig := strings.Repeat("original-", 100000) // ~900 KB
			oi := put(t, b, bkt, "k", orig)
			info, r1, err := b.OpenObject(ctx, bkt, "k", "")
			if err != nil {
				t.Fatal(err)
			}
			defer r1.Close()
			if info.Size != int64(len(orig)) {
				t.Fatalf("size %d", info.Size)
			}
			head := make([]byte, 9)
			if _, err := io.ReadFull(r1, head); err != nil {
				t.Fatal(err)
			}
			_, r2, err := b.OpenObject(ctx, bkt, "k", "")
			if err != nil {
				t.Fatal(err)
			}
			defer r2.Close()

			put(t, b, bkt, "k", "replacement")
			// Permanently delete everything, including the version r1/r2 read.
			for _, v := range versions(t, b, bkt, "", "", "") {
				if _, err := b.DeleteObject(ctx, bkt, v.key, v.id); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.GetObjectInfo(ctx, bkt, "k", oi.VersionID); !errors.Is(err, storage.ErrVersionNotFound) {
				t.Fatalf("deleted version still visible: %v", err)
			}
			eq(t, "data files", len(dataFiles(t, b)), 0)

			rest, err := io.ReadAll(r1)
			if err != nil || string(head)+string(rest) != orig {
				t.Fatalf("r1 content changed (err %v)", err)
			}
			if _, err := r2.Seek(int64(len(orig))-9, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			tail, _ := io.ReadAll(r2)
			eq(t, "r2 tail", string(tail), "original-")
			if err := r2.Close(); err != nil {
				t.Fatal(err)
			}
			if err := r2.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
			if _, err := r2.Read(make([]byte, 1)); err == nil {
				t.Fatal("read after Close succeeded")
			}
		})
	}
}

func TestOpenRetriesAfterConcurrentOverwrite(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "k", "v0")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			put(t, b, bkt, "k", fmt.Sprintf("v%d", i))
		}
	}()
	for range 200 {
		info, r, err := b.OpenObject(ctx, bkt, "k", "")
		if err != nil {
			t.Fatalf("open during overwrites: %v", err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || int64(len(data)) != info.Size || md5hex(string(data)) != info.ETag {
			t.Fatalf("inconsistent read %q for %+v (%v)", data, info, err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestFailedCommitsLeaveNoFiles(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "k", "v")
	before := dataFiles(t, b)
	for i := range 5 {
		if _, err := putOpts(t, b, bkt, "k", fmt.Sprint("new", i), storage.CommitOptions{IfNoneMatch: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
			t.Fatal(err)
		}
		if _, err := putOpts(t, b, bkt, "k", fmt.Sprint("new", i), storage.CommitOptions{IfMatch: "wrong"}); !errors.Is(err, storage.ErrPreconditionFailed) {
			t.Fatal(err)
		}
	}
	// Multipart completion with failed precondition.
	up, _ := b.CreateMultipartUpload(ctx, bkt, "k", storage.ObjectMeta{}, "")
	uploadPart(t, b, bkt, "k", up.UploadID, 1, "p")
	if _, err := b.CompleteMultipartUpload(ctx, bkt, "k", up.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{IfMatch: "wrong"}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatal(err)
	}
	after := dataFiles(t, b)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("data files changed: %v -> %v", before, after)
	}
	eq(t, "tmp files", len(tmpFiles(t, b)), 0)
	eq(t, "content", read(t, b, bkt, "k", ""), "v")
}

func TestConcurrentWritersSameKey(t *testing.T) {
	for _, vs := range []storage.VersioningStatus{"", storage.VersioningEnabled, storage.VersioningSuspended} {
		t.Run(string(vs)+"_", func(t *testing.T) {
			b, bkt := newBucket(t, "")
			if vs != "" {
				if err := b.SetBucketVersioning(ctx, bkt, vs); err != nil {
					t.Fatal(err)
				}
			}
			const writers, each = 8, 15
			var wg sync.WaitGroup
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range each {
						data := fmt.Sprintf("writer-%d-%d", w, i)
						put(t, b, bkt, "hot", data)
						if i%5 == 0 {
							if _, err := b.DeleteObject(ctx, bkt, "hot", ""); err != nil {
								t.Error(err)
							}
							put(t, b, bkt, "hot", data)
						}
						info, r, err := b.OpenObject(ctx, bkt, "hot", "")
						if errors.Is(err, storage.ErrObjectNotFound) {
							continue // unversioned delete by another writer
						}
						if err != nil {
							t.Error(err)
							return
						}
						if r != nil {
							got, _ := io.ReadAll(r)
							r.Close()
							if md5hex(string(got)) != info.ETag {
								t.Errorf("content/ETag mismatch: %q vs %s", got, info.ETag)
							}
						}
					}
				}()
			}
			wg.Wait()

			vers := versions(t, b, bkt, "", "", "")
			var objects int
			for i, v := range vers {
				if v.latest != (i == 0) {
					t.Fatalf("latest flags wrong: %s", joinVers(vers))
				}
				if !v.dm {
					objects++
				}
			}
			switch vs {
			case storage.VersioningEnabled:
				eq(t, "versions", len(vers), writers*(each+2*3))
			default:
				eq(t, "versions", joinVers(vers), "hot@null*")
			}
			// Exactly one data file per stored (non-marker) version.
			eq(t, "data files", len(dataFiles(t, b)), objects)
			eq(t, "tmp files", len(tmpFiles(t, b)), 0)
			if keys := keysOf(t, b, bkt, "", ""); len(keys) != 1 {
				t.Fatalf("keys = %v", keys)
			}
		})
	}
}

// patternReader produces a deterministic pseudo-random stream without
// buffering it.
type patternReader struct {
	rng *rand.ChaCha8
	n   int64
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.n {
		b = b[:p.n]
	}
	p.rng.Read(b)
	p.n -= int64(len(b))
	return len(b), nil
}

func TestLargeObjectStreamed(t *testing.T) {
	if testing.Short() {
		t.Skip("large object in -short mode")
	}
	b, bkt := newBucket(t, "")
	const size = 40 << 20 // 40 MiB, not a multiple of the buffer size
	const odd = size + 12345
	seed := [32]byte{1, 2, 3}
	want := sha256.New()
	io.Copy(want, &patternReader{rng: rand.NewChaCha8(seed), n: odd})

	w, err := b.NewObjectWriter(ctx, bkt, "big")
	if err != nil {
		t.Fatal(err)
	}
	// Mix of write sizes: small writes go through the buffer, large ones bypass it.
	src := &patternReader{rng: rand.NewChaCha8(seed), n: odd}
	buf := make([]byte, 1<<20)
	for sizes := []int{1, 7, 100, 65536, 1 << 20, 3}; ; {
		n := sizes[0]
		sizes = append(sizes[1:], n)
		m, err := io.ReadFull(src, buf[:n])
		if m > 0 {
			if _, werr := w.Write(buf[:m]); werr != nil {
				t.Fatal(werr)
			}
		}
		if err != nil {
			break
		}
	}
	oi, err := w.Commit(ctx, storage.ObjectMeta{ETag: "big"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "size", oi.Size, int64(odd))

	_, r, err := b.OpenObject(ctx, bkt, "big", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := sha256.New()
	if n, err := io.Copy(got, r); err != nil || n != odd {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatal("content mismatch")
	}

	// Multipart of large parts streams too.
	up, _ := b.CreateMultipartUpload(ctx, bkt, "mp", storage.ObjectMeta{}, "")
	for n := 1; n <= 3; n++ {
		pw, err := b.NewPartWriter(ctx, bkt, "mp", up.UploadID, n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(pw, &patternReader{rng: rand.NewChaCha8([32]byte{byte(n)}), n: 12 << 20}); err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Commit(ctx, storage.PartMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	want.Reset()
	for n := 1; n <= 3; n++ {
		io.Copy(want, &patternReader{rng: rand.NewChaCha8([32]byte{byte(n)}), n: 12 << 20})
	}
	oi, err = b.CompleteMultipartUpload(ctx, bkt, "mp", up.UploadID, []int{1, 2, 3}, storage.ObjectMeta{}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "multipart size", oi.Size, int64(36<<20))
	_, r2, err := b.OpenObject(ctx, bkt, "mp", "")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got.Reset()
	io.Copy(got, r2)
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatal("multipart content mismatch")
	}
}

func TestCorruptDataDetected(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "k", "0123456789")
	files := dataFiles(t, b)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if err := os.Truncate(filepath.Join(b.dataDir, files[0]), 4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.OpenObject(ctx, bkt, "k", ""); !errors.Is(err, errCorrupt) {
		t.Fatalf("truncated data: %v", err)
	}
	if err := os.Remove(filepath.Join(b.dataDir, files[0])); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.OpenObject(ctx, bkt, "k", ""); err == nil || errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("missing data file: %v", err)
	}
}

func TestDeleteBucketRemovesDataDir(t *testing.T) {
	b, bkt := newBucket(t, "")
	put(t, b, bkt, "k", "v")
	var id string
	for _, f := range dataFiles(t, b) {
		id = strings.Split(f, string(filepath.Separator))[0]
	}
	if _, err := b.DeleteObject(ctx, bkt, "k", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteBucket(ctx, bkt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.dataDir, id)); !os.IsNotExist(err) {
		t.Fatalf("bucket data dir survived: %v", err)
	}
}

func TestDurableMode(t *testing.T) {
	dir := t.TempDir()
	b := openDurable(t, dir)
	mkBucket(t, b, "bkt", "")
	put(t, b, "bkt", "k", "durable")
	up, _ := b.CreateMultipartUpload(ctx, "bkt", "mp", storage.ObjectMeta{}, "")
	uploadPart(t, b, "bkt", "mp", up.UploadID, 1, "p1")
	if _, err := b.CompleteMultipartUpload(ctx, "bkt", "mp", up.UploadID, []int{1}, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	b.Close()
	b = openTest(t, dir)
	eq(t, "content", read(t, b, "bkt", "k", ""), "durable")
	eq(t, "multipart", read(t, b, "bkt", "mp", ""), "p1")
}

// syncBuffer is a goroutine-safe bytes.Buffer for log capture.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
