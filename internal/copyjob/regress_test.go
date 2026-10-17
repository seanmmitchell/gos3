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

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// Regression tests from the review of the copy jobs.

// With prefixes, a copied policy grants on the target only what the
// source's policy granted on the keys the target keys are copies of
// (review: TestScratchPolicyRewritePrefix).
func TestPolicyRewritePrefix(t *testing.T) {
	m, err := iam.Open(iam.Options{RootAccessKey: "rootrootroot", RootSecretKey: "rootrootrootrootroot"})
	if err != nil {
		t.Fatal(err)
	}
	// allowed reports whether anonymous GetObject of data-copy/key is
	// allowed by policy out.
	allowed := func(out []byte, key string) bool {
		t.Helper()
		doc, err := iam.ParseBucketPolicy(out, "data-copy")
		if err != nil {
			t.Fatalf("rewritten policy %s: %v", out, err)
		}
		return m.Authorize(t.Context(), iam.AuthzRequest{Identity: auth.Anonymous, Action: "s3:GetObject", Bucket: "data-copy", Key: key, BucketPolicy: doc}).Allowed
	}
	spec := func(sp, tp string) *Spec {
		return &Spec{Kind: KindCopy, Source: SourceSpec{Bucket: "data", Prefix: sp}, Target: TargetSpec{Bucket: "data-copy", Prefix: tp}}
	}
	prefixes := []struct{ sp, tp string }{{"internal/", ""}, {"", "bak/"}, {"internal/", "bak/"}}

	// The reviewer's policy: public/ is public, listable with an s3:prefix
	// condition. No mapping is safe: it is not copied.
	withCondition := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/public/*"},
{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::data","Condition":{"StringLike":{"s3:prefix":"public/*"}}}]}`
	for _, c := range prefixes {
		if out, reason := rewritePolicy([]byte(withCondition), spec(c.sp, c.tp)); reason == "" {
			t.Errorf("prefixes %q -> %q: the policy with an s3:prefix condition was copied: %s", c.sp, c.tp, out)
		}
	}

	// Without the condition: public/ of the source is only copied (as
	// bak/public/) with an empty source prefix.
	publicOnly := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/public/*"},
{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::data"}]}`
	for _, c := range prefixes {
		out, reason := rewritePolicy([]byte(publicOnly), spec(c.sp, c.tp))
		if c.sp == "internal/" {
			// No copied key is public: nothing to copy.
			if reason == "" {
				for _, key := range []string{"public/x", "bak/public/x", "x"} {
					if allowed(out, key) {
						t.Errorf("prefixes %q -> %q: data-copy/%s is public: %s", c.sp, c.tp, key, out)
					}
				}
			}
			continue
		}
		if reason != "" {
			t.Fatalf("prefixes %q -> %q: %s", c.sp, c.tp, reason)
		}
		if !allowed(out, "bak/public/x") || allowed(out, "public/x") || allowed(out, "bak/x") {
			t.Errorf("prefixes %q -> %q: wrong grants: %s", c.sp, c.tp, out)
		}
		if strings.Contains(string(out), "ListBucket") {
			t.Errorf("prefixes %q -> %q: a bucket-wide grant was kept: %s", c.sp, c.tp, out)
		}
	}

	// Patterns: exact within the prefix, every copied key, disjoint, and a
	// Deny that partly covers the copied keys and grows.
	mixed := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject",
 "Resource":["arn:aws:s3:::data/internal/pub/*","arn:aws:s3:::data/pub*","arn:aws:s3:::data/intern"]}]}`
	out, reason := rewritePolicy([]byte(mixed), spec("internal/", "bak/"))
	if reason != "" {
		t.Fatal(reason)
	}
	if !allowed(out, "bak/pub/x") || allowed(out, "pub/x") || allowed(out, "bak/other") || allowed(out, "intern") {
		t.Errorf("mixed patterns: wrong grants: %s", out)
	}
	everything := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*"},
 {"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*/secret"}]}`
	out, reason = rewritePolicy([]byte(everything), spec("internal/", "bak/"))
	if reason != "" {
		t.Fatal(reason)
	}
	if !strings.Contains(string(out), `"arn:aws:s3:::data-copy/bak/*"`) || allowed(out, "bak/x") || allowed(out, "other") {
		t.Errorf("a partial Deny must cover every copied key: %s", out)
	}
	// A wildcard bucket resource validates for both source and target but
	// must not bypass prefix scoping.
	global := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::*/*"}]}`
	out, reason = rewritePolicy([]byte(global), spec("internal/", "bak/"))
	if reason == "" && allowed(out, "outside/secret") {
		t.Fatalf("global resource escaped prefix: %s (%s)", out, reason)
	}
	for _, prefixes := range [][2]string{{"internal/", "*/"}, {"internal/", "?/"}, {"*/", "bak/"}} {
		if out, reason := rewritePolicy([]byte(global), spec(prefixes[0], prefixes[1])); reason == "" {
			t.Errorf("literal wildcard prefix became a policy pattern: %s", out)
		}
	}
	variableDeny := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*"},
{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/internal/\u0024{aws:username}/*"}]}`
	if out, reason := rewritePolicy([]byte(variableDeny), spec("internal/bob/", "bak/")); reason == "" {
		t.Fatalf("variable-based Deny could be dropped: %s", out)
	}
}

// A mirror's target loses the policy it copied once the source's policy
// can no longer be copied (review finding 8).
func TestMirrorDropsStalePolicy(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	ctx := context.Background()
	createBucket(t, r, "pol-src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	good := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pol-src/*"}]}`
	if err := r.PutBucketConfig(ctx, "pol-src", storage.ConfigPolicy, []byte(good)); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, r, "")
	j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "pol-src"},
		"target": map[string]any{"bucket": "pol-dst", "create": map[string]any{}}})
	e.waitIdle(j.ID)
	if _, err := r.GetBucketConfig(ctx, "pol-dst", storage.ConfigPolicy); err != nil {
		t.Fatalf("policy not copied: %v", err)
	}
	bad := `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::elsewhere/*"}]}`
	if err := r.PutBucketConfig(ctx, "pol-src", storage.ConfigPolicy, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stale policy to be deleted", func() bool {
		_, err := r.GetBucketConfig(ctx, "pol-dst", storage.ConfigPolicy)
		return errors.Is(err, storage.ErrConfigNotFound)
	})
	st := statusOf(t, e.waitIdle(j.ID))
	if !containsText(st.Warnings, "policy copied earlier was deleted") {
		t.Errorf("warnings %q", st.Warnings)
	}
}

// The source's public access block and ownership controls that neutralize
// object ACLs reach the target even with configs "none", since copies keep
// their ACLs (review finding 3).
func TestACLGuardsAlwaysCopied(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	ctx := context.Background()
	createBucket(t, r, "guarded", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	createBucket(t, r, "open", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	pab := `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls></PublicAccessBlockConfiguration>`
	own := `<OwnershipControls><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`
	weak := `<PublicAccessBlockConfiguration><BlockPublicPolicy>true</BlockPublicPolicy></PublicAccessBlockConfiguration>`
	for b, docs := range map[string]map[string]string{
		"guarded": {storage.ConfigPublicAccessBlock: pab, storage.ConfigOwnership: own, storage.ConfigCORS: "<CORSConfiguration/>"},
		"open":    {storage.ConfigPublicAccessBlock: weak},
	} {
		for k, v := range docs {
			if err := r.PutBucketConfig(ctx, b, k, []byte(v)); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(t, r, "guarded", "k", "v")
	put(t, r, "open", "k", "v")
	e := newEnv(t, r, "")
	spec := func(src, dst string) map[string]any {
		return map[string]any{"kind": "copy", "source": map[string]any{"bucket": src},
			"target": map[string]any{"bucket": dst, "create": map[string]any{}}, "options": map[string]any{"configs": "none"}}
	}
	_, warnings, err := e.s.DryRun(ctx, mustJSON(t, spec("guarded", "guarded-copy")))
	if err != nil {
		t.Fatal(err)
	}
	if !containsText(warnings, "public-access-block configuration is copied although") || !containsText(warnings, "ownership configuration is copied although") {
		t.Errorf("warnings %q", warnings)
	}
	e.waitState(e.mustSubmit(spec("guarded", "guarded-copy")).ID, jobs.StateSucceeded)
	for k, want := range map[string]string{storage.ConfigPublicAccessBlock: pab, storage.ConfigOwnership: own} {
		if got, err := r.GetBucketConfig(ctx, "guarded-copy", k); err != nil || string(got) != want {
			t.Errorf("%s on the target: %q, %v", k, got, err)
		}
	}
	if _, err := r.GetBucketConfig(ctx, "guarded-copy", storage.ConfigCORS); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Errorf("configs none copied cors (%v)", err)
	}
	// A public access block that does not neutralize ACLs follows configs.
	e.waitState(e.mustSubmit(spec("open", "open-copy")).ID, jobs.StateSucceeded)
	if _, err := r.GetBucketConfig(ctx, "open-copy", storage.ConfigPublicAccessBlock); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Errorf("configs none copied a public access block that guards nothing (%v)", err)
	}
}

// onExisting "adopt" is for mirrors, which then own the whole target: keys
// the source lacks are deleted (review: TestScratchAdoptDeletesExtraneous).
func TestAdoptDeletesExtraneous(t *testing.T) {
	r := openRouter(t, memBacking("a"), memBacking("b"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	createBucket(t, r, "dst", storage.CreateBucketOptions{Backing: "b"}, storage.VersioningUnversioned)
	put(t, r, "src", "k", "v1")
	put(t, r, "dst", "extra", "old")
	put(t, r, "dst", "k", "old")
	e := newEnv(t, r, "")
	if _, err := e.submit(map[string]any{"kind": "copy", "source": map[string]any{"bucket": "src"},
		"target": map[string]any{"bucket": "dst"}, "options": map[string]any{"onExisting": "adopt"}}); !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Errorf("a copy with onExisting adopt: %v, want ErrInvalidSpec", err)
	}
	j := e.mustSubmit(map[string]any{
		"kind":    "mirror",
		"source":  map[string]any{"bucket": "src"},
		"target":  map[string]any{"bucket": "dst"},
		"options": map[string]any{"onExisting": "adopt"},
	})
	e.waitIdle(j.ID)
	waitFor(t, "the target to be taken over", func() bool { return slices.Equal(keys(t, r, "dst"), []string{"k"}) })
	if got := read(t, r, "dst", "k"); got != "v1" {
		t.Errorf("k = %q", got)
	}
}

// Two jobs needing an empty target cannot share one (review finding 7).
func TestEmptyTargetNotShared(t *testing.T) {
	cp := func(on string) *Spec {
		return &Spec{Kind: KindCopy, Source: SourceSpec{Bucket: "s2"}, Target: TargetSpec{Bucket: "t", Prefix: "a/"},
			Options: SpecOptions{OnExisting: on}}
	}
	others := []otherJob{{id: "j1", kind: KindCopy, src: keyRange{"s1", ""}, dst: keyRange{"t", ""}}}
	if err := checkJobs(cp(OnExistingFail), others); !errors.Is(err, jobs.ErrConflict) {
		t.Errorf("a second copy into the target of an active copy: %v, want ErrConflict", err)
	}
	if err := checkJobs(cp(OnExistingMerge), others); err != nil {
		t.Errorf("merging into the target of an active copy: %v", err)
	}
}

func mirrorSpecSlow() map[string]any {
	return map[string]any{
		"kind":    "mirror",
		"source":  map[string]any{"bucket": "src"},
		"target":  map[string]any{"bucket": "dst", "create": map[string]any{"backing": "b"}},
		"options": map[string]any{"concurrency": 1},
	}
}

// A dirty-set overflow recorded while a mirror's initial pass is paused
// restarts the pass, so changes to keys behind the checkpoint are copied
// (review: TestScratchResumeDiscardsOverflow).
func TestMirrorResumeAfterOverflow(t *testing.T) {
	slow := &slowBackend{Backend: memory.New(), delay: 15 * time.Millisecond}
	r := openRouter(t, multi.Backing{Name: "slow", Kind: "memory", Backend: slow}, memBacking("b"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	const n = 60
	for i := range n {
		put(t, r, "src", fmt.Sprintf("k%03d", i), "v1")
	}
	e := newEnv(t, r, "")
	j := e.mustSubmit(mirrorSpecSlow())
	waitFor(t, "part of the pass", func() bool { return slow.opens.Load() >= 20 })
	if err := e.m.Pause(j.ID); err != nil {
		t.Fatal(err)
	}
	e.waitState(j.ID, jobs.StatePaused)
	if got := read(t, r, "dst", "k000"); got != "v1" {
		t.Fatalf("k000 = %q", got)
	}
	put(t, r, "src", "k000", "v2")
	// As if more than maxDirty keys changed while paused.
	e.s.mu.Lock()
	w := e.s.watches[j.ID]
	e.s.mu.Unlock()
	w.mu.Lock()
	w.overflow, w.dirty = true, map[string]time.Time{}
	w.mu.Unlock()
	if err := e.m.Resume(j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the change behind the checkpoint to be copied", func() bool { return readOrEmpty(t, r, "dst", "k000") == "v2" })
	e.waitIdle(j.ID)
}

// failFirst makes the first opens of one key fail as if its version
// vanished.
type failFirst struct {
	*slowBackend
	key   string
	fails atomic.Int64
}

func (b *failFirst) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if bucket == "src" && key == b.key && b.fails.Add(1) <= 4 {
		return storage.ObjectInfo{}, nil, storage.ErrVersionNotFound
	}
	return b.slowBackend.OpenObject(ctx, bucket, key, versionID)
}

// A key that failed in a mirror pass that was then interrupted is copied
// later (review: TestScratchResumeForgetsFailedKeys).
func TestMirrorRetriesFailedKeysAfterPause(t *testing.T) {
	slow := &failFirst{slowBackend: &slowBackend{Backend: memory.New(), delay: 15 * time.Millisecond}, key: "k003"}
	r := openRouter(t, multi.Backing{Name: "slow", Kind: "memory", Backend: slow}, memBacking("b"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	const n = 60
	for i := range n {
		put(t, r, "src", fmt.Sprintf("k%03d", i), "v1")
	}
	e := newEnv(t, r, "")
	j := e.mustSubmit(mirrorSpecSlow())
	waitFor(t, "part of the pass", func() bool { return slow.opens.Load() >= 20 })
	if slow.fails.Load() < 4 {
		t.Fatalf("k003 was tried %d times before the pause", slow.fails.Load())
	}
	if err := e.m.Pause(j.ID); err != nil {
		t.Fatal(err)
	}
	e.waitState(j.ID, jobs.StatePaused)
	if err := e.m.Resume(j.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "k003 to be copied", func() bool { return slices.Contains(keys(t, r, "dst"), "k003") })
	e.waitIdle(j.ID)
	if got := len(keys(t, r, "dst")); got != n {
		t.Errorf("%d keys on the target, want %d", got, n)
	}
}
