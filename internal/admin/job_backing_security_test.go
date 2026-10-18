package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/seanmmitchell/gos3/internal/copyjob"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestCopyJobBackingAuthorization(t *testing.T) {
	e, router := newRouterEnv(t)
	s := copyjob.New(copyjob.Options{Router: router})
	m, err := jobs.Open(jobs.Options{Runners: map[string]jobs.Runner{"copy": s.Runner("copy"), "mirror": s.Runner("mirror")}})
	if err != nil {
		t.Fatal(err)
	}
	s.Attach(m)
	t.Cleanup(func() { _ = m.Close(); s.Close() })
	e.api.d.Jobs, e.api.d.CopyJobs = m, s
	e.putPolicy("jobs", map[string]any{"Effect": "Allow", "Action": "admin:CreateCopyJob", "Resource": "arn:gos3:admin:::*"},
		map[string]any{"Effect": "Deny", "Action": "admin:CreateCopyJob", "Resource": "arn:gos3:admin:::backing/default"})
	user := e.addUser("delegate", "jobs")
	for _, kind := range []string{"copy", "mirror"} {
		for _, dry := range []bool{false, true} {
			for _, create := range []string{`{}`, `{"backing":"default"}`, `{"backing":"fast","BACKING":"default"}`} {
				body := fmt.Sprintf(`{"kind":%q,"source":{"bucket":"photos"},"target":{"bucket":"denied","create":%s},"dryRun":%t}`, kind, create, dry)
				e.do(&user, "POST", Prefix+"/jobs", body).expectError(t, 403, codeAccessDenied)
			}
		}
	}
	if len(m.List(jobs.Filter{})) != 0 {
		t.Fatal("denied requests submitted a job")
	}
	if _, err := router.GetBucket(t.Context(), "denied"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Fatal(err)
	}
	// An explicitly allowed backing still works through the real runner.
	var j jobs.Job
	e.do(&user, "POST", Prefix+"/jobs", `{"kind":"copy","source":{"bucket":"photos"},"target":{"bucket":"allowed","create":{"backing":"fast"}}}`).json(t, &j)
	waitFor(t, func() bool { j, err = m.Get(j.ID); return err == nil && j.State.Finished() })
	if j.State != jobs.StateSucceeded {
		t.Fatal(j.LastError)
	}
	info, err := router.GetBucket(t.Context(), "allowed")
	if err != nil || info.Backing != "fast" {
		t.Fatalf("target: %+v, %v", info, err)
	}
	// Omitted backings are pinned in the persisted spec.
	e.root("POST", "/jobs", `{"kind":"copy","source":{"bucket":"photos"},"target":{"bucket":"pinned","create":{}}}`).json(t, &j)
	var spec copyjob.Spec
	if err := json.Unmarshal(j.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Target.Create == nil || spec.Target.Create.Backing != "default" {
		t.Fatalf("unpinned spec: %s", j.Spec)
	}
}

func TestRestoreBackingAuthorization(t *testing.T) {
	e, _ := newRouterEnv(t)
	snaps := newFakeSnapshots()
	e.api.d.Snapshots = snaps
	e.putPolicy("restore", map[string]any{"Effect": "Allow", "Action": []string{"admin:RestoreBucketSnapshot", "admin:CreateCopyJob"}, "Resource": "arn:gos3:admin:::*"},
		map[string]any{"Effect": "Deny", "Action": "admin:CreateCopyJob", "Resource": "arn:gos3:admin:::backing/default"})
	user := e.addUser("delegate", "restore")
	path := Prefix + "/buckets/photos/restic/snapshots/" + testSnapshotID + "/restore"
	for _, create := range []string{`{}`, `{"backing":"default"}`, `{"backing":"fast","BACKING":"default"}`} {
		e.do(&user, "POST", path, `{"target":{"bucket":"restored","create":`+create+`}}`).expectError(t, 403, codeAccessDenied)
	}
	if len(snaps.restores) != 0 {
		t.Fatal("unauthorized restore reached service")
	}
}
