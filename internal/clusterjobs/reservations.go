package clusterjobs

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/copyjob"
	"github.com/seanmmitchell/gos3/internal/jobs"
)

// A fail copy needs an exclusive target. Two explicit merges may
// share a range because neither deletes the other's versions.
func checkCopyTargets(candidate jobs.Job, records []cluster.JobRecord, runningOnly bool) error {
	if candidate.Kind != copyjob.KindCopy {
		return nil
	}
	var target copySpec
	if err := json.Unmarshal(candidate.Spec, &target); err != nil {
		return err
	}
	if target.Target.Bucket == "" {
		return nil
	}
	for _, raw := range records {
		if raw.ID == candidate.ID || strings.HasPrefix(raw.ID, "replication_") {
			continue
		}
		other, err := decode(raw.ID, raw.Data)
		if err != nil {
			return err
		}
		if other.Kind != copyjob.KindCopy || other.State.Finished() || runningOnly && other.State != jobs.StateRunning {
			continue
		}
		var spec copySpec
		if err := json.Unmarshal(other.Spec, &spec); err != nil {
			return err
		}
		if spec.Target.Bucket != target.Target.Bucket {
			continue
		}
		if !strings.HasPrefix(spec.Target.Prefix, target.Target.Prefix) && !strings.HasPrefix(target.Target.Prefix, spec.Target.Prefix) {
			continue
		}
		if spec.Options.OnExisting == copyjob.OnExistingMerge && target.Options.OnExisting == copyjob.OnExistingMerge {
			continue
		}
		return fmt.Errorf("%w: copy target %s/%s overlaps active job %s", jobs.ErrConflict, target.Target.Bucket, target.Target.Prefix, other.ID)
	}
	return nil
}
