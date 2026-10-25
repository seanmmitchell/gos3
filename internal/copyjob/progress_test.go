package copyjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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

func requestComparison(e *env, j jobs.Job) jobs.Job {
	e.t.Helper()
	before := statusOf(e.t, e.waitIdle(j.ID)).Passes
	e.s.RequestFullPass(j)
	e.m.Kick(j.ID)
	return e.waitJob(j.ID, "a requested full comparison", func(j jobs.Job) bool {
		return j.State == jobs.StateWaiting && statusOf(e.t, j).Passes > before
	})
}

func liveStatus(t *testing.T, e *env, id string) Status {
	t.Helper()
	j, err := e.m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	j.Status = e.s.JobStatus(j)
	return statusOf(t, j)
}

func TestMirrorProgressSeparatesPassAndLifetime(t *testing.T) {
	for _, body := range []string{"", "body"} {
		t.Run(fmt.Sprintf("bytes-%d", len(body)), func(t *testing.T) {
			r := openRouter(t, memBacking("a"))
			createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
			e := newEnv(t, r, t.TempDir())
			j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src", "prefix": "in/"}, "target": map[string]any{"bucket": "dst", "prefix": "out/", "create": map[string]any{}}, "options": map[string]any{"versions": "current"}})
			initial := statusOf(t, e.waitIdle(j.ID))
			if !initial.PassComplete || !initial.TotalsKnown || initial.TotalKeys != 0 || initial.BytesDone != 0 {
				t.Fatalf("empty pass: %+v", initial)
			}
			put(t, r, "src", "ignored", "outside prefix")
			for i := range 4 {
				before := liveStatus(t, e, j.ID).KeysCopied
				key := fmt.Sprintf("in/k%d", i%2)
				put(t, r, "src", key, body)
				e.waitJob(j.ID, "incremental copy", func(j jobs.Job) bool {
					return j.State == jobs.StateWaiting && statusOf(t, j).KeysCopied > before
				})
				st := liveStatus(t, e, j.ID)
				if st.BytesDone != initial.BytesDone || st.SourceKeysDone != 0 || st.TotalKeys != 0 || st.Pending != 0 || st.Unresolved != 0 {
					t.Fatalf("incremental counters: %+v", st)
				}
			}
			for range 2 {
				st := statusOf(t, requestComparison(e, j))
				if st.TotalKeys != 2 || st.SourceKeysDone != 2 || st.KeysScanned != 2 || st.TotalBytes != int64(2*len(body)) || st.BytesDone != st.TotalBytes || st.KeysCopied != 4 || !st.PassComplete {
					t.Fatalf("full pass: %+v", st)
				}
			}
		})
	}
}

// Both comparison modes repair a missing target, same-size target drift,
// and source writes that bypass notifications. Equal sizes alone do not
// establish agreement. A restart must also perform a fresh comparison.
func TestMirrorManualComparisonRepairsDrift(t *testing.T) {
	for _, fingerprint := range []bool{false, true} {
		t.Run(fmt.Sprintf("fingerprint-%t", fingerprint), func(t *testing.T) {
			src := memory.New()
			var backend storage.Backend = memory.New()
			if fingerprint {
				backend = plainBackend{backend.(plainMember)}
			}
			r := openRouter(t, multi.Backing{Name: "a", Kind: "memory", Backend: src}, multi.Backing{Name: "b", Kind: "memory", Backend: backend})
			createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
			put(t, r, "src", "x", "aaaa")
			dir := t.TempDir()
			e := newEnv(t, r, dir)
			j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst", "create": map[string]any{"backing": "b"}}, "options": map[string]any{"versions": "current"}})
			e.waitIdle(j.ID)
			for _, drift := range []func(){
				func() { del(t, r, "dst", "x", "") },
				func() { put(t, r, "dst", "x", "bbbb") },
				func() { put(t, src, "src", "x", "cccc") },
			} {
				drift()
				st := statusOf(t, requestComparison(e, j))
				if read(t, r, "dst", "x") != read(t, r, "src", "x") || st.Unresolved != 0 || st.Pending != 0 || st.CheckRequired {
					t.Fatalf("drift not reconciled: %+v", st)
				}
			}
			e.close()
			put(t, r, "dst", "x", "dddd")
			e2 := newEnv(t, r, dir)
			e2.waitIdle(j.ID)
			if read(t, r, "dst", "x") != "cccc" {
				t.Fatal("restart did not reconcile drift")
			}
		})
	}
}

type progressFault struct {
	*memory.Backend
	fail     atomic.Bool
	walkFail atomic.Bool
}

func (b *progressFault) WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error {
	if b.walkFail.Load() {
		return errors.New("injected listing failure")
	}
	return b.Backend.WalkObjects(ctx, bucket, prefix, after, fn)
}

func (b *progressFault) OpenObject(ctx context.Context, bucket, key, vid string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if b.fail.Load() {
		return storage.ObjectInfo{}, nil, errors.New("injected copy failure")
	}
	return b.Backend.OpenObject(ctx, bucket, key, vid)
}

