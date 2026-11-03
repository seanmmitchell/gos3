package clusterjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/seanmmitchell/gos3/internal/copyjob"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
)

// CopyRunner executes idempotent version-preserving copies in one cluster
// namespace. Cluster-aware mirror ownership requires separate persisted state.
type CopyRunner struct {
	Backend  storage.Backend
	IsLeader func() bool
}

type copySpec struct {
	copyjob.Spec
	SourceInstanceID string `json:"clusterSourceInstanceID,omitempty"`
	TargetInstanceID string `json:"clusterTargetInstanceID,omitempty"`
	TargetExisted    bool   `json:"clusterTargetExisted,omitempty"`
}

type jobBucketCreator interface {
	CreateBucketForJob(context.Context, string, time.Time, string, storage.CreateBucketOptions) (storage.BucketInfo, error)
}

func (r *CopyRunner) Validate(ctx context.Context, raw json.RawMessage) (json.RawMessage, map[string]string, error) {
	sp, err := copyjob.NormalizeSpec(raw, copyjob.KindCopy)
	if err != nil {
		return nil, nil, err
	}
	if r.Backend == nil {
		return nil, nil, fmt.Errorf("%w: cluster storage is unavailable", jobs.ErrInvalidSpec)
	}
	if sp.DryRun {
		return nil, nil, fmt.Errorf("%w: dry run cannot be submitted", jobs.ErrInvalidSpec)
	}
	if create := sp.Target.Create; create != nil {
		if create.Backing != "" || create.Kind != "" || create.Container != "" || create.RemoteBucket != "" {
			return nil, nil, fmt.Errorf("%w: cluster target.create cannot select a node-local backing or bucket kind", jobs.ErrInvalidSpec)
		}
		if !copyjob.ValidBucketName(sp.Target.Bucket) {
			return nil, nil, fmt.Errorf("%w: invalid target bucket name %q", jobs.ErrInvalidSpec, sp.Target.Bucket)
		}
		if _, ok := r.Backend.(jobBucketCreator); !ok {
			return nil, nil, fmt.Errorf("%w: cluster target creation is unavailable", jobs.ErrInvalidSpec)
		}
	}
	if sp.Options.Encryption != copyjob.EncryptionPreserve || sp.Options.Compression != copyjob.CompressionPreserve {
		return nil, nil, fmt.Errorf("%w: cluster copy jobs currently require preserved encryption and compression", jobs.ErrInvalidSpec)
	}
	source, err := r.Backend.GetBucket(ctx, sp.Source.Bucket)
	if err != nil {
		return nil, nil, err
	}
	if err := validateClusterConfigs(ctx, r.Backend, sp); err != nil {
		return nil, nil, err
	}
	target, err := r.Backend.GetBucket(ctx, sp.Target.Bucket)
	targetExists := err == nil
	if errors.Is(err, storage.ErrBucketNotFound) && sp.Target.Create != nil {
		lock := sp.Target.Create.ObjectLock
		if lock == nil {
			v := source.ObjectLockEnabled
			sp.Target.Create.ObjectLock = &v
		}
		target = storage.BucketInfo{Name: sp.Target.Bucket, ObjectLockEnabled: *sp.Target.Create.ObjectLock, Versioning: storage.VersioningEnabled}
	} else if err != nil {
		return nil, nil, err
	}
	if sp.Options.Versions == "" {
		if target.Versioning == storage.VersioningEnabled {
			sp.Options.Versions = copyjob.VersionsAll
		} else {
			sp.Options.Versions = copyjob.VersionsCurrent
		}
	}
	if sp.Options.Versions == copyjob.VersionsAll && target.Versioning != storage.VersioningEnabled {
		return nil, nil, fmt.Errorf("%w: copying all versions requires target versioning", jobs.ErrInvalidSpec)
	}
	if source.ObjectLockEnabled && !target.ObjectLockEnabled && !sp.Options.DropRetention {
		return nil, nil, fmt.Errorf("%w: target object lock is required to preserve source retention", jobs.ErrInvalidSpec)
	}
	if targetExists && sp.Options.OnExisting == copyjob.OnExistingFail {
		empty, err := clusterCopyRangeEmpty(ctx, r.Backend, sp.Target.Bucket, sp.Target.Prefix)
		if err != nil {
			return nil, nil, err
		}
		if !empty {
			return nil, nil, fmt.Errorf("%w: target range is not empty", jobs.ErrConflict)
		}
	}
	norm, err := json.Marshal(copySpec{Spec: sp, SourceInstanceID: source.InstanceID, TargetInstanceID: target.InstanceID, TargetExisted: targetExists})
	if err != nil {
		return nil, nil, err
	}
	return norm, map[string]string{"source": sp.Source.Bucket, "target": sp.Target.Bucket}, nil
}

