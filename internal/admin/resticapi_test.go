package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// testSnapshotID is a whole-bucket snapshot of the fake service;
// testArchiveSnapshotID a per-version archive snapshot.
var (
	testSnapshotID        = strings.Repeat("ab", 32)
	testArchiveSnapshotID = strings.Repeat("cd", 32)
	testPartialSnapshotID = strings.Repeat("ef", 32)
)

// fakeSnapshots is the SnapshotService of the admin tests.
type fakeSnapshots struct {
	repo restic.RepositoryInfo

	mu       sync.Mutex
	configs  map[string]json.RawMessage
	listErr  error
	repoErr  error
	running  bool // RunNow answers jobs.ErrConflict
	forgot   []string
	restores []json.RawMessage
	seq      int
}

func newFakeSnapshots() *fakeSnapshots {
	return &fakeSnapshots{
		configs: map[string]json.RawMessage{},
		repo: restic.RepositoryInfo{Repository: "/data/restic-aux/snap-1", PasswordSource: "field password of keys/aux/snap-1.restic.json",
			Initialized: true},
	}
}

func (f *fakeSnapshots) setConfig(bucket, doc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[bucket] = json.RawMessage(doc)
}

func (f *fakeSnapshots) Config(_ context.Context, bucket string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.configs[bucket]; ok {
		return c, nil
	}
	return nil, storage.ErrConfigNotFound
}

func (f *fakeSnapshots) PutConfig(_ context.Context, bucket string, doc json.RawMessage) (json.RawMessage, error) {
	var cfg struct {
		Interval string `json:"interval"`
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %v", jobs.ErrInvalidSpec, err)
	}
	if _, err := time.ParseDuration(cfg.Interval); err != nil {
		return nil, fmt.Errorf("%w: interval: %v", jobs.ErrInvalidSpec, err)
	}
	norm := json.RawMessage(fmt.Sprintf(`{"enabled":true,"interval":%q,"target":{"kind":"bucket"}}`, cfg.Interval))
	f.setConfig(bucket, string(norm))
	return norm, nil
}

func (f *fakeSnapshots) DeleteConfig(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.configs[bucket]; !ok {
		return storage.ErrConfigNotFound
	}
	delete(f.configs, bucket)
	return nil
}

func (f *fakeSnapshots) Status(context.Context, string) (json.RawMessage, error) {
	return json.RawMessage(`{"lastSnapshot":"` + testSnapshotID + `"}`), nil
}

func (f *fakeSnapshots) Repository(ctx context.Context, bucket string) (*restic.RepositoryInfo, error) {
	if _, err := f.Config(ctx, bucket); err != nil {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repoErr != nil {
		return nil, f.repoErr
	}
	r := f.repo
	return &r, nil
}

func (f *fakeSnapshots) Snapshots(context.Context, string) ([]restic.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []restic.Snapshot{
		{ID: testSnapshotID, ShortID: testSnapshotID[:8], Tags: []string{"gos3-snapshot"}},
		{ID: testPartialSnapshotID, ShortID: testPartialSnapshotID[:8], Tags: []string{"gos3-snapshot", "gos3-partial"}},
	}, nil
}

func (f *fakeSnapshots) job(kind, bucket, createdBy string, labels map[string]string) jobs.Job {
	f.seq++
	if labels == nil {
		labels = map[string]string{"source": bucket}
	}
	return jobs.Job{ID: fmt.Sprintf("job%d", f.seq), Kind: kind, CreatedBy: createdBy, State: jobs.StateQueued,
		Labels: labels, Spec: json.RawMessage(`{}`), Checkpoint: json.RawMessage(`{"stage":"internal"}`)}
}

func (f *fakeSnapshots) RunNow(_ context.Context, bucket, createdBy string) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return jobs.Job{}, fmt.Errorf("%w: a snapshot of %q is running", jobs.ErrConflict, bucket)
	}
	if _, ok := f.configs[bucket]; !ok {
		return jobs.Job{}, storage.ErrConfigNotFound
	}
	f.running = true
	return f.job("restic-snapshot", bucket, createdBy, nil), nil
}

