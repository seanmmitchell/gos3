package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/jobs"
)

// Background jobs (docs/admin-api.md "Jobs: copy and mirror"). Jobs are
// run by Deps.Jobs; copy and mirror jobs are submitted here, restic jobs by
// the snapshot endpoints. A job's buckets are its labels "source" and
// "target" (copy, mirror, restore) or "bucket", set by the job's runner:
// they decide which jobs a bucket lists and what admin:GetJob and
// admin:UpdateJob are evaluated against (every one of the job's buckets;
// service level for a job without bucket labels).

// Job kinds that POST /jobs submits.
const (
	jobKindCopy   = "copy"
	jobKindMirror = "mirror"
)

// Labels naming a job's buckets.
var jobBucketLabels = []string{"source", "target", "bucket"}

// errJobsUnavailable is the error of the jobs endpoints on a server without
// a job manager.
func errJobsUnavailable() error {
	return notFound("background jobs are not available on this server")
}

// jobBuckets returns the distinct buckets job j works on (see
// jobBucketLabels).
func jobBuckets(j jobs.Job) []string {
	var out []string
	for _, l := range jobBucketLabels {
		if b := j.Labels[l]; b != "" && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// publicJob returns j as the API shows it: without its checkpoint (a
// runner's internal resume state).
func (a *api) publicJob(j jobs.Job) jobs.Job {
	if reporter, ok := a.d.CopyJobs.(interface {
		JobStatus(jobs.Job) json.RawMessage
	}); ok {
		j.Status = reporter.JobStatus(j)
	}
	j.Checkpoint = nil
	return j
}

// jobList is the response of the job listings.
type jobList struct {
	Jobs []jobs.Job `json:"jobs"`
}

func (a *api) newJobList(js []jobs.Job) jobList {
	out := jobList{Jobs: make([]jobs.Job, 0, len(js))}
	for _, j := range js {
		out.Jobs = append(out.Jobs, a.publicJob(j))
	}
	return out
}

// callerName names the caller as a job's creator.
func callerName(rc *reqCtx) string {
	switch {
	case rc.id.User != "":
		return rc.id.User
	case rc.id.IsRoot:
		return auth.RootUser
	}
	return rc.id.AccessKey
}

// jobFilter parses the kind and state query parameters of a job listing.
func jobFilter(r *http.Request) (jobs.Filter, error) {
	q := r.URL.Query()
	f := jobs.Filter{Kind: q.Get("kind")}
	if s := jobs.State(q.Get("state")); s != "" {
		if !s.Valid() {
			return jobs.Filter{}, badRequest("state must be one of queued, running, waiting, paused, succeeded, failed or canceled, got %q", s)
		}
		f.State = s
	}
	return f, nil
}

// onBucket returns a filter function matching the jobs working on bucket.
func onBucket(bucket string) func(jobs.Job) bool {
	return func(j jobs.Job) bool { return slices.Contains(jobBuckets(j), bucket) }
}

// listJobs implements GET /jobs?kind=&state=&bucket= (admin:ListJobs at
// service level): every job, newest first; bucket matches jobs whose
// source or target is the bucket.
func (a *api) listJobs(rc *reqCtx) (int, any, error) {
	if a.d.Jobs == nil {
		return 0, nil, errJobsUnavailable()
	}
	f, err := jobFilter(rc.r)
	if err != nil {
		return 0, nil, err
	}
	if b := rc.r.URL.Query().Get("bucket"); b != "" {
		f.Match = onBucket(b)
	}
	return http.StatusOK, a.newJobList(a.d.Jobs.List(f)), nil
}

// listBucketJobs implements GET /buckets/{name}/jobs (admin:GetJob on the
// bucket): the jobs whose source or target is the bucket, also after the
// bucket was deleted (their history stays until it expires). Like GET
// /jobs/{id}, it shows a job only to callers allowed admin:GetJob on each
// of its buckets: a grant on the bucket alone does not show the jobs that
// copy it to, or from, a bucket the caller may not see.
func (a *api) listBucketJobs(rc *reqCtx) (int, any, error) {
	if a.d.Jobs == nil {
		return 0, nil, errJobsUnavailable()
	}
	f, err := jobFilter(rc.r)
	if err != nil {
		return 0, nil, err
	}
	bucket := rc.r.PathValue("name")
	allowed := map[string]bool{bucket: true} // the route's authorization
	f.Match = func(j jobs.Job) bool {
		buckets := jobBuckets(j)
		if !slices.Contains(buckets, bucket) {
			return false
		}
		for _, b := range buckets {
			ok, seen := allowed[b]
			if !seen {
				ok = a.authorize(rc, rc.action, iam.AdminBucketARN(b), nil).Allowed
				allowed[b] = ok
			}
			if !ok {
				return false
			}
		}
		return true
	}
	return http.StatusOK, a.newJobList(a.d.Jobs.List(f)), nil
}

// copyJobHead is the part of a CopyJobSpec the API needs before handing
// the spec to the runner: the kind and the buckets authorization is
// evaluated against. It is decoded from exactly the bytes the runner gets,
// with encoding/json into a struct as the runner does, so both read the
// same buckets (duplicate or differently cased keys included).
type copyJobHead struct {
	Kind   string `json:"kind"`
	Source struct {
		Bucket string `json:"bucket"`
	} `json:"source"`
	Target struct {
		Bucket string `json:"bucket"`
		Create *struct {
			Backing string `json:"backing"`
		} `json:"create"`
	} `json:"target"`
}

// dryRunResponse is the response of POST /jobs with "dryRun": true.
type dryRunResponse struct {
	Spec     json.RawMessage `json:"spec"`
	Warnings []string        `json:"warnings"`
}

// createJob implements POST /jobs: a copy or mirror job. The caller needs
// admin:CreateCopyJob on both the source and the target bucket (evaluated
// here: they come from the body) before the spec is validated, so that
// validation errors tell nothing about buckets the caller may not use.
// With "dryRun": true the spec is validated and normalized by the copy job
// service and nothing is created (200); otherwise the job is submitted
// without "dryRun" (201).
func (a *api) createJob(rc *reqCtx) (int, any, error) {
	body, err := rc.readJSONObject()
	if err != nil {
		return 0, nil, err
	}
	spec, dryRun, err := splitDryRun(body)
	if err != nil {
		return 0, nil, err
	}
	var head copyJobHead
	if err := json.Unmarshal(spec, &head); err != nil {
		return 0, nil, badRequest("invalid job spec: %s", jsonErrorMessage(err))
	}
	source, target := head.Source.Bucket, head.Target.Bucket
	if err := a.requireAction(rc, rc.action, iam.AdminBucketARN(source), nil); err != nil {
		return 0, nil, err
	}
	if target != source {
		if err := a.requireAction(rc, rc.action, iam.AdminBucketARN(target), nil); err != nil {
			return 0, nil, err
		}
	}
	if head.Target.Create != nil {
		if spec, err = a.authorizeJobBacking(rc, spec, head.Target.Create.Backing); err != nil {
			return 0, nil, err
		}
	}
	switch {
	case head.Kind != jobKindCopy && head.Kind != jobKindMirror:
		return 0, nil, badRequest("kind must be %q or %q, got %q (restic jobs are started by the snapshot endpoints)",
			jobKindCopy, jobKindMirror, head.Kind)
	case source == "":
		return 0, nil, badRequest("\"source\": {\"bucket\": …} is required")
	case target == "":
		return 0, nil, badRequest("\"target\": {\"bucket\": …} is required")
	}
	if dryRun {
		if a.d.CopyJobs == nil {
			return 0, nil, notFound("copy jobs are not available on this server")
		}
		norm, warnings, err := a.d.CopyJobs.DryRun(rc.ctx, spec)
		if err != nil {
			return 0, nil, err
		}
		if norm == nil {
			norm = spec
		}
		if warnings == nil {
			warnings = []string{}
		}
		return http.StatusOK, dryRunResponse{Spec: norm, Warnings: warnings}, nil
	}
	if a.d.Jobs == nil {
		return 0, nil, errJobsUnavailable()
	}
	j, err := a.d.Jobs.Submit(rc.ctx, head.Kind, spec, callerName(rc))
	if err != nil {
		return 0, nil, err
	}
	if err := a.checkJobBuckets(j, source, target); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: job created", "job", j.ID, "kind", j.Kind, "source", source, "target", target,
		"by", describeIdentity(rc.id))
	return http.StatusCreated, a.publicJob(j), nil
}

// Authorize the effective backing before validation or submission. Pin
// requests using target.create to the same boundary as placement changes.
func (a *api) authorizeJobBacking(rc *reqCtx, spec json.RawMessage, backing string) (json.RawMessage, error) {
	if backing == "" {
		r, err := a.router()
		if err != nil {
			return nil, err
		}
		backing = r.DefaultBacking()
	}
	if err := a.requireAction(rc, actionCreateCopyJob, iam.AdminBackingARN(backing), nil); err != nil {
		return nil, err
	}
	// encoding/json merges duplicate struct members. Append an override so
	// validation and the worker use the authorized backing even if the
	// server's default changes while the job is queued. Preserve all other
	// members verbatim, including their ordering and strict validation.
	override, err := json.Marshal(map[string]any{"target": map[string]any{"create": map[string]string{"backing": backing}}})
	if err != nil {
		return nil, err
	}
	spec = bytes.TrimSpace(spec)
	out := append([]byte(nil), spec[:len(spec)-1]...)
	out = append(out, ',')
	return append(out, override[1:]...), nil
}

// splitDryRun returns the JSON object body without its "dryRun" member and
// that member's value. Members are removed by how encoding/json matches
// them to the field (case-insensitively, every duplicate), and the other
// members are kept as they are, in their order, so the runner reads them
// as copyJobHead was read.
func splitDryRun(body json.RawMessage) (json.RawMessage, bool, error) {
	var flag struct {
		DryRun bool `json:"dryRun"`
	}
	if err := json.Unmarshal(body, &flag); err != nil {
		return nil, false, badRequest("invalid job spec: %s", jsonErrorMessage(err))
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false, badRequest("invalid JSON body: the body must be a JSON object")
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	n := 0
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false, badRequest("invalid JSON body: %s", jsonErrorMessage(err))
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false, badRequest("invalid JSON body: %s", jsonErrorMessage(err))
		}
		k, err := json.Marshal(key)
		if err != nil {
			return nil, false, err
		}
		if isDryRunMember(k) {
			continue
		}
		if n > 0 {
			buf.WriteByte(',')
		}
		n++
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), flag.DryRun, nil
}

