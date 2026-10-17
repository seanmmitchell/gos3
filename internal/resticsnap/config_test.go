package resticsnap

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// PutConfig validates strictly and fills in the defaults.
func TestPutConfigValidation(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("mem1", "", "")
	disk := `"target":{"kind":"backing","backing":"disk"}`
	for name, doc := range map[string]string{
		"not an object":    `[]`,
		"unknown field":    `{"enabled":true,"x":1,` + disk + `}`,
		"trailing data":    `{` + disk + `} {}`,
		"short interval":   `{"interval":"30m",` + disk + `}`,
		"long interval":    `{"interval":"1000h",` + disk + `}`,
		"bad interval":     `{"interval":"daily",` + disk + `}`,
		"at, short":        `{"interval":"12h","at":"03:00",` + disk + `}`,
		"bad at":           `{"at":"25:00",` + disk + `}`,
		"keeps nothing":    `{"retention":{},` + disk + `}`,
		"negative keep":    `{"retention":{"keepLast":-1},` + disk + `}`,
		"bad within":       `{"retention":{"keepWithin":"forever"},` + disk + `}`,
		"bad prune":        `{"prune":{"interval":"1m"},` + disk + `}`,
		"unknown kind":     `{"target":{"kind":"tape"}}`,
		"bucket on memory": `{"target":{"kind":"bucket"}}`,
		"no backing":       `{"target":{"kind":"backing"}}`,
		"unknown backing":  `{"target":{"kind":"backing","backing":"nope"}}`,
		"no aux on memory": `{"target":{"kind":"backing","backing":"mem"}}`,
		"chosen repo":      `{"target":{"kind":"backing","backing":"disk","repo":"snap-mine"}}`,
		"default target":   `{}`, // memory bucket, memory default backing
	} {
		if _, err := e.svc.PutConfig(ctx, "mem1", json.RawMessage(doc)); !errors.Is(err, jobs.ErrInvalidSpec) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.svc.PutConfig(ctx, "nope", json.RawMessage(`{}`)); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("unknown bucket: %v", err)
	}
	if _, err := e.svc.Config(ctx, "mem1"); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("Config after refused PUTs: %v", err)
	}

	c := e.configure("mem1", `{`+disk+`}`)
	want := Config{Enabled: true, Interval: "24h", Consistent: true, Retention: defaultRetention,
		Target: Target{Kind: TargetBacking, Backing: "disk", Repo: c.Target.Repo}, Prune: PruneConfig{Enabled: true, Interval: "168h", Default: true}}
	if c != want || !validRepoName(c.Target.Repo) {
		t.Fatalf("defaults %+v, want %+v", c, want)
	}
	// The repository was created.
	if info, err := e.svc.Repository(ctx, "mem1"); err != nil || info == nil || !info.Initialized {
		t.Fatalf("repository %+v, %v", info, err)
	}
	// Later PUTs keep the repository name; it cannot be changed.
	c2 := e.configure("mem1", `{"enabled":false,"interval":"48h","at":"3:30","retention":{"keepDaily":3},"target":{"backing":"disk"},"prune":{"enabled":false}}`)
	if c2.Target.Repo != c.Target.Repo || c2.At != "03:30" || c2.Interval != "48h" || c2.Enabled || c2.Prune.Enabled || c2.Prune.Default ||
		c2.Retention != (Retention{KeepDaily: 3, KeepWithin: "0s"}) {
		t.Fatalf("second PUT %+v", c2)
	}
	e.configure("mem1", `{"target":{"kind":"backing","backing":"disk","repo":"`+c.Target.Repo+`"}}`)
	if _, err := e.svc.PutConfig(ctx, "mem1", json.RawMessage(`{"target":{"kind":"backing","backing":"disk","repo":"snap-other"}}`)); !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Fatalf("changed repo: %v", err)
	}
	// The name survives DeleteConfig.
	if err := e.svc.DeleteConfig(ctx, "mem1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Config(ctx, "mem1"); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("Config after delete: %v", err)
	}
	if c3 := e.configure("mem1", `{`+disk+`}`); c3.Target.Repo != c.Target.Repo {
		t.Fatalf("repo after delete %q, want %q", c3.Target.Repo, c.Target.Repo)
	}
	raw, err := e.svc.Config(ctx, "mem1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := parseStoredConfig(raw); got.Target.Repo != c.Target.Repo {
		t.Fatalf("stored %s", raw)
	}

	// An fs bucket defaults to its own repository; prune is off by default
	// when its archive scope is "all".
	e.createBucket("fs1", "disk", "")
	if err := e.router.PutBucketConfig(ctx, "fs1", storage.ConfigEncryptionProvider, []byte(`{"provider":"restic","scope":"all"}`)); err != nil {
		t.Fatal(err)
	}
	c4 := e.configure("fs1", `{}`)
	if c4.Target != (Target{Kind: TargetBucket}) || c4.Prune.Enabled || !c4.Prune.Default {
		t.Fatalf("fs bucket defaults %+v", c4)
	}
	if info, err := e.svc.Repository(ctx, "fs1"); err != nil || info == nil || info.BucketID == "" {
		t.Fatalf("fs bucket repository %+v, %v", info, err)
	}
}