func (f *fakeSnapshots) Forget(_ context.Context, _, id string) error {
	switch id {
	case testSnapshotID:
		f.mu.Lock()
		f.forgot = append(f.forgot, id)
		f.mu.Unlock()
		return nil
	case testArchiveSnapshotID:
		return fmt.Errorf("%w: %s is a per-version archive snapshot", restic.ErrForgetRefused, id)
	}
	return fmt.Errorf("%w: %s", restic.ErrSnapshotNotFound, id)
}

func (f *fakeSnapshots) Restore(_ context.Context, bucket, id string, req json.RawMessage, createdBy string) (jobs.Job, error) {
	var r struct {
		Target struct {
			Bucket string `json:"bucket"`
		} `json:"target"`
		OnExisting string `json:"onExisting"`
	}
	dec := json.NewDecoder(bytes.NewReader(req))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return jobs.Job{}, fmt.Errorf("%w: %v", jobs.ErrInvalidSpec, err)
	}
	if id != testSnapshotID {
		return jobs.Job{}, fmt.Errorf("%w: %s", restic.ErrSnapshotNotFound, id)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, req)
	return f.job("restic-restore", bucket, createdBy, map[string]string{"source": bucket, "target": r.Target.Bucket}), nil
}

func (f *fakeSnapshots) Prune(_ context.Context, bucket, createdBy string) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.job("restic-prune", bucket, createdBy, nil), nil
}

func (f *fakeSnapshots) Credentials(ctx context.Context, bucket string) (restic.Location, *restic.RepositoryInfo, error) {
	repo, err := f.Repository(ctx, bucket)
	if err != nil {
		return restic.Location{}, nil, err
	}
	if repo == nil {
		return restic.Location{}, nil, storage.ErrConfigNotFound
	}
	return restic.Location{Repository: repo.Repository, Password: testResticPassword, Env: []string{"RESTIC_CACHE_DIR=/tmp/c"}}, repo, nil
}

// isNull reports whether a decoded JSON value is absent or null.
func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

// snapshotsDoc returns the GET document of path (…/restic/snapshots).
func (e *testEnv) snapshotsDoc(path string) snapshotsDocument {
	e.t.Helper()
	var d snapshotsDocument
	e.root("GET", path, nil).json(e.t, &d)
	return d
}

// resticInfoDoc returns the ResticInfo of bucket.
func (e *testEnv) resticInfoDoc(bucket string) resticInfo {
	e.t.Helper()
	var info resticInfo
	e.root("GET", "/buckets/"+bucket+"/restic", nil).json(e.t, &info)
	return info
}

func newSnapshotsEnv(t *testing.T) (*testEnv, *fakeSnapshots) {
	t.Helper()
	snaps := newFakeSnapshots()
	e := newEnv(t, func(d *Deps) { d.Snapshots = snaps })
	e.createBucket("photos", storage.CreateBucketOptions{})
	e.createBucket("restored", storage.CreateBucketOptions{})
	return e, snaps
}

