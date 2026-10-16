package copyjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

const testPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
 "Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/*"]}]}`

// A copy under another name, onto another backing, keeps every version's
// ID and modification time, follows the source's versioning and copies the
// safe configuration with the policy's ARNs rewritten.
func TestCopyClonesBucket(t *testing.T) {
	r := openRouter(t, memBacking("a"), memBacking("b"))
	ctx := context.Background()
	createBucket(t, r, "photos", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	put(t, r, "photos", "k1", "one")
	time.Sleep(2 * time.Millisecond)
	put(t, r, "photos", "k1", "one, again")
	put(t, r, "photos", "k2", "two")
	del(t, r, "photos", "k2", "")
	put(t, r, "photos", "k3", "three")
	configs := map[string]string{
		storage.ConfigCORS:        "<CORSConfiguration><CORSRule><AllowedMethod>GET</AllowedMethod><AllowedOrigin>*</AllowedOrigin></CORSRule></CORSConfiguration>",
		storage.ConfigPolicy:      testPolicy,
		storage.ConfigReplication: "<ReplicationConfiguration/>",
		storage.ConfigLifecycle:   "<LifecycleConfiguration/>",
	}
	for k, v := range configs {
		if err := r.PutBucketConfig(ctx, "photos", k, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}

	e := newEnv(t, r, t.TempDir())
	j := e.mustSubmit(map[string]any{
		"kind":   "copy",
		"source": map[string]any{"bucket": "photos"},
		"target": map[string]any{"bucket": "photos-bak", "create": map[string]any{"backing": "b"}},
	})
	if j.Labels["source"] != "photos" || j.Labels["target"] != "photos-bak" {
		t.Errorf("labels %v", j.Labels)
	}
	j = e.waitState(j.ID, jobs.StateSucceeded)
	if j.Phase != phaseDone {
		t.Errorf("phase %q, want done", j.Phase)
	}

	info, err := r.GetBucket(ctx, "photos-bak")
	if err != nil {
		t.Fatal(err)
	}
	if info.Backing != "b" || info.Versioning != storage.VersioningEnabled || info.Owner != "tester" {
		t.Errorf("target bucket %+v: want backing b, versioning enabled, owner tester", info)
	}
	for _, key := range []string{"k1", "k2", "k3"} {
		sv, dv := versions(t, r, "photos", key), versions(t, r, "photos-bak", key)
		if len(sv) != len(dv) {
			t.Fatalf("%s: %d versions copied, want %d", key, len(dv), len(sv))
		}
		for i := range sv {
			s, d := sv[i], dv[i]
			if s.VersionID != d.VersionID || !s.LastModified.Equal(d.LastModified) || s.ETag != d.ETag || s.DeleteMarker != d.DeleteMarker {
				t.Errorf("%s version %d: copy %+v of %+v", key, i, d, s)
			}
		}
	}
	if got := read(t, r, "photos-bak", "k1"); got != "one, again" {
		t.Errorf("k1 = %q", got)
	}

	cors, err := r.GetBucketConfig(ctx, "photos-bak", storage.ConfigCORS)
	if err != nil || string(cors) != configs[storage.ConfigCORS] {
		t.Errorf("cors = %q, %v", cors, err)
	}
	pol, err := r.GetBucketConfig(ctx, "photos-bak", storage.ConfigPolicy)
	if err != nil {
		t.Fatalf("policy not copied: %v", err)
	}
	if !strings.Contains(string(pol), `"arn:aws:s3:::photos-bak/*"`) || !strings.Contains(string(pol), `"arn:aws:s3:::photos-bak"`) ||
		strings.Contains(string(pol), `"arn:aws:s3:::photos"`) {
		t.Errorf("policy not rewritten: %s", pol)
	}
	if _, err := r.GetBucketConfig(ctx, "photos-bak", storage.ConfigReplication); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Errorf("replication copied (err %v)", err)
	}
	if _, err := r.GetBucketConfig(ctx, "photos-bak", storage.ConfigLifecycle); err != nil {
		t.Errorf("lifecycle not copied by a copy: %v", err)
	}

	st := statusOf(t, j)
	if st.Mode != modeIdentity || st.VersionsCopied != 5 || st.KeysCopied != 3 || st.Passes != 2 || st.TotalKeys != 3 {
		t.Errorf("status %+v", st)
	}
	if st.BytesDone != st.TotalBytes || st.TotalBytes == 0 {
		t.Errorf("bytesDone %d, totalBytes %d", st.BytesDone, st.TotalBytes)
	}
	var cp checkpoint
	if err := json.Unmarshal(j.Checkpoint, &cp); err != nil || !cp.Created || cp.Phase != phaseDone {
		t.Errorf("checkpoint %s (%v)", j.Checkpoint, err)
	}
}

// onExisting "fail" refuses a target that is not empty; "merge" copies
// into it and keeps its keys.
func TestNonEmptyTarget(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	createBucket(t, r, "dst", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	put(t, r, "src", "a", "from src")
	put(t, r, "dst", "mine", "kept")
	e := newEnv(t, r, "")

	spec := map[string]any{"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst"}}
	if _, err := e.submit(spec); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("Submit onto a non-empty target: %v, want ErrConflict", err)
	}
	if _, _, err := e.s.DryRun(context.Background(), mustJSON(t, spec)); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("DryRun onto a non-empty target: %v, want ErrConflict", err)
	}
	// A prefix of the target that is empty is fine.
	spec["target"] = map[string]any{"bucket": "dst", "prefix": "copy/"}
	j := e.mustSubmit(spec)
	e.waitState(j.ID, jobs.StateSucceeded)
	if got := read(t, r, "dst", "copy/a"); got != "from src" {
		t.Errorf("copy/a = %q", got)
	}

	spec["target"] = map[string]any{"bucket": "dst"}
	spec["options"] = map[string]any{"onExisting": "merge"}
	j = e.mustSubmit(spec)
	e.waitState(j.ID, jobs.StateSucceeded)
	if got := keys(t, r, "dst"); !slices.Equal(got, []string{"a", "copy/a", "mine"}) {
		t.Errorf("target keys %v", got)
	}
}

// A mirror that would close a cycle is refused, chains are not; nothing
// may be copied into the target of an active mirror.
func TestMirrorCycleRefused(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	for _, b := range []string{"m1", "m2", "m3", "m4"} {
		createBucket(t, r, b, storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	}
	e := newEnv(t, r, "")
	mirror := func(src, dst string) map[string]any {
		return map[string]any{"kind": "mirror", "source": map[string]any{"bucket": src}, "target": map[string]any{"bucket": dst},
			"options": map[string]any{"onExisting": "merge"}}
	}
	j1 := e.mustSubmit(mirror("m1", "m2"))
	if _, err := e.submit(mirror("m2", "m1")); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("mirror m2->m1 after m1->m2: %v, want ErrConflict", err)
	}
	e.mustSubmit(mirror("m2", "m3")) // a chain
	if _, err := e.submit(mirror("m3", "m1")); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("mirror m3->m1 closing m1->m2->m3: %v, want ErrConflict", err)
	}
	copyInto := map[string]any{"kind": "copy", "source": map[string]any{"bucket": "m4"}, "target": map[string]any{"bucket": "m2"},
		"options": map[string]any{"onExisting": "merge"}}
	if _, err := e.submit(copyInto); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("copy into the target of a mirror: %v, want ErrConflict", err)
	}
	if _, err := e.submit(mirror("m4", "m3")); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("a second mirror into m3: %v, want ErrConflict", err)
	}
	// Once the mirror is canceled, its target is free.
	if err := e.m.Cancel(j1.ID); err != nil {
		t.Fatal(err)
	}
	e.waitState(j1.ID, jobs.StateCanceled)
	e.mustSubmit(copyInto)
}

func TestCheckJobsPrefixes(t *testing.T) {
	mirror := func(id, sb, sp, db, dp string) otherJob {
		return otherJob{id: id, kind: KindMirror, src: keyRange{sb, sp}, dst: keyRange{db, dp}}
	}
	spec := func(kind, sb, sp, db, dp string) *Spec {
		return &Spec{Kind: kind, Source: SourceSpec{Bucket: sb, Prefix: sp}, Target: TargetSpec{Bucket: db, Prefix: dp}}
	}
	others := []otherJob{mirror("j1", "a", "x/", "b", "y/")}
	for _, c := range []struct {
		sp   *Spec
		fail bool
	}{
		{spec(KindMirror, "b", "y/", "a", "z/"), false},    // back into another prefix of a
		{spec(KindMirror, "b", "y/", "a", "x/sub/"), true}, // into the mirrored range
		{spec(KindMirror, "b", "", "a", ""), true},         // the whole buckets
		{spec(KindMirror, "b", "other/", "a", "x/"), false},
		{spec(KindCopy, "c", "", "b", "y/q/"), true}, // into the mirror's target
		{spec(KindCopy, "c", "", "b", "z/"), false},
	} {
		err := checkJobs(c.sp, others)
		if (err != nil) != c.fail || err != nil && !errors.Is(err, jobs.ErrConflict) {
			t.Errorf("%s %s: %v (want failure %v)", c.sp.Kind, describe(c.sp), err, c.fail)
		}
	}
}

// A mirror copies the changes made to its source (writes, new versions,
// delete markers and deleted versions) and fails once its source is
// deleted.
func TestMirrorPropagatesChanges(t *testing.T) {
	r := openRouter(t, memBacking("a"), memBacking("b"))
	createBucket(t, r, "live", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	put(t, r, "live", "old", "before the mirror")
	e := newEnv(t, r, t.TempDir())
	j := e.mustSubmit(map[string]any{
		"kind":   "mirror",
		"source": map[string]any{"bucket": "live"},
		"target": map[string]any{"bucket": "replica", "create": map[string]any{"backing": "b"}},
	})
	e.waitIdle(j.ID)
	same := func(key string) func() bool {
		return func() bool {
			sv, dv := versions(t, r, "live", key), versions(t, r, "replica", key)
			if len(sv) != len(dv) {
				return false
			}
			for i := range sv {
				if sv[i].VersionID != dv[i].VersionID || sv[i].ETag != dv[i].ETag || sv[i].DeleteMarker != dv[i].DeleteMarker {
					return false
				}
			}
			return true
		}
	}
	waitFor(t, "the initial copy", same("old"))

	put(t, r, "live", "new", "v1")
	waitFor(t, "a new key to be mirrored", same("new"))
	v2 := put(t, r, "live", "new", "v2")
	waitFor(t, "a new version to be mirrored", same("new"))
	if got := read(t, r, "replica", "new"); got != "v2" {
		t.Errorf("new = %q", got)
	}
	del(t, r, "live", "new", "")
	waitFor(t, "a delete marker to be mirrored", same("new"))
	del(t, r, "live", "new", v2.VersionID)
	waitFor(t, "a deleted version to be mirrored", same("new"))
	if n := len(versions(t, r, "replica", "new")); n != 2 {
		t.Errorf("replica has %d versions of new, want 2", n)
	}

	// Configuration follows.
	cors := []byte("<CORSConfiguration><CORSRule><AllowedMethod>PUT</AllowedMethod><AllowedOrigin>*</AllowedOrigin></CORSRule></CORSConfiguration>")
	if err := r.PutBucketConfig(context.Background(), "live", storage.ConfigCORS, cors); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the configuration to be mirrored", func() bool {
		got, err := r.GetBucketConfig(context.Background(), "replica", storage.ConfigCORS)
		return err == nil && string(got) == string(cors)
	})
	if err := r.DeleteBucketConfig(context.Background(), "live", storage.ConfigCORS); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the configuration's deletion to be mirrored", func() bool {
		_, err := r.GetBucketConfig(context.Background(), "replica", storage.ConfigCORS)
		return errors.Is(err, storage.ErrConfigNotFound)
	})

	st := statusOf(t, e.waitIdle(j.ID))
	if st.Passes < 1 || st.LastPassAt.IsZero() || st.NextPassAt.IsZero() {
		t.Errorf("status %+v", st)
	}

	// Empty and delete the source: the mirror fails.
	for _, key := range []string{"old", "new"} {
		for _, v := range versions(t, r, "live", key) {
			del(t, r, "live", key, v.VersionID)
		}
	}
	waitFor(t, "the target to be emptied", func() bool {
		return len(versions(t, r, "replica", "old")) == 0 && len(versions(t, r, "replica", "new")) == 0
	})
	e.waitIdle(j.ID)
	if err := r.DeleteBucket(context.Background(), "live"); err != nil {
		t.Fatal(err)
	}
	j = e.waitState(j.ID, jobs.StateFailed)
	if !strings.Contains(j.LastError, `"live" was deleted`) {
		t.Errorf("lastError %q", j.LastError)
	}
}

// Without versions, a mirror deletes the target keys it wrote once the
// source deletes them, and keeps the keys others wrote.
func TestMirrorDeletesOwnedKeysOnly(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	createBucket(t, r, "dst", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	put(t, r, "src", "a", "a")
	put(t, r, "src", "b", "b")
	e := newEnv(t, r, "")
	j := e.mustSubmit(map[string]any{"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst"}})
	e.waitIdle(j.ID)
	waitFor(t, "the initial copy", func() bool { return slices.Equal(keys(t, r, "dst"), []string{"a", "b"}) })
	put(t, r, "dst", "theirs", "not the mirror's")
	del(t, r, "src", "a", "")
	waitFor(t, "the delete to be mirrored", func() bool { return slices.Equal(keys(t, r, "dst"), []string{"b", "theirs"}) })

	// A full pass (as after a restart) keeps the key the mirror did not
	// write.
	before := statusOf(t, e.waitIdle(j.ID)).Passes
	e.s.mu.Lock()
	w := e.s.watches[j.ID]
	e.s.mu.Unlock()
	w.mu.Lock()
	w.verified = false
	w.mu.Unlock()
	e.m.Kick(j.ID)
	e.waitJob(j.ID, "a full pass", func(j jobs.Job) bool {
		return j.State == jobs.StateWaiting && statusOf(t, j).Passes > before
	})
	if got := keys(t, r, "dst"); !slices.Equal(got, []string{"b", "theirs"}) {
		t.Errorf("target keys after a full pass: %v", got)
	}
}

// A copy interrupted by a shutdown resumes from its checkpoint.
func TestCopyResumesFromCheckpoint(t *testing.T) {
	slow := &slowBackend{Backend: memory.New(), delay: 15 * time.Millisecond}
	r := openRouter(t, multi.Backing{Name: "slow", Kind: "memory", Backend: slow}, memBacking("b"))
	createBucket(t, r, "big", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	const n = 60
	for i := range n {
		put(t, r, "big", fmt.Sprintf("k%03d", i), fmt.Sprintf("body %d", i))
	}
	dir := t.TempDir()
	e := newEnv(t, r, dir)
	j := e.mustSubmit(map[string]any{
		"kind":    "copy",
		"source":  map[string]any{"bucket": "big"},
		"target":  map[string]any{"bucket": "big-copy", "create": map[string]any{"backing": "b"}},
		"options": map[string]any{"concurrency": 1, "finalPass": false},
	})
	waitFor(t, "part of the copy", func() bool { return slow.opens.Load() >= 20 })
	e.close()
	j, err := e.m.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cp checkpoint
	if err := json.Unmarshal(j.Checkpoint, &cp); err != nil || cp.Phase != phaseCopying || cp.After == "" {
		t.Fatalf("checkpoint after the shutdown: %s (%v)", j.Checkpoint, err)
	}
	if j.State != jobs.StateQueued {
		t.Fatalf("state after the shutdown %s, want queued", j.State)
	}
	copied := len(keys(t, r, "big-copy"))
	if copied == 0 || copied == n {
		t.Fatalf("%d keys copied before the shutdown", copied)
	}

	e2 := newEnv(t, r, dir)
	j = e2.waitState(j.ID, jobs.StateSucceeded)
	if got := len(keys(t, r, "big-copy")); got != n {
		t.Fatalf("%d keys copied, want %d", got, n)
	}
	// Every key was read once, plus at most the one interrupted.
	if opens := slow.opens.Load(); opens > n+1 {
		t.Errorf("%d reads of the source for %d keys: the copy started over", opens, n)
	}
	if st := statusOf(t, j); st.KeysCopied != n || st.Passes != 1 {
		t.Errorf("status %+v", st)
	}
}

// Into a target that cannot import versions, keys are copied in
// fingerprint mode, with the records in the job's database.
func TestFingerprintMode(t *testing.T) {
	plain := multi.Backing{Name: "plain", Kind: "memory", Backend: plainBackend{memory.New()}}
	r := openRouter(t, memBacking("a"), plain)
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	put(t, r, "src", "x", "x1")
	put(t, r, "src", "y", "y1")
	dir := t.TempDir()
	e := newEnv(t, r, dir)
	spec := map[string]any{
		"kind":   "mirror",
		"source": map[string]any{"bucket": "src"},
		"target": map[string]any{"bucket": "native", "create": map[string]any{"backing": "plain"}},
	}
	norm, warnings, err := e.s.DryRun(context.Background(), mustJSON(t, spec))
	if err != nil {
		t.Fatal(err)
	}
	var sp Spec
	if err := json.Unmarshal(norm, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Options.Versions != VersionsCurrent {
		t.Errorf("versions default %q for a target that cannot import, want current", sp.Options.Versions)
	}
	if !containsText(warnings, "cannot import versions") {
		t.Errorf("warnings %q", warnings)
	}

	j := e.mustSubmit(spec)
	j = e.waitIdle(j.ID)
	if st := statusOf(t, j); st.Mode != modeFingerprint {
		t.Errorf("mode %q, want fingerprint", st.Mode)
	}
	waitFor(t, "the initial copy", func() bool { return slices.Equal(keys(t, r, "native"), []string{"x", "y"}) })
	if _, err := os.Stat(filepath.Join(dir, j.ID, stateFile)); err != nil {
		t.Errorf("job database: %v", err)
	}
	put(t, r, "src", "x", "x2")
	waitFor(t, "an overwrite to be mirrored", func() bool { return readOrEmpty(t, r, "native", "x") == "x2" })
	del(t, r, "src", "y", "")
	waitFor(t, "a delete to be mirrored", func() bool { return slices.Equal(keys(t, r, "native"), []string{"x"}) })
	if n := len(versions(t, r, "native", "x")); n != 1 {
		t.Errorf("%d versions of x on the target, want 1", n)
	}
}

func TestDryRun(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	ctx := context.Background()
	createBucket(t, r, "src", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	createBucket(t, r, "dst", storage.CreateBucketOptions{}, storage.VersioningUnversioned)
	put(t, r, "dst", "existing", "x")
	if err := r.PutBucketConfig(ctx, "dst", storage.ConfigLifecycle, []byte("<LifecycleConfiguration/>")); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, r, "")

	norm, warnings, err := e.s.DryRun(ctx, mustJSON(t, map[string]any{
		"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "dst"},
		"options": map[string]any{"onExisting": "merge"}, "dryRun": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !containsText(warnings, "lifecycle configuration") || !containsText(warnings, "not empty") {
		t.Errorf("warnings %q", warnings)
	}
	var sp Spec
	if err := json.Unmarshal(norm, &sp); err != nil {
		t.Fatal(err)
	}
	o := sp.Options
	if o.Versions != VersionsAll || o.Configs != ConfigsSafe || o.Encryption != EncryptionPreserve || o.Compression != CompressionPreserve ||
		o.Concurrency != DefaultConcurrency || o.FinalPass == nil || !*o.FinalPass || o.PollInterval != "" || o.PropagateDeletes != nil || sp.DryRun {
		t.Errorf("normalized copy spec %s", norm)
	}
	if len(e.m.List(jobs.Filter{})) != 0 {
		t.Error("a dry run created a job")
	}

	norm, _, err = e.s.DryRun(ctx, mustJSON(t, map[string]any{
		"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "new-one", "create": map[string]any{}},
		"options": map[string]any{"verifyInterval": "90m"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	sp = Spec{}
	if err := json.Unmarshal(norm, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Options.PollInterval != "5m" || sp.Options.VerifyInterval != "1h30m" || sp.Options.FinalPass != nil ||
		sp.Options.PropagateDeletes == nil || !*sp.Options.PropagateDeletes || sp.Target.Create.ObjectLock == nil {
		t.Errorf("normalized mirror spec %s", norm)
	}

	for name, spec := range map[string]map[string]any{
		"unknown field":     {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{}}, "bogus": 1},
		"unknown option":    {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{}}, "options": map[string]any{"fast": true}},
		"no kind":           {"source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{}}},
		"missing source":    {"kind": "copy", "source": map[string]any{"bucket": "nope"}, "target": map[string]any{"bucket": "x", "create": map[string]any{}}},
		"missing target":    {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x"}},
		"onto itself":       {"kind": "copy", "source": map[string]any{"bucket": "src", "prefix": "a/"}, "target": map[string]any{"bucket": "src", "prefix": "a/b/"}},
		"unknown backing":   {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{"backing": "zz"}}},
		"invalid name":      {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "X_Y", "create": map[string]any{}}},
		"bad concurrency":   {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{}}, "options": map[string]any{"concurrency": 33}},
		"short poll":        {"kind": "mirror", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{}}, "options": map[string]any{"pollInterval": "10s"}},
		"bad versions":      {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{}}, "options": map[string]any{"versions": "some"}},
		"kind of a backing": {"kind": "copy", "source": map[string]any{"bucket": "src"}, "target": map[string]any{"bucket": "x", "create": map[string]any{"kind": "native"}}},
	} {
		if _, _, err := e.s.DryRun(ctx, mustJSON(t, spec)); !errors.Is(err, jobs.ErrInvalidSpec) {
			t.Errorf("%s: %v, want ErrInvalidSpec", name, err)
		}
	}
	// Submitting a dry run is refused.
	if _, err := e.submit(map[string]any{"kind": "copy", "source": map[string]any{"bucket": "src"},
		"target": map[string]any{"bucket": "x", "create": map[string]any{}}, "dryRun": true}); !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Errorf("submitting a dry run: %v", err)
	}
}

// Locked data needs a target with object lock, or dropRetention.
func TestObjectLockRules(t *testing.T) {
	r := openRouter(t, memBacking("a"))
	createBucket(t, r, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true}, storage.VersioningUnversioned)
	createBucket(t, r, "plain", storage.CreateBucketOptions{}, storage.VersioningEnabled)
	e := newEnv(t, r, "")
	ctx := context.Background()
	_, _, err := e.s.DryRun(ctx, mustJSON(t, map[string]any{"kind": "copy", "source": map[string]any{"bucket": "locked"}, "target": map[string]any{"bucket": "plain"}}))
	if !errors.Is(err, jobs.ErrInvalidSpec) || !strings.Contains(err.Error(), "dropRetention") {
		t.Errorf("locked into a target without object lock: %v", err)
	}
	if _, _, err := e.s.DryRun(ctx, mustJSON(t, map[string]any{"kind": "copy", "source": map[string]any{"bucket": "locked"},
		"target": map[string]any{"bucket": "plain"}, "options": map[string]any{"dropRetention": true}})); err != nil {
		t.Errorf("with dropRetention: %v", err)
	}
	norm, _, err := e.s.DryRun(ctx, mustJSON(t, map[string]any{"kind": "copy", "source": map[string]any{"bucket": "locked"},
		"target": map[string]any{"bucket": "locked-copy", "create": map[string]any{}}}))
	if err != nil {
		t.Fatalf("into a new bucket: %v", err)
	}
	var sp Spec
	if err := json.Unmarshal(norm, &sp); err != nil {
		t.Fatal(err)
	}
	if sp.Target.Create.ObjectLock == nil || !*sp.Target.Create.ObjectLock {
		t.Errorf("a new target of a locked source gets no object lock: %s", norm)
	}
}

func TestRewritePolicy(t *testing.T) {
	sp := &Spec{Source: SourceSpec{Bucket: "photos", Prefix: "pub/"}, Target: TargetSpec{Bucket: "copy", Prefix: "old/pub/"}}
	out, reason := rewritePolicy([]byte(`{"Statement":{"Effect":"Deny","Principal":"*","Action":"s3:GetObject",
		"Resource":["arn:aws:s3:::photos/pub/*","arn:aws:s3:::photos"]}}`), sp)
	if reason != "" {
		t.Fatal(reason)
	}
	if !strings.Contains(string(out), `"arn:aws:s3:::copy/old/pub/*"`) || !strings.Contains(string(out), `"arn:aws:s3:::copy"`) {
		t.Errorf("rewritten policy %s", out)
	}
	if _, reason := rewritePolicy([]byte(`{"Statement":{"Effect":"Deny","Principal":"*","Action":"s3:GetObject",
		"NotResource":"arn:aws:s3:::photos/pub/*"}}`), sp); reason == "" {
		t.Error("NotResource was mapped to a prefix")
	}
	// Without prefixes, object ARNs keep their key.
	sp.Source.Prefix = ""
	sp.Target.Prefix = ""
	out, reason = rewritePolicy([]byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}`), sp)
	if reason != "" || !strings.Contains(string(out), `"arn:aws:s3:::copy/*"`) {
		t.Errorf("rewritten policy %s (%s)", out, reason)
	}
	// A policy naming other buckets is not valid for the target.
	if _, reason := rewritePolicy([]byte(`{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::elsewhere/*"}]}`), sp); reason == "" {
		t.Error("a policy for another bucket was accepted")
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func containsText(list []string, sub string) bool {
	return slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, sub) })
}