// A memory bucket defaults to an aux repository on an fs default backing.
func TestPutConfigDefaultAux(t *testing.T) {
	e := newEnv(t, "disk", Options{Incremental: true})
	e.createBucket("mem1", "mem", "")
	c := e.configure("mem1", `{"retention":{"keepLast":2}}`)
	if c.Target.Kind != TargetBacking || c.Target.Backing != "disk" || !validRepoName(c.Target.Repo) {
		t.Fatalf("target %+v", c.Target)
	}
}

// Without a configuration there is nothing to run, list or reveal.
func TestUnconfigured(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("b1", "", "")
	if _, err := e.svc.RunNow(ctx, "b1", "x"); !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Fatalf("RunNow: %v", err)
	}
	if _, err := e.svc.Prune(ctx, "b1", "x"); !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Fatalf("Prune: %v", err)
	}
	if snaps, err := e.svc.Snapshots(ctx, "b1"); err != nil || snaps != nil {
		t.Fatalf("Snapshots: %v, %v", snaps, err)
	}
	if info, err := e.svc.Repository(ctx, "b1"); err != nil || info != nil {
		t.Fatalf("Repository: %v, %v", info, err)
	}
	if _, _, err := e.svc.Credentials(ctx, "b1"); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("Credentials: %v", err)
	}
	raw, err := e.svc.Status(ctx, "b1")
	if err != nil || string(raw) != `{"lastSnapshot":"","lastError":"","running":""}` {
		t.Fatalf("Status: %s, %v", raw, err)
	}
	if _, err := e.svc.RunNow(ctx, "nope", "x"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatalf("RunNow of an unknown bucket: %v", err)
	}
}