func TestSnapshotEndpoints(t *testing.T) {
	e, snaps := newSnapshotsEnv(t)
	base := "/buckets/photos/restic/snapshots"

	// No schedule yet.
	var doc snapshotsDocument
	r := e.root("GET", base, nil)
	r.json(t, &doc)
	if !isNull(doc.Config) || doc.Repository != nil || len(doc.Snapshots) != 0 || !bytes.Contains(r.body, []byte(`"snapshots":[]`)) ||
		!bytes.Contains(r.body, []byte(`"config":null`)) || !strings.Contains(string(doc.Status), "lastSnapshot") {
		t.Fatalf("GET without a schedule: %s", r.body)
	}
	if msg := e.root("POST", base, nil).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "schedule") {
		t.Fatalf("run without a schedule: %q", msg)
	}

	// PUT validates through the service.
	e.root("PUT", base+"/config", map[string]any{"interval": "soon"}).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", base+"/config", map[string]any{"interval": "24h", "bogus": true}).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", base+"/config", `[]`).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", base+"/config", "").expectError(t, 400, codeInvalidRequest)
	var cfg map[string]any
	e.root("PUT", base+"/config", map[string]any{"interval": "24h"}).json(t, &cfg)
	if cfg["interval"] != "24h" || cfg["enabled"] != true {
		t.Fatalf("PUT = %v", cfg)
	}
	doc = e.snapshotsDoc(base)
	r = e.root("GET", base, nil)
	doc = snapshotsDocument{}
	r.json(t, &doc)
	if isNull(doc.Config) || doc.Repository == nil || len(doc.Snapshots) != 2 || doc.Snapshots[0].ID != testSnapshotID || doc.Error != "" {
		t.Fatalf("GET = %+v", doc)
	}
	// Partial snapshots (tag gos3-partial) are marked; "partial" is always sent.
	if doc.Snapshots[0].Partial || !doc.Snapshots[1].Partial || doc.Snapshots[1].ID != testPartialSnapshotID ||
		!bytes.Contains(r.body, []byte(`"partial":false`)) || !bytes.Contains(r.body, []byte(`"shortId":"abababab"`)) {
		t.Fatalf("GET = %s", r.body)
	}
	e.root("GET", base+"/config", nil).expectError(t, 405, codeMethodNotAllowed)

	// A repository that cannot be listed leaves the rest of the document.
	snaps.mu.Lock()
	snaps.listErr = fmt.Errorf("restic: repository locked: %w", storage.ErrEncryptionUnavailable)
	snaps.mu.Unlock()
	doc = e.snapshotsDoc(base)
	if doc.Error == "" || isNull(doc.Config) || len(doc.Snapshots) != 0 {
		t.Fatalf("GET with a failing list = %+v", doc)
	}

	// Run now, once at a time.
	var j jobs.Job
	r = e.root("POST", base, nil)
	r.expectStatus(t, 202)
	r.json(t, &j)
	if j.Kind != "restic-snapshot" || j.CreatedBy != "root" || bytes.Contains(r.body, []byte("checkpoint")) {
		t.Fatalf("run = %s", r.body)
	}
	e.root("POST", base, nil).expectError(t, 409, codeConflict)

	// Forget.
	e.root("DELETE", base+"/"+testSnapshotID, nil).expectStatus(t, 204)
	if msg := e.root("DELETE", base+"/"+testArchiveSnapshotID, nil).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "per-version") {
		t.Fatalf("forget archive snapshot: %q", msg)
	}
	e.root("DELETE", base+"/"+strings.Repeat("0", 64), nil).expectError(t, 404, codeNotFound)
	e.root("DELETE", base+"/latest", nil).expectError(t, 400, codeInvalidRequest)
	e.root("DELETE", base+"/"+strings.ToUpper(testSnapshotID), nil).expectError(t, 400, codeInvalidRequest)
	snaps.mu.Lock()
	forgot := snaps.forgot
	snaps.mu.Unlock()
	if len(forgot) != 1 {
		t.Fatalf("forgot %v", forgot)
	}

	// Prune.
	r = e.root("POST", "/buckets/photos/restic/prune", nil)
	r.expectStatus(t, 202)
	r.json(t, &j)
	if j.Kind != "restic-prune" {
		t.Fatalf("prune = %+v", j)
	}

	// Delete the schedule, idempotently.
	e.root("DELETE", base+"/config", nil).expectStatus(t, 204)
	e.root("DELETE", base+"/config", nil).expectStatus(t, 204)
	doc = e.snapshotsDoc(base)
	if !isNull(doc.Config) {
		t.Fatalf("GET after DELETE = %+v", doc)
	}

	// Unknown buckets.
	for _, req := range [][2]string{{"GET", "/buckets/nope/restic/snapshots"}, {"POST", "/buckets/nope/restic/snapshots"},
		{"DELETE", "/buckets/nope/restic/snapshots/config"}, {"POST", "/buckets/nope/restic/prune"}} {
		e.root(req[0], req[1], nil).expectError(t, 404, codeNotFound)
	}
	e.root("PUT", "/buckets/nope/restic/snapshots/config", map[string]any{"interval": "24h"}).expectError(t, 404, codeNotFound)
}