// isDryRunMember reports whether encoding/json decodes the member named by
// the JSON string key into a field tagged "dryRun".
func isDryRunMember(key []byte) bool {
	var probe struct {
		DryRun *json.RawMessage `json:"dryRun"`
	}
	doc := append(append([]byte{'{'}, key...), ":0}"...)
	return json.Unmarshal(doc, &probe) == nil && probe.DryRun != nil
}

// checkJobBuckets makes sure that job j, just created for a request
// authorized on the buckets allowed, works on no other bucket (its
// runner's labels). Otherwise the request and the runner read different
// buckets from the spec: the job is canceled and the request fails.
func (a *api) checkJobBuckets(j jobs.Job, allowed ...string) error {
	for _, b := range jobBuckets(j) {
		if slices.Contains(allowed, b) {
			continue
		}
		a.log.Error("admin: a new job works on a bucket the request was not authorized for; canceling it",
			"job", j.ID, "kind", j.Kind, "bucket", b, "authorized", allowed)
		if a.d.Jobs != nil {
			if err := a.d.Jobs.Cancel(j.ID); err != nil && !errors.Is(err, jobs.ErrConflict) {
				a.log.Error("admin: canceling the job failed", "job", j.ID, "err", err)
			}
		}
		return errorf(http.StatusInternalServerError, codeInternalError,
			"job %s was canceled: it does not work on the buckets the request named", j.ID)
	}
	return nil
}