// A prune job, and the credentials of the repository.
func TestPruneAndCredentials(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("b1", "", "")
	e.configure("b1", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	loc, info, err := e.svc.Credentials(ctx, "b1")
	if err != nil || loc.Password == "" || info == nil || loc.Repository != info.Repository {
		t.Fatalf("credentials %+v %+v %v", loc.Repository, info, err)
	}
	j, err := e.svc.Prune(ctx, "b1", "x")
	if err != nil {
		t.Fatal(err)
	}
	if j.Labels["bucket"] != "b1" || len(j.Labels) != 1 {
		t.Fatalf("labels %v", j.Labels)
	}
	if j = e.wait(j.ID); j.State != jobs.StateSucceeded {
		t.Fatalf("prune: %s %s", j.State, j.LastError)
	}
	if e.svc.stateOf("b1").LastPruneAt.IsZero() {
		t.Fatal("prune not recorded")
	}
}

// nextRun schedules by interval, or at a time of day.
func TestNextRun(t *testing.T) {
	day := func(d, h, m int) time.Time { return time.Date(2024, 6, d, h, m, 0, 0, time.UTC) }
	cfg := func(interval, at string) Config {
		return Config{Enabled: true, Interval: interval, At: at}
	}
	for _, c := range []struct {
		name      string
		cfg       Config
		last, now time.Time
		want      time.Time
	}{
		{"disabled", Config{Interval: "1h"}, time.Time{}, day(10, 12, 0), time.Time{}},
		{"never run", cfg("6h", ""), time.Time{}, day(10, 12, 0), day(10, 12, 0)},
		{"interval", cfg("6h", ""), day(10, 1, 30), day(10, 12, 0), day(10, 7, 30)},
		{"at, never run, later today", cfg("24h", "18:00"), time.Time{}, day(10, 12, 0), day(10, 18, 0)},
		{"at, never run, tomorrow", cfg("24h", "03:00"), time.Time{}, day(10, 12, 0), day(11, 3, 0)},
		{"at, daily", cfg("24h", "03:00"), day(10, 3, 0), day(10, 12, 0), day(11, 3, 0)},
		{"at, daily, late run", cfg("24h", "03:00"), day(10, 15, 0), day(10, 16, 0), day(11, 3, 0)},
		{"at, daily, early manual run", cfg("24h", "03:00"), day(10, 2, 0), day(10, 2, 30), day(10, 3, 0)},
		{"at, two days", cfg("48h", "03:00"), day(10, 3, 1), day(10, 12, 0), day(12, 3, 0)},
		{"at, 36h rounds up", cfg("36h", "03:00"), day(10, 3, 0), day(10, 12, 0), day(12, 3, 0)},
		{"at, weekly", cfg("168h", "23:30"), day(10, 23, 30), day(11, 0, 0), day(17, 23, 30)},
	} {
		if got := nextRun(c.cfg, bucketState{LastAttemptAt: c.last}, c.now); !got.Equal(c.want) {
			t.Errorf("%s: next run %v, want %v", c.name, got, c.want)
		}
	}
}

// The scheduler submits due snapshots, once.
func TestSchedulerTick(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("b1", "", "")
	e.createBucket("b2", "", "")
	e.put("b1", "k", "v", storage.ObjectMeta{})
	e.configure("b1", `{"interval":"1h","target":{"kind":"backing","backing":"disk"}}`)
	e.configure("b2", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	e.svc.tick(ctx)
	e.svc.tick(ctx)
	list := e.mgr.List(jobs.Filter{Kind: KindSnapshot})
	if len(list) != 1 || list[0].Labels["bucket"] != "b1" || list[0].CreatedBy != "scheduler" {
		t.Fatalf("jobs %+v", list)
	}
	if j := e.wait(list[0].ID); j.State != jobs.StateSucceeded {
		t.Fatalf("scheduled job: %s", j.LastError)
	}
	// Not due again for an hour.
	e.svc.tick(ctx)
	if n := len(e.mgr.List(jobs.Filter{Kind: KindSnapshot})); n != 1 {
		t.Fatalf("%d jobs after a second tick", n)
	}
	e.svc.now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	e.svc.tick(ctx)
	if n := len(e.mgr.List(jobs.Filter{Kind: KindSnapshot})); n != 2 {
		t.Fatalf("%d jobs an hour later", n)
	}
	for _, j := range e.mgr.List(jobs.Filter{Kind: KindSnapshot}) {
		e.wait(j.ID)
	}
	// The state survives a restart of the service.
	svc, err := New(Options{Router: e.router, SystemDir: e.svc.sysDir})
	if err != nil {
		t.Fatal(err)
	}
	if st := svc.stateOf("b1"); st.LastSnapshot == "" || st.LastAttemptAt.IsZero() || st.Target == nil {
		t.Fatalf("reloaded state %+v", st)
	}
}