// A restore needs RestoreBucketSnapshot on the bucket and CreateCopyJob on
// the target bucket.
func TestSnapshotRestore(t *testing.T) {
	e, snaps := newSnapshotsEnv(t)
	path := "/buckets/photos/restic/snapshots/" + testSnapshotID + "/restore"
	body := map[string]any{"target": map[string]any{"bucket": "restored"}, "onExisting": "merge"}
	e.putPolicy("restore-only", map[string]any{"Effect": "Allow", "Action": "admin:RestoreBucketSnapshot", "Resource": "arn:gos3:admin:::bucket/photos"})
	e.putPolicy("restore-to", map[string]any{"Effect": "Allow", "Action": "admin:CreateCopyJob", "Resource": "arn:gos3:admin:::bucket/restored"})
	e.putPolicy("copy-only", map[string]any{"Effect": "Allow", "Action": "admin:CreateCopyJob", "Resource": "arn:gos3:admin:::bucket/*"})
	restoreOnly := e.addUser("alice", "restore-only")
	both := e.addUser("bob", "restore-only", "restore-to")
	copyOnly := e.addUser("carol", "copy-only")

	if msg := e.do(&restoreOnly, "POST", Prefix+path, body).expectError(t, 403, codeAccessDenied); !strings.Contains(msg, "admin:CreateCopyJob") ||
		!strings.Contains(msg, "bucket/restored") {
		t.Fatalf("message %q", msg)
	}
	e.do(&copyOnly, "POST", Prefix+path, body).expectError(t, 403, codeAccessDenied)
	// The target is read like the service reads it: the last of
	// differently cased duplicates.
	e.do(&both, "POST", Prefix+path, `{"target":{"bucket":"restored"},"TARGET":{"bucket":"photos"}}`).expectError(t, 403, codeAccessDenied)

	var j jobs.Job
	r := e.do(&both, "POST", Prefix+path, body)
	r.expectStatus(t, 202)
	r.json(t, &j)
	if j.Kind != "restic-restore" || j.CreatedBy != "bob" || j.Labels["target"] != "restored" || j.Checkpoint != nil {
		t.Fatalf("restore = %s", r.body)
	}
	snaps.mu.Lock()
	got := string(snaps.restores[len(snaps.restores)-1])
	snaps.mu.Unlock()
	if !strings.Contains(got, `"onExisting":"merge"`) {
		t.Fatalf("the service got %s", got)
	}

	e.root("POST", path, map[string]any{"onExisting": "merge"}).expectError(t, 400, codeInvalidRequest)
	e.root("POST", path, map[string]any{"target": map[string]any{"bucket": "restored"}, "bogus": 1}).expectError(t, 400, codeInvalidRequest)
	e.root("POST", "/buckets/photos/restic/snapshots/"+strings.Repeat("0", 64)+"/restore", body).expectError(t, 404, codeNotFound)
	e.root("POST", "/buckets/photos/restic/snapshots/nothex!/restore", body).expectError(t, 400, codeInvalidRequest)
}

