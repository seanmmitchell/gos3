package admin

import (
	"encoding/json"
	"testing"

	"github.com/seanmmitchell/gos3/internal/jobs"
)

type reportingCopyJobs struct {
	*fakeCopyJobs
	requested []string // guarded by fakeCopyJobs.mu
}

func (s *reportingCopyJobs) RequestFullPass(j jobs.Job) {
	if j.Kind != jobKindMirror {
		return
	}
	s.mu.Lock()
	s.requested = append(s.requested, j.ID)
	s.mu.Unlock()
}

func (s *reportingCopyJobs) JobStatus(j jobs.Job) json.RawMessage {
	if j.Kind == jobKindMirror {
		return json.RawMessage(`{"pending":7,"checkRequired":true}`)
	}
	return j.Status
}

func TestMirrorRunRequestsFullPassAndViewsShareLiveStatus(t *testing.T) {
	e := newJobsEnv(t)
	reporter := &reportingCopyJobs{fakeCopyJobs: e.copies}
	e.api.d.CopyJobs = reporter
	j := e.createJob(copySpec("mirror", "src", "dst"))
	e.waitJob(j.ID, jobs.StateWaiting)
	var got jobs.Job
	e.root("GET", "/jobs/"+j.ID, nil).json(t, &got)
	want := string(got.Status)
	if want != `{"pending":7,"checkRequired":true}` || got.Checkpoint != nil {
		t.Fatalf("live job: %+v", got)
	}
	for _, path := range []string{"/jobs", "/buckets/src/jobs", "/buckets/dst/jobs"} {
		var list jobList
		e.root("GET", path, nil).json(t, &list)
		if len(list.Jobs) != 1 || string(list.Jobs[0].Status) != want || list.Jobs[0].Checkpoint != nil {
			t.Fatalf("%s: %+v", path, list)
		}
	}
	e.root("POST", "/jobs/"+j.ID+"/run", nil).json(t, &got)
	if string(got.Status) != want {
		t.Fatalf("run response: %+v", got)
	}
	reporter.mu.Lock()
	if len(reporter.requested) != 1 || reporter.requested[0] != j.ID {
		t.Errorf("full comparisons requested: %v", reporter.requested)
	}
	reporter.mu.Unlock()
	e.root("POST", "/jobs/"+j.ID+"/pause", nil).expectStatus(t, 200)
	e.waitJob(j.ID, jobs.StatePaused)
	e.root("POST", "/jobs/"+j.ID+"/run", nil).expectError(t, 409, codeConflict)
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.requested) != 1 {
		t.Fatal("rejected run requested work")
	}
}