func clusterCopyRangeEmpty(ctx context.Context, backend storage.Backend, bucket, prefix string) (bool, error) {
	empty := true
	err := backend.WalkVersions(ctx, bucket, prefix, "", "", func(storage.ObjectInfo) error {
		empty = false
		return storage.ErrStopWalk
	})
	if err != nil || !empty {
		return empty, err
	}
	err = storage.WalkMultipartUploads(ctx, backend, bucket, prefix, storage.UploadPosition{}, func(storage.UploadInfo) error {
		empty = false
		return storage.ErrStopWalk
	})
	return empty, err
}

// DryRun implements the admin copy validator for cluster mode.
func (r *CopyRunner) DryRun(ctx context.Context, raw json.RawMessage) (json.RawMessage, []string, error) {
	norm, _, err := r.Validate(ctx, raw)
	return norm, []string{}, err
}

func (r *CopyRunner) Run(ctx context.Context, job jobs.Job) error {
	return r.RunWithProgress(ctx, job, nil)
}

type copyCheckpoint struct {
	Pass    int    `json:"pass"`
	LastKey string `json:"lastKey,omitempty"`
}

// RunWithProgress resumes after the last fully synchronized key. A failed
// key is repeated, which is safe because version imports are idempotent.
func (r *CopyRunner) RunWithProgress(ctx context.Context, job jobs.Job, update func(context.Context, string, json.RawMessage, json.RawMessage) error) error {
	var sp copySpec
	if err := json.Unmarshal(job.Spec, &sp); err != nil || sp.Kind != copyjob.KindCopy {
		return jobs.Permanent(fmt.Errorf("invalid cluster copy job specification: %w", errors.Join(err, jobs.ErrInvalidSpec)))
	}
	if r.IsLeader == nil || !r.IsLeader() {
		return errors.New("cluster copy leader changed")
	}
	source, err := r.Backend.GetBucket(ctx, sp.Source.Bucket)
	if err != nil {
		return err
	}
	if source.InstanceID != sp.SourceInstanceID {
		return jobs.Permanent(errors.New("copy source or target bucket was replaced"))
	}
	var target storage.BucketInfo
	if !sp.TargetExisted && sp.Target.Create != nil {
		creator, ok := r.Backend.(jobBucketCreator)
		if !ok {
			return jobs.Permanent(errors.New("cluster copy target creation is unavailable"))
		}
		lock := sp.Target.Create.ObjectLock != nil && *sp.Target.Create.ObjectLock
		target, err = creator.CreateBucketForJob(ctx, job.ID, job.CreatedAt, sp.Target.Bucket,
			storage.CreateBucketOptions{Owner: job.CreatedBy, Region: source.Region, ObjectLockEnabled: lock, Import: true})
		if errors.Is(err, storage.ErrBucketExists) {
			return jobs.Permanent(fmt.Errorf("%w: the target bucket appeared after submission", jobs.ErrConflict))
		}
		if err != nil {
			return err
		}
		if sp.Options.Versions == copyjob.VersionsAll {
			if err := r.Backend.SetBucketVersioning(ctx, sp.Target.Bucket, storage.VersioningEnabled); err != nil {
				return err
			}
		}
		current, err := r.Backend.GetBucket(ctx, sp.Target.Bucket)
		if err != nil {
			return err
		}
		if current.InstanceID != target.InstanceID {
			return jobs.Permanent(errors.New("copy target bucket was replaced"))
		}
	} else {
		target, err = r.Backend.GetBucket(ctx, sp.Target.Bucket)
		if err != nil {
			return err
		}
		if target.InstanceID != sp.TargetInstanceID {
			return jobs.Permanent(errors.New("copy target bucket was replaced"))
		}
	}
	src := copier.Endpoint{Backend: r.Backend, Bucket: sp.Source.Bucket, Prefix: sp.Source.Prefix}
	dst := copier.Endpoint{Backend: r.Backend, Bucket: sp.Target.Bucket, Prefix: sp.Target.Prefix}
	if sp.Options.OnExisting == copyjob.OnExistingFail {
		if len(job.Checkpoint) == 0 {
			empty, err := clusterCopyRangeEmpty(ctx, r.Backend, sp.Target.Bucket, sp.Target.Prefix)
			if err != nil {
				return err
			}
			if !empty {
				return jobs.Permanent(fmt.Errorf("%w: target range changed after copy job submission", jobs.ErrConflict))
			}
		} else if err := ensureCopyTargetOwned(ctx, src, dst, sp.Options.Versions == copyjob.VersionsAll); err != nil {
			return jobs.Permanent(err)
		}
	}
	policy := copier.Policy{Versions: copier.Versions(sp.Options.Versions), Merge: sp.Options.OnExisting == copyjob.OnExistingMerge,
		Encryption: copier.EncryptionPreserve, Compression: copier.CompressionPreserve,
		AllowPlaintext: sp.Options.AllowPlaintext, DropRetention: sp.Options.DropRetention}
	passes := 1
	if sp.Options.FinalPass == nil || *sp.Options.FinalPass {
		passes++
	}
	var checkpoint copyCheckpoint
	if len(job.Checkpoint) != 0 && json.Unmarshal(job.Checkpoint, &checkpoint) != nil {
		return jobs.Permanent(errors.New("invalid cluster copy checkpoint"))
	}
	if checkpoint.Pass < 0 || checkpoint.Pass > passes {
		return jobs.Permanent(errors.New("invalid cluster copy pass checkpoint"))
	}
	var status copyjob.Status
	if len(job.Status) != 0 && json.Unmarshal(job.Status, &status) != nil {
		return jobs.Permanent(errors.New("invalid cluster copy status"))
	}
	status.Mode = "identity"
	warnings, err := syncClusterConfigs(ctx, r.Backend, sp.Spec, target.ObjectLockEnabled)
	if err != nil {
		return err
	}
	status.Warnings = warnings
	save := func(phase string) error {
		if update == nil {
			return nil
		}
		statusDoc, err := json.Marshal(status)
		if err != nil {
			return err
		}
		checkpointDoc, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		return update(ctx, phase, statusDoc, checkpointDoc)
	}
	for pass := checkpoint.Pass; pass < passes; pass++ {
		phase := "copying"
		if pass > 0 {
			phase = "verifying"
			if checkpoint.LastKey == "" {
				status.KeysScanned, status.BytesDone = 0, 0
			}
		}
		if err := save(phase); err != nil {
			return err
		}
		err = copier.ZipWalk(ctx, src, dst, checkpoint.LastKey, sp.Options.Versions == copyjob.VersionsAll, func(batch []copier.KeyPair) error {
			for _, pair := range batch {
				if !r.IsLeader() {
					return errors.New("cluster copy leader changed")
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				result, err := copier.SyncKey(ctx, src, dst, pair, policy)
				if err != nil {
					return err
				}
				if len(result.Conflicts) != 0 {
					return fmt.Errorf("copy conflict on %s: %s", pair.SrcKey, result.Conflicts[0].Reason)
				}
				if result.Deferred {
					return fmt.Errorf("copy of %s deferred after concurrent changes", pair.SrcKey)
				}
				status.KeysScanned++
				if result.Changed() {
					status.KeysCopied++
				}
				status.VersionsCopied += result.VersionsCopied
				status.BytesCopied += result.BytesCopied
				for _, version := range pair.Src {
					status.BytesDone += version.Size
				}
				checkpoint.LastKey = pair.SrcKey
			}
			return save(phase)
		})
		if err != nil {
			return err
		}
		checkpoint.Pass++
		checkpoint.LastKey = ""
		status.Passes++
		status.LastPassAt = time.Now().UTC()
		if err := save(phase); err != nil {
			return err
		}
	}
	return nil
}

// ensureCopyTargetOwned refuses target versions that cannot be explained by
// the source. A retry may see versions this job already imported, while an
// unrelated writer's new version must not be silently removed or replaced.
func ensureCopyTargetOwned(ctx context.Context, src, dst copier.Endpoint, all bool) error {
	return copier.ZipWalk(ctx, src, dst, "", all, func(batch []copier.KeyPair) error {
		for _, pair := range batch {
			for _, target := range pair.Dst {
				owned := false
				for _, source := range pair.Src {
					if source.VersionID == target.VersionID && source.DeleteMarker == target.DeleteMarker ||
						!all && target.VersionID == "null" && !source.DeleteMarker && source.ETag == target.ETag && source.Size == target.Size {
						owned = true
						break
					}
				}
				if !owned {
					return fmt.Errorf("%w: target key %q has a version the copy job did not write", jobs.ErrConflict, pair.DstKey)
				}
			}
		}
		return nil
	})
}

var _ Runner = (*CopyRunner)(nil)
var _ ProgressRunner = (*CopyRunner)(nil)
