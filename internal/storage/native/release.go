package native

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// DiscardBucketLocalState removes a detached bucket's local uploads and
// overlays without contacting its remote bucket. The account must retain
// its deletion record until this succeeds, so the name cannot be rebound
// while an upload or overlay record could still be loaded at Open.
func (s *Store) DiscardBucketLocalState(ctx context.Context, bucket string) error {
	ctx = context.WithoutCancel(ctx)
	recs, err := s.state.Records(ctx)
	if err != nil {
		return fmt.Errorf("native %q: read state before releasing bucket %q: %w", s.name, bucket, err)
	}
	ids := map[string]bool{}
	s.mu.Lock()
	s.gens[bucket]++ // invalidate object writers opened before the release
	for _, u := range s.byBucket[bucket] {
		ids[u.id] = true
	}
	s.mu.Unlock()
	for key, data := range recs {
		if !strings.HasPrefix(key, recUpload) {
			continue
		}
		var r uploadRecord
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("native %q: read upload record %q before releasing bucket %q: %w", s.name, key, bucket, err)
		}
		if r.Bucket == bucket {
			ids[strings.TrimPrefix(key, recUpload)] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		if err := s.discardLocalUpload(ctx, bucket, id, recs); err != nil {
			return err
		}
	}
	if err := s.dropBucketOverlays(ctx, bucket); err != nil {
		return err
	}
	return nil
}

// discardLocalUpload deletes the authoritative upload record first. Part
// records and staged blobs left after that point are unreachable and Open
// reclaims them if their best-effort removal here fails.
func (s *Store) discardLocalUpload(ctx context.Context, bucket, id string, recs map[string][]byte) error {
	s.mu.Lock()
	u := s.uploads[id]
	if u != nil && u.bucket != bucket {
		u = nil
	}
	s.mu.Unlock()
	if u != nil {
		u.op.Lock()
		defer u.op.Unlock()
	}
	if err := s.state.DeleteRecords(ctx, uploadKey(id)); err != nil {
		return fmt.Errorf("native %q: delete upload %q before releasing bucket %q: %w", s.name, id, bucket, err)
	}
	files := map[string]bool{}
	for key, data := range recs {
		if !strings.HasPrefix(key, partsPrefix(id)) {
			continue
		}
		var p partRecord
		if err := json.Unmarshal(data, &p); err == nil && p.File != "" {
			files[p.File] = true
		}
	}
	if u != nil {
		s.mu.Lock()
		u.gone = true
		s.removeUploadLocked(u)
		for _, p := range u.parts {
			if p.file != "" {
				files[p.file] = true
			}
		}
		s.mu.Unlock()
	}
	if err := s.state.DeleteRecords(ctx, partsPrefix(id)); err != nil {
		s.log.Warn("native: delete orphaned part records after releasing a bucket", "backing", s.name,
			"bucket", bucket, "upload", id, "err", err)
	}
	for file := range files {
		s.deleteStaged(ctx, file)
	}
	return nil
}