func TestMirrorUnresolvedSurvivesFailureAndRecovers(t *testing.T) {
	src := &progressFault{Backend: memory.New()}
	r := openRouter(t, multi.Backing{Name: "a", Kind: "memory", Backend: src}, memBacking("b"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	put(t, r, "src", "x", "aaaa")
	e := newEnv(t, r, t.TempDir())
	j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst", "create": map[string]any{"backing": "b"}}, "options": map[string]any{"versions": "current"}})
	e.waitIdle(j.ID)
	src.fail.Store(true)
	put(t, r, "src", "x", "bbbb")
	e.waitJob(j.ID, "known failed work", func(j jobs.Job) bool { return j.State == jobs.StateWaiting && statusOf(t, j).Unresolved == 1 })
	st := statusOf(t, requestComparison(e, j))
	if st.Unresolved != 1 || st.Pending != 1 || len(st.Errors) == 0 {
		t.Fatalf("failed check lost outstanding work: %+v", st)
	}
	src.walkFail.Store(true)
	e.s.RequestFullPass(j)
	e.m.Kick(j.ID)
	e.waitJob(j.ID, "a failed full comparison", func(j jobs.Job) bool { return j.State == jobs.StateWaiting && j.LastError != "" })
	st = liveStatus(t, e, j.ID)
	if st.Unresolved != 1 || !st.CheckRequired || st.PassComplete {
		t.Fatalf("failed listing replaced known work: %+v", st)
	}
	e.close()
	src.walkFail.Store(false)
	src.fail.Store(false)
	e = newEnv(t, r, e.dir)
	st = statusOf(t, e.waitIdle(j.ID))
	if st.Unresolved != 0 || st.Pending != 0 || len(st.Errors) == 0 || read(t, r, "dst", "x") != "bbbb" {
		t.Fatalf("recovery: %+v", st)
	}
}

func TestUnresolvedStorePersistsDistinctKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	b, err := openBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.SetUnresolved("x", true) != 1 || b.SetUnresolved("x", true) != 0 {
		t.Fatal("duplicate key counted")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b, err = openBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(b.UnresolvedKeys()) != 1 || b.SetUnresolved("x", false) != -1 || len(b.UnresolvedKeys()) != 0 {
		t.Fatal("unresolved state was lost")
	}
}

func TestMirrorConflictSurvivesRestartAndClearsOnRecovery(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	createBucket(t, r, "src", storage.CreateBucketOptions{ObjectLockEnabled: true}, storage.VersioningEnabled)
	put(t, r, "src", "x", "aaaa")
	dir := t.TempDir()
	e := newEnv(t, r, dir)
	j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst", "create": map[string]any{}}})
	e.waitIdle(j.ID)
	v := versions(t, r, "dst", "x")[0]
	setHold := func(hold bool) {
		_, err := r.UpdateObjectMeta(t.Context(), "dst", "x", v.VersionID, func(m *storage.ObjectMeta) error { m.LegalHold = hold; return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	setHold(true)
	del(t, r, "src", "x", v.VersionID)
	e.waitJob(j.ID, "a deletion conflict", func(j jobs.Job) bool { return j.State == jobs.StateWaiting && statusOf(t, j).Unresolved == 1 })
	e.close()
	e2 := newEnv(t, r, dir)
	st := statusOf(t, e2.waitIdle(j.ID))
	if st.Unresolved != 1 || st.Pending != 0 || st.Conflicts == 0 {
		t.Fatalf("lost conflict after restart: %+v", st)
	}
	setHold(false)
	st = statusOf(t, requestComparison(e2, j))
	if st.Unresolved != 0 || len(keys(t, r, "dst")) != 0 {
		t.Fatalf("conflict recovery: %+v", st)
	}
}

// Source growth while measuring invalidates the estimate instead of
// clamping impossible ratios. Lifetime activity remains independent.
func TestPassProgressInvalidatesGrowingTotals(t *testing.T) {
	tr := newTracker(Status{TotalKeys: 1, TotalBytes: 4, TotalsKnown: true})
	p := copier.KeyPair{SrcKey: "x", Src: []storage.ObjectInfo{{Key: "x", Size: 4}}}
	tr.record(p, false, copier.Result{VersionsCopied: 1, BytesCopied: 4}, true)
	p.SrcKey, p.Src[0].Key = "y", "y"
	tr.record(p, false, copier.Result{VersionsCopied: 1, BytesCopied: 4}, true)
	st := tr.snapshot()
	if st.TotalsKnown || st.SourceKeysDone != 2 || st.BytesDone != 8 {
		t.Fatalf("growth: %+v", st)
	}
	tr.record(p, false, copier.Result{VersionsCopied: 1, BytesCopied: 4}, false)
	st = tr.snapshot()
	if st.KeysCopied != 3 || st.SourceKeysDone != 2 || st.BytesDone != 8 {
		t.Fatalf("incremental work: %+v", st)
	}
}

func TestMirrorPassCountsAllVersionsAndMarkers(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	put(t, r, "src", "x", "aaaa")
	put(t, r, "src", "x", "bbbb")
	del(t, r, "src", "x", "")
	e := newEnv(t, r, "")
	j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst", "create": map[string]any{}}})
	st := statusOf(t, e.waitIdle(j.ID))
	if st.TotalKeys != 1 || st.SourceKeysDone != 1 || st.BytesDone != 8 || st.VersionsCopied != 3 || len(versions(t, r, "dst", "x")) != 3 {
		t.Fatalf("versions progress: %+v", st)
	}
}

func TestMirrorResolvesUnmappableKeyRemovedFromSource(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	key := strings.Repeat("k", copier.MaxKeyLen)
	put(t, r, "src", key, "body")
	e := newEnv(t, r, "")
	j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst", "prefix": "out/", "create": map[string]any{}}})
	st := statusOf(t, e.waitIdle(j.ID))
	if st.Unresolved != 1 {
		t.Fatalf("expected mapping conflict: %+v", st)
	}
	del(t, r, "src", key, "")
	e.waitJob(j.ID, "the removed key to resolve", func(j jobs.Job) bool {
		st := statusOf(t, j)
		return j.State == jobs.StateWaiting && st.Unresolved == 0 && st.Pending == 0
	})
}

type quickVerifyRunner struct {
	jobs.Runner
	native bool
}

func (r quickVerifyRunner) Validate(ctx context.Context, spec json.RawMessage) (json.RawMessage, map[string]string, error) {
	norm, labels, err := r.Runner.Validate(ctx, spec)
	if err != nil {
		return norm, labels, err
	}
	// Shorten only the normalized test spec, after public validation.
	var sp Spec
	if err := json.Unmarshal(norm, &sp); err != nil {
		return nil, nil, err
	}
	if r.native {
		sp.Options.PollInterval = "80ms"
	} else {
		sp.Options.VerifyInterval = "80ms"
	}
	norm, err = json.Marshal(sp)
	return norm, labels, err
}

func TestScheduledComparisonReconcilesUnnotifiedChanges(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native-%t", native), func(t *testing.T) { testScheduledComparison(t, native) })
	}
}

type pollingNativeSource struct{ *externalNativeIdentity }

func (b *pollingNativeSource) BucketCaps(context.Context, string) (storage.BucketCaps, error) {
	return storage.BucketCaps{Kind: storage.BucketKindNative, Clone: true, Versioning: true, ObjectLock: true}, nil
}

func (b *pollingNativeSource) NewBucketCaps(ctx context.Context, _ storage.CreateBucketOptions) (storage.BucketCaps, error) {
	return b.BucketCaps(ctx, "")
}

func testScheduledComparison(t *testing.T, native bool) {
	src := memory.New()
	backing := multi.Backing{Name: "a", Kind: "memory", Backend: src}
	if native {
		backing.Kind, backing.Backend = "s3", &pollingNativeSource{&externalNativeIdentity{Backend: src, remoteCreatedAt: time.Now().UTC()}}
	}
	r := openRouter(t, backing, memBacking("b"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	put(t, r, "src", "x", "aaaa")
	s := New(Options{Router: r, Logger: testLogger()})
	m, err := jobs.Open(jobs.Options{Runners: map[string]jobs.Runner{KindMirror: quickVerifyRunner{s.Runner(KindMirror), native}}, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	s.Attach(m)
	e := &env{t: t, r: r, s: s, m: m}
	t.Cleanup(e.close)
	j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst", "create": map[string]any{"backing": "b"}}})
	before := statusOf(t, e.waitIdle(j.ID))
	put(t, src, "src", "x", strings.Repeat("b", 4))
	e.waitJob(j.ID, "a scheduled comparison", func(j jobs.Job) bool { return j.State == jobs.StateWaiting && statusOf(t, j).Passes > before.Passes })
	if read(t, r, "dst", "x") != "bbbb" {
		t.Fatal("scheduled pass failed to repair same-size drift")
	}
}

func TestMirrorLiveQueueIncludesInFlight(t *testing.T) {
	s := New(Options{})
	defer s.Close()
	j := jobs.Job{ID: "mirror", Kind: KindMirror, State: jobs.StateWaiting, Spec: json.RawMessage(`{"source":{"bucket":"src"},"target":{"bucket":"dst"}}`)}
	w := s.register(j.ID, Spec{})
	w.setVerified()
	at := time.Now().Add(-10 * time.Second)
	w.markKey("x", at)
	_, since := w.takeDirty()
	w.setInFlight(since)
	w.markKey("x", at)
	var st Status
	if err := json.Unmarshal(s.JobStatus(j), &st); err != nil {
		t.Fatal(err)
	}
	if st.Pending != 1 || st.LagSeconds < 10 {
		t.Fatalf("in-flight status: %+v", st)
	}
	w.finishKey("x")
	if since["x"] != at {
		t.Fatal("finishing work changed retry timestamps")
	}
	s.RequestFullPass(j)
	if err := json.Unmarshal(s.JobStatus(j), &st); err != nil || !st.CheckRequired {
		t.Fatalf("manual request: %+v, %v", st, err)
	}
}