// authorizedJob returns the {id} job once rc.action (admin:GetJob or
// admin:UpdateJob) is allowed on every one of its buckets. An unknown job
// (and a server without jobs) is evaluated at service level, so that a
// caller allowed on some buckets only cannot tell unknown IDs from other
// buckets' jobs.
func (a *api) authorizedJob(rc *reqCtx) (jobs.Job, error) {
	if a.d.Jobs == nil {
		if err := a.requireAction(rc, rc.action, "", nil); err != nil {
			return jobs.Job{}, err
		}
		return jobs.Job{}, errJobsUnavailable()
	}
	j, err := a.d.Jobs.Get(rc.r.PathValue("id"))
	if err != nil {
		if err := a.requireAction(rc, rc.action, "", nil); err != nil {
			return jobs.Job{}, err
		}
		return jobs.Job{}, err
	}
	buckets := jobBuckets(j)
	if len(buckets) == 0 {
		return j, a.requireAction(rc, rc.action, "", nil)
	}
	for _, b := range buckets {
		if err := a.requireAction(rc, rc.action, iam.AdminBucketARN(b), nil); err != nil {
			return jobs.Job{}, err
		}
	}
	return j, nil
}

// getJob implements GET /jobs/{id}.
func (a *api) getJob(rc *reqCtx) (int, any, error) {
	j, err := a.authorizedJob(rc)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, a.publicJob(j), nil
}

