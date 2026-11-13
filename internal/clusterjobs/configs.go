package clusterjobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/copyjob"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// clusterConfig reads a raw bucket configuration, treating absence as nil.
func clusterConfig(ctx context.Context, backend storage.Backend, bucket, key string) ([]byte, error) {
	data, err := backend.GetBucketConfig(ctx, bucket, key)
	if errors.Is(err, storage.ErrConfigNotFound) {
		return nil, nil
	}
	return data, err
}

// validateClusterConfigs prevents copying settings that would claim a
// storage capability the cluster backend cannot currently provide. It runs
// before submission and again before the first configuration write.
func validateClusterConfigs(ctx context.Context, backend storage.Backend, sp copyjob.Spec) error {
	for _, key := range []string{storage.ConfigEncryption, storage.ConfigEncryptionProvider} {
		if !copyjob.SelectedConfig(sp, key) {
			continue
		}
		data, err := clusterConfig(ctx, backend, sp.Source.Bucket, key)
		if err != nil {
			return err
		}
		if data != nil {
			return fmt.Errorf("%w: cluster copy cannot copy the source's %s configuration", jobs.ErrInvalidSpec, key)
		}
	}
	return nil
}

// syncClusterConfigs copies the standalone job's selected documents and
// mandatory ACL guards. A warning records a document that cannot safely be
// mapped to the destination. Every PutBucketConfig is a Raft-acknowledged
// mutation; repeating this pass after a crash is idempotent.
func syncClusterConfigs(ctx context.Context, backend storage.Backend, sp copyjob.Spec, destinationLock bool) ([]string, error) {
	if err := validateClusterConfigs(ctx, backend, sp); err != nil {
		return nil, jobs.Permanent(err)
	}
	var warnings []string
	for _, key := range copyjob.ConfigKeys(sp) {
		data, err := clusterConfig(ctx, backend, sp.Source.Bucket, key)
		if err != nil {
			return warnings, fmt.Errorf("reading the source's %s configuration: %w", key, err)
		}
		current, err := clusterConfig(ctx, backend, sp.Target.Bucket, key)
		if err != nil {
			return warnings, fmt.Errorf("reading the target's %s configuration: %w", key, err)
		}
		guard := storage.ACLGuard(key, data)
		if !copyjob.SelectedConfig(sp, key) && (!guard || storage.ACLGuard(key, current)) {
			continue
		}
		if data == nil {
			continue // one-shot copies do not remove an existing target setting
		}
		switch key {
		case storage.ConfigPolicy:
			mapped, reason := copyjob.RewritePolicy(data, sp)
			if reason != "" {
				warnings = append(warnings, "the bucket policy is not copied: "+reason)
				continue
			}
			data = mapped
		case storage.ConfigObjectLock:
			if !destinationLock {
				warnings = append(warnings, "the object lock configuration is not copied: the target has no object lock")
				continue
			}
		case storage.ConfigCompression:
			// The cluster backend has no CompressingBackend interface. A
			// copied preference would claim compression for future writes.
			warnings = append(warnings, "the compression configuration is not copied: cluster storage cannot apply it")
			continue
		}
		if bytes.Equal(current, data) {
			continue
		}
		if err := backend.PutBucketConfig(ctx, sp.Target.Bucket, key, data); err != nil {
			if errors.Is(err, storage.ErrNotSupported) || errors.Is(err, errors.ErrUnsupported) {
				if guard {
					return warnings, jobs.Permanent(fmt.Errorf("target cannot store the source's %s ACL guard: %w", key, err))
				}
				warnings = append(warnings, fmt.Sprintf("the %s configuration is not copied: the target does not support it", key))
				continue
			}
			return warnings, fmt.Errorf("storing the target's %s configuration: %w", key, err)
		}
	}
	return warnings, nil
}
