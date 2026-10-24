package copyjob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// The ordinary bucket description stays cached when an external native
// bucket is replaced. Only the job-specific identity read sees the remote.
type externalNativeIdentity struct {
	*memory.Backend
	remoteCreatedAt time.Time
}

func (b *externalNativeIdentity) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	info, err := b.Backend.GetBucket(ctx, name)
	info.Kind = storage.BucketKindNative
	info.Location = "s3://remote/"
	return info, err
}

func (b *externalNativeIdentity) GetBucketIdentity(ctx context.Context, name string) (storage.BucketInfo, error) {
	info, err := b.GetBucket(ctx, name)
	info.CreatedAt = b.remoteCreatedAt
	return info, err
}

func TestCopyJobChecksExternalNativeIncarnation(t *testing.T) {
	native := &externalNativeIdentity{Backend: memory.New(), remoteCreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	router := openRouter(t, multi.Backing{Name: "external", Kind: "s3", Backend: native}, memBacking("local"))
	createBucket(t, router, "src", storage.CreateBucketOptions{Backing: "external"}, storage.VersioningUnversioned)
	createBucket(t, router, "dst", storage.CreateBucketOptions{Backing: "local"}, storage.VersioningUnversioned)
	s := New(Options{Router: router, Logger: testLogger()})
	t.Cleanup(s.Close)
	sp, _, err := s.check(t.Context(), KindCopy, []byte(`{"source":{"bucket":"src"},"target":{"bucket":"dst"},"options":{"onExisting":"merge"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !sp.BucketBindings.Source.CreatedAt.Equal(native.remoteCreatedAt) {
		t.Fatalf("job pinned a cached creation time: %+v", sp.BucketBindings.Source)
	}
	r := &run{s: s, sp: sp, cp: checkpoint{Phase: phaseCopying}, tr: newTracker(Status{})}
	if err := r.refresh(t.Context()); err != nil {
		t.Fatalf("unchanged remote: %v", err)
	}
	native.remoteCreatedAt = native.remoteCreatedAt.Add(time.Hour)
	if err := r.refresh(t.Context()); !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "source bucket") {
		t.Fatalf("replaced remote source: %v", err)
	}
}

func TestCopyJobPinsBucketIncarnations(t *testing.T) {
	router := openRouter(t, memBacking("a"))
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	createBucket(t, router, "dst", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	s := New(Options{Router: router, Logger: testLogger()})
	t.Cleanup(s.Close)
	sp, _, err := s.check(t.Context(), KindCopy, []byte(`{"source":{"bucket":"src"},"target":{"bucket":"dst"},"options":{"onExisting":"merge"}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if sp.BucketBindings.Source.empty() || sp.BucketBindings.Target.empty() {
		t.Fatalf("missing bucket bindings: %+v", sp.BucketBindings)
	}
	makeRun := func() *run { return &run{s: s, sp: sp, cp: checkpoint{Phase: phaseCopying}, tr: newTracker(Status{})} }
	if err := makeRun().refresh(t.Context()); err != nil {
		t.Fatalf("unchanged buckets: %v", err)
	}
	if err := router.DeleteBucket(context.Background(), "src"); err != nil {
		t.Fatal(err)
	}
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	if err := makeRun().refresh(t.Context()); !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "source bucket") {
		t.Fatalf("replaced source: %v", err)
	}
	newSource, err := router.GetBucket(t.Context(), "src")
	if err != nil {
		t.Fatal(err)
	}
	sp.BucketBindings.Source = identityOf(newSource)
	if err := router.DeleteBucket(context.Background(), "dst"); err != nil {
		t.Fatal(err)
	}
	createBucket(t, router, "dst", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	if err := makeRun().refresh(t.Context()); !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "target bucket") {
		t.Fatalf("replaced target: %v", err)
	}
}

// A key with more versions than copier.MaxKeyVersions fails alone: the
// mirror copies every other key, reports that one in its errors and keeps
// running (retrying it), instead of failing for good at that key.
func TestMirrorSkipsKeyWithTooManyVersions(t *testing.T) {
	router := openRouter(t, memBacking("a"))
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	createBucket(t, router, "dst", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	put(t, router, "src", "aaa-before", "a")
	for range copier.MaxKeyVersions + 1 {
		put(t, router, "src", "u/x", "x")
	}
	put(t, router, "src", "zzz-after", "z")
	e := newEnv(t, router, "")
	j := e.mustSubmit(map[string]any{"kind": KindMirror, "source": map[string]any{"bucket": "src"},
		"target": map[string]any{"bucket": "dst"}})
	j = e.waitJob(j.ID, "the mirror to watch with the hot key failed", func(j jobs.Job) bool {
		if j.State == jobs.StateFailed {
			t.Fatalf("mirror failed: %s", j.LastError)
		}
		if j.State != jobs.StateWaiting || j.Phase != phaseWatching || len(j.Status) == 0 {
			return false
		}
		for _, ke := range statusOf(t, j).Errors {
			if ke.Key == "u/x" && strings.Contains(ke.Error, "too many versions") {
				return true
			}
		}
		return false
	})
	if j.LastError != "" {
		t.Fatalf("lastError %q", j.LastError)
	}
	if got := keys(t, router, "dst"); !slices.Equal(got, []string{"aaa-before", "zzz-after"}) {
		t.Fatalf("target keys %v", got)
	}
}

// A copy job's final pass fails while a key cannot be copied, so the job
// is retried rather than reported as done; the other keys are copied.
func TestCopySkipsKeyWithTooManyVersions(t *testing.T) {
	router := openRouter(t, memBacking("a"))
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	put(t, router, "src", "aaa-before", "a")
	for range copier.MaxKeyVersions + 1 {
		put(t, router, "src", "u/x", "x")
	}
	put(t, router, "src", "zzz-after", "z")
	e := newEnv(t, router, "")
	j := e.mustSubmit(map[string]any{"kind": KindCopy, "source": map[string]any{"bucket": "src"},
		"target": map[string]any{"bucket": "dst", "create": map[string]any{}}})
	j = e.waitJob(j.ID, "the copy to retry its final pass", func(j jobs.Job) bool {
		if j.State.Finished() {
			t.Fatalf("copy finished: %s %s", j.State, j.LastError)
		}
		return j.State == jobs.StateWaiting && strings.Contains(j.LastError, "1 keys could not be copied")
	})
	if got := keys(t, router, "dst"); !slices.Equal(got, []string{"aaa-before", "zzz-after"}) {
		t.Fatalf("target keys %v", got)
	}
	found := false
	for _, ke := range statusOf(t, j).Errors {
		found = found || ke.Key == "u/x" && strings.Contains(ke.Error, "too many versions")
	}
	if !found {
		t.Fatalf("errors %+v", statusOf(t, j).Errors)
	}
}

// failingReads fails reads from the bucket "src" with an error that a
// rerun would meet again while fail is set.
type failingReads struct {
	*memory.Backend
	fail atomic.Bool
}

func (b *failingReads) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if b.fail.Load() && bucket == "src" {
		return storage.ObjectInfo{}, nil, fmt.Errorf("injected: %w", storage.ErrBucketNotFound)
	}
	return b.Backend.OpenObject(ctx, bucket, key, versionID)
}

func TestFailedMirrorRechecksConflictsOnResume(t *testing.T) {
	backend := &failingReads{Backend: memory.New()}
	backend.fail.Store(true)
	router := openRouter(t, multi.Backing{Name: "a", Kind: "memory", Backend: backend})
	createBucket(t, router, "src", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	createBucket(t, router, "other", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	createBucket(t, router, "dst", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	put(t, router, "src", "k", "x")
	e := newEnv(t, router, "")
	first := e.mustSubmit(map[string]any{"kind": KindMirror, "source": map[string]any{"bucket": "src"},
		"target": map[string]any{"bucket": "dst"}})
	failed := e.waitState(first.ID, jobs.StateFailed)
	if !strings.Contains(failed.LastError, "injected") || failed.Phase == phasePreparing {
		t.Fatalf("first failure: %s in phase %s", failed.LastError, failed.Phase)
	}
	backend.fail.Store(false)
	second := e.mustSubmit(map[string]any{"kind": KindMirror, "source": map[string]any{"bucket": "other"},
		"target": map[string]any{"bucket": "dst"}})
	e.waitIdle(second.ID)
	if err := e.m.Resume(first.ID); err != nil {
		t.Fatal(err)
	}
	resumed := e.waitJob(first.ID, "resumed mirror conflict", func(j jobs.Job) bool {
		return j.State == jobs.StateFailed && strings.Contains(j.LastError, second.ID)
	})
	if resumed.Phase == phasePreparing {
		t.Fatalf("conflict was not checked after preparation: %+v", resumed)
	}
}