// The snapshot actions are scoped to buckets.
func TestSnapshotAuthorization(t *testing.T) {
	e, _ := newSnapshotsEnv(t)
	e.putPolicy("view", map[string]any{"Effect": "Allow", "Action": "admin:GetBucketSnapshots", "Resource": "arn:gos3:admin:::bucket/photos"})
	viewer := e.addUser("viewer", "view")
	diag := e.addUser("diag", "diagnostics")
	base := Prefix + "/buckets/photos/restic/snapshots"
	e.do(&viewer, "GET", base, nil).expectStatus(t, 200)
	e.do(&diag, "GET", base, nil).expectStatus(t, 200)
	for _, c := range []auth.Credentials{viewer, diag} {
		e.do(&c, "PUT", base+"/config", map[string]any{"interval": "24h"}).expectError(t, 403, codeAccessDenied)
		e.do(&c, "POST", base, nil).expectError(t, 403, codeAccessDenied)
		e.do(&c, "POST", Prefix+"/buckets/photos/restic/prune", nil).expectError(t, 403, codeAccessDenied)
		e.do(&c, "DELETE", base+"/"+testSnapshotID, nil).expectError(t, 403, codeAccessDenied)
		e.do(&c, "POST", Prefix+"/buckets/photos/restic/credentials", map[string]any{}).expectError(t, 403, codeAccessDenied)
	}
	e.do(&viewer, "GET", Prefix+"/buckets/restored/restic/snapshots", nil).expectError(t, 403, codeAccessDenied)
}

// Without a snapshot service the endpoints say so.
func TestSnapshotsUnavailable(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	if msg := e.root("GET", "/buckets/photos/restic/snapshots", nil).expectError(t, 404, codeNotFound); !strings.Contains(msg, "not available") {
		t.Fatalf("message %q", msg)
	}
	e.root("POST", "/buckets/photos/restic/snapshots", nil).expectError(t, 404, codeNotFound)
	var info resticInfo
	info = e.resticInfoDoc("photos")
	if !isNull(info.Snapshots) || len(info.Archive) != 1 || info.Archive[0].Backing != "default" || !strings.HasPrefix(info.Archive[0].Error, "no repository") {
		t.Fatalf("info = %+v", info)
	}
}

// The job and restic capabilities follow service-level grants.
func TestWhoamiJobCapabilities(t *testing.T) {
	e := newEnv(t)
	e.putPolicy("ops", map[string]any{"Effect": "Allow",
		"Action": []string{"admin:ListJobs", "admin:CreateCopyJob", "admin:PutBucketSnapshots", "admin:GetResticCredentials",
			"admin:RunBucketSnapshot", "admin:RestoreBucketSnapshot", "admin:UpdateJob"}, "Resource": "*"})
	e.putPolicy("scoped", map[string]any{"Effect": "Allow",
		"Action": []string{"admin:ListJobs", "admin:CreateCopyJob", "admin:PutBucketSnapshots", "admin:GetResticCredentials",
			"admin:RunBucketSnapshot", "admin:RestoreBucketSnapshot", "admin:UpdateJob"},
		"Resource": "arn:gos3:admin:::bucket/*"})
	ops := e.addUser("ops", "ops")
	scoped := e.addUser("scoped", "scoped")
	diag := e.addUser("diag", "diagnostics")

	caps := func(c auth.Credentials) capabilities {
		t.Helper()
		var w whoAmI
		e.do(&c, "GET", Prefix+"/whoami", nil).json(t, &w)
		return w.Capabilities
	}
	if got := caps(ops); got != (capabilities{Admin: true, ViewJobs: true, ManageJobs: true, ManageSnapshots: true, RevealResticCredentials: true,
		RunSnapshots: true, RestoreSnapshots: true, UpdateJobs: true}) {
		t.Errorf("ops = %+v", got)
	}
	if got := caps(scoped); got != (capabilities{}) {
		t.Errorf("scoped = %+v", got)
	}
	if got := caps(diag); !got.ViewJobs || got.ManageJobs || got.ManageSnapshots || got.RevealResticCredentials ||
		got.RunSnapshots || got.RestoreSnapshots || got.UpdateJobs {
		t.Errorf("diagnostics = %+v", got)
	}
	r := e.root("GET", "/whoami", nil)
	for _, k := range []string{`"viewJobs":true`, `"manageJobs":true`, `"manageSnapshots":true`, `"revealResticCredentials":true`,
		`"runSnapshots":true`, `"restoreSnapshots":true`, `"updateJobs":true`} {
		if !strings.Contains(string(r.body), k) {
			t.Errorf("root whoami lacks %s: %s", k, r.body)
		}
	}
}
