package copyjob

import (
	"encoding/json"

	"github.com/seanmmitchell/gos3/internal/jobs"
)

// RequestFullPass makes the next mirror run compare every key. The caller
// queues the job after recording this request, so even an idle mirror
// with no source notifications performs a fresh reconciliation.
func (s *Service) RequestFullPass(j jobs.Job) {
	if j.Kind != KindMirror {
		return
	}
	var sp Spec
	if json.Unmarshal(j.Spec, &sp) != nil {
		return
	}
	w := s.register(j.ID, sp)
	w.mu.Lock()
	w.fullRequested = true
	w.mu.Unlock()
}

// JobStatus overlays live source notifications on the persisted report.
// A queue can be empty while a full comparison is required; neither fact
// establishes that the destination currently agrees with the source.
func (s *Service) JobStatus(j jobs.Job) json.RawMessage {
	if j.Kind != KindMirror || j.State.Finished() {
		return j.Status
	}
	var st Status
	if len(j.Status) > 0 && json.Unmarshal(j.Status, &st) != nil {
		return j.Status
	}
	s.mu.Lock()
	w := s.watches[j.ID]
	s.mu.Unlock()
	if w == nil {
		st.CheckRequired = true
	} else {
		n, oldest := w.pending()
		st.Pending, st.LagSeconds = int64(n), lag(oldest)
		w.mu.Lock()
		st.CheckRequired = st.CheckRequired || !w.verified || w.fullRequested || w.overflow || w.configDirty
		w.mu.Unlock()
	}
	data, err := json.Marshal(st)
	if err != nil {
		return j.Status
	}
	return data
}
