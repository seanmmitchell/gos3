package multi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	fsbackend "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// testLogger returns a logger that writes to the test log when -v is set.
func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// memBackings returns n memory backings named a, b, c, ...
func memBackings(n int) []Backing {
	out := make([]Backing, n)
	for i := range out {
		out[i] = Backing{Name: string(rune('a' + i)), Kind: "memory", Backend: memory.New()}
	}
	return out
}

// fsBacking opens an fs backing rooted at dir.
func fsBacking(t *testing.T, name, dir string) Backing {
	t.Helper()
	b, err := fsbackend.Open(dir, fsbackend.Options{})
	if err != nil {
		t.Fatalf("opening fs backing %s: %v", name, err)
	}
	return Backing{Name: name, Kind: "fs", Path: dir, Backend: b}
}

// openRouter opens a router over backings with fast timings for tests.
func openRouter(t *testing.T, opts Options) *Router {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = testLogger(t)
	}
	if opts.HealthInterval == 0 {
		opts.HealthInterval = 50 * time.Millisecond
	}
	if opts.CutoverTimeout == 0 {
		opts.CutoverTimeout = 5 * time.Second
	}
	r, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitState waits until every member of bucket is in state want.
func waitState(t *testing.T, r *Router, bucket, want string) PlacementStatus {
	t.Helper()
	var st PlacementStatus
	waitFor(t, fmt.Sprintf("all copies of %s to be %s", bucket, want), func() bool {
		var err error
		st, err = r.Placement(bucket)
		if err != nil {
			t.Fatalf("Placement: %v", err)
		}
		if st.Move != nil {
			return false
		}
		for _, m := range st.Members {
			if m.State != want {
				return false
			}
		}
		return true
	})
	return st
}

func put(t *testing.T, b storage.Backend, bucket, key, data string, meta storage.ObjectMeta) storage.ObjectInfo {
	t.Helper()
	ctx := context.Background()
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		t.Fatalf("NewObjectWriter %s/%s: %v", bucket, key, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatalf("Write %s/%s: %v", bucket, key, err)
	}
	sum := sha256.Sum256([]byte(data))
	if meta.ETag == "" {
		meta.ETag = hex.EncodeToString(sum[:16])
	}
	info, err := w.Commit(ctx, meta, storage.CommitOptions{})
	if err != nil {
		t.Fatalf("Commit %s/%s: %v", bucket, key, err)
	}
	return info
}

func read(t *testing.T, b storage.Backend, bucket, key, versionID string) string {
	t.Helper()
	_, rc, err := b.OpenObject(context.Background(), bucket, key, versionID)
	if err != nil {
		t.Fatalf("OpenObject %s/%s@%s: %v", bucket, key, versionID, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// dump describes everything stored for bucket on b: bucket settings,
// configuration documents and every version (identity, metadata, data
// digest), in order.
func dump(t *testing.T, b storage.Backend, bucket string) string {
	t.Helper()
	ctx := context.Background()
	var sb strings.Builder
	info, err := b.GetBucket(ctx, bucket)
	if errors.Is(err, storage.ErrBucketNotFound) {
		return "<no bucket>"
	}
	if err != nil {
		t.Fatalf("GetBucket: %v", err)
	}
	fmt.Fprintf(&sb, "versioning=%q lock=%v\n", info.Versioning, info.ObjectLockEnabled)
	for _, k := range storage.AllConfigKeys {
		data, err := b.GetBucketConfig(ctx, bucket, k)
		if err == nil {
			fmt.Fprintf(&sb, "config %s=%q\n", k, data)
		}
	}
	err = b.WalkVersions(ctx, bucket, "", "", "", func(oi storage.ObjectInfo) error {
		digest := "-"
		if !oi.DeleteMarker {
			_, rc, err := b.OpenObject(ctx, bucket, oi.Key, oi.VersionID)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			s := sha256.Sum256(data)
			digest = hex.EncodeToString(s[:8])
		}
		tags := make([]string, 0, len(oi.Tags))
		for k, v := range oi.Tags {
			tags = append(tags, k+"="+v)
		}
		sort.Strings(tags)
		fmt.Fprintf(&sb, "%s %s latest=%v dm=%v size=%d mtime=%d etag=%s tags=%v ct=%q data=%s\n",
			oi.Key, oi.VersionID, oi.IsLatest, oi.DeleteMarker, oi.Size, oi.LastModified.UnixNano(), oi.ETag, tags, oi.ContentType, digest)
		return nil
	})
	if err != nil {
		t.Fatalf("WalkVersions: %v", err)
	}
	return sb.String()
}

// waitSame waits until the bucket's copies on every given backing are
// identical.
func waitSame(t *testing.T, bucket string, bs ...storage.Backend) string {
	t.Helper()
	var last []string
	waitFor(t, "identical copies of "+bucket, func() bool {
		last = last[:0]
		for _, b := range bs {
			last = append(last, dump(t, b, bucket))
		}
		for _, d := range last[1:] {
			if d != last[0] {
				return false
			}
		}
		return true
	})
	return last[0]
}

// faulty wraps a backend and fails every call while down is set.
type faulty struct {
	storage.Backend
	down atomic.Bool
}

var errFault = errors.New("injected disk failure")

func (f *faulty) err() error {
	if f.down.Load() {
		return errFault
	}
	return nil
}

func (f *faulty) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	return f.Backend.ListBuckets(ctx)
}

func (f *faulty) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	if err := f.err(); err != nil {
		return storage.BucketInfo{}, err
	}
	return f.Backend.GetBucket(ctx, name)
}

func (f *faulty) GetObjectInfo(ctx context.Context, bucket, key, v string) (storage.ObjectInfo, error) {
	if err := f.err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	return f.Backend.GetObjectInfo(ctx, bucket, key, v)
}

func (f *faulty) OpenObject(ctx context.Context, bucket, key, v string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if err := f.err(); err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	return f.Backend.OpenObject(ctx, bucket, key, v)
}

func (f *faulty) NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	return f.Backend.NewObjectWriter(ctx, bucket, key)
}

func (f *faulty) DeleteObject(ctx context.Context, bucket, key, v string) (storage.DeleteResult, error) {
	if err := f.err(); err != nil {
		return storage.DeleteResult{}, err
	}
	return f.Backend.DeleteObject(ctx, bucket, key, v)
}

func (f *faulty) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	if err := f.err(); err != nil {
		return err
	}
	return f.Backend.WalkVersions(ctx, bucket, prefix, km, vm, fn)
}

func (f *faulty) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if err := f.err(); err != nil {
		return err
	}
	return f.Backend.WalkObjects(ctx, bucket, prefix, after, fn)
}

func (f *faulty) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	return f.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}

func (f *faulty) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	return f.Backend.GetBucketConfig(ctx, bucket, key)
}