// updateJob returns the handler of POST /jobs/{id}/<op>: it applies op to
// the authorized job and returns the job as it is then.
func (a *api) updateJob(name string, op func(m *jobs.Manager, j jobs.Job) error) handlerFunc {
	return func(rc *reqCtx) (int, any, error) {
		j, err := a.authorizedJob(rc)
		if err != nil {
			return 0, nil, err
		}
		if err := op(a.d.Jobs, j); err != nil {
			return 0, nil, err
		}
		a.log.Info("admin: job "+name, "job", j.ID, "kind", j.Kind, "by", describeIdentity(rc.id))
		if j, err = a.d.Jobs.Get(j.ID); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, a.publicJob(j), nil
	}
}

func pauseJob(m *jobs.Manager, j jobs.Job) error  { return m.Pause(j.ID) }
func resumeJob(m *jobs.Manager, j jobs.Job) error { return m.Resume(j.ID) }
func cancelJob(m *jobs.Manager, j jobs.Job) error { return m.Cancel(j.ID) }

// runJob requests a full mirror comparison or starts a pending retry.
// A queued job stays queued; a running mirror runs again if needed to
// include the comparison request. Paused and finished jobs are 409.
func (a *api) runJob(m *jobs.Manager, j jobs.Job) error {
	switch {
	case j.State == jobs.StatePaused:
		return errorf(http.StatusConflict, codeConflict, "job %s is paused: resume it instead", j.ID)
	case j.State.Finished():
		return errorf(http.StatusConflict, codeConflict, "job %s is %s and cannot run again", j.ID, j.State)
	}
	if requester, ok := a.d.CopyJobs.(interface{ RequestFullPass(jobs.Job) }); ok {
		requester.RequestFullPass(j)
	}
	m.Kick(j.ID)
	return nil
}

// deleteJob implements DELETE /jobs/{id}: removes a finished job (409
// otherwise: cancel it first).
func (a *api) deleteJob(rc *reqCtx) (int, any, error) {
	j, err := a.authorizedJob(rc)
	if err != nil {
		return 0, nil, err
	}
	if err := a.d.Jobs.Delete(j.ID); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: job deleted", "job", j.ID, "kind", j.Kind, "by", describeIdentity(rc.id))
	return http.StatusNoContent, nil, nil
}
