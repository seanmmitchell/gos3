package s3store

import (
	"context"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// Orphan recovery.
//
// Blobs can be left unreferenced: a write that failed after its upload, a
// journal record that never became durable before a fail-stop, a delete
// that failed. The recovery pass lists data/, parts/ and gateway/parts/ and
// deletes a blob when its name is one this package generates, it is not
// pending (a write in flight) or held by an open reader, the loaded state
// does not reference it, and its LastModified is older than
// Options.OrphanRetention.

// recoveryStats counts what a recovery pass did.
type recoveryStats struct {
	deleted, young, ignored, errors int
}

// runRecovery runs the recovery pass periodically once the backing is
// loaded.
func (b *Backend) runRecovery() {
	defer b.wg.Done()
	if b.opts.RecoveryInterval < 0 {
		return
	}
	select {
	case <-b.ctx.Done():
		return
	case <-b.ready:
	}
	first := min(b.opts.RecoveryInterval, 5*time.Minute)
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
		st, err := b.recoverOrphans(b.ctx)
		switch {
		case err != nil:
			b.log.Debug("s3store: recovery pass skipped", "backing", b.opts.Name, "err", err)
		case st.deleted > 0:
			b.log.Warn("s3store: recovery deleted unreferenced blobs older than the retention period",
				"backing", b.opts.Name, "deleted", st.deleted, "kept_young", st.young, "errors", st.errors)
		default:
			b.log.Debug("s3store: recovery pass finished", "backing", b.opts.Name, "kept_young", st.young, "errors", st.errors)
		}
		t.Reset(b.opts.RecoveryInterval)
	}
}

// referenced returns the blob keys the state references.
func (b *Backend) referenced(st *state) map[string]bool {
	refs := map[string]bool{}
	st.mu.RLock()
	bks := make([]*bucket, 0, len(st.buckets))
	for _, bk := range st.buckets {
		bks = append(bks, bk)
	}
	st.mu.RUnlock()
	for _, bk := range bks {
		bk.mu.RLock()
		for _, o := range bk.ix.Objects() {
			for _, v := range o.Versions() {
				if v.Data.ID != "" {
					refs[b.dataKey(bk.id, v.Data.ID)] = true
				}
			}
		}
		for _, u := range bk.ix.Uploads() {
			for _, k := range b.partBlobs(u) {
				refs[k] = true
			}
		}
		bk.mu.RUnlock()
	}
	st.auxMu.Lock()
	for ns, a := range st.aux {
		for name := range a.staged {
			refs[b.stagedKey(ns, name)] = true
		}
	}
	st.auxMu.Unlock()
	return refs
}

// recoverOrphans runs one recovery pass.
func (b *Backend) recoverOrphans(ctx context.Context) (recoveryStats, error) {
	var stats recoveryStats
	st, err := b.current(ctx)
	if err != nil {
		return stats, err
	}
	if b.eng.State().State != StateServing {
		return stats, b.unavailableErr()
	}
	refs := b.referenced(st)
	cutoff := b.now().Add(-b.opts.OrphanRetention)
	var victims []string
	for _, area := range []string{blobDataDir, blobPartsDir, blobStagedDir} {
		prefix := b.keys.prefix + area
		err := b.client().Walk(ctx, b.opts.Bucket, prefix, "", func(o s3client.ObjectAttrs) error {
			rest := strings.TrimPrefix(o.Key, prefix)
			if !recognizedBlob(area, rest) {
				stats.ignored++
				return nil
			}
			if refs[o.Key] || b.pending.has(o.Key) || b.removing.has(o.Key) || b.blobs.held(o.Key) {
				return nil
			}
			if o.LastModified.IsZero() || !o.LastModified.Before(cutoff) {
				stats.young++
				return nil
			}
			victims = append(victims, o.Key)
			return nil
		})
		if err != nil {
			return stats, b.mapErr(ctx, "list "+prefix, err)
		}
	}
	// The pass is void if the state was reloaded meanwhile, or the
	// backing stopped serving (failed or fenced).
	b.mu.RLock()
	same := b.st == st
	b.mu.RUnlock()
	if !same || b.eng.State().State != StateServing {
		return stats, b.unavailableErr()
	}
	var del []string
	for _, k := range victims {
		if !b.pending.has(k) && !b.removing.has(k) && b.blobs.deleteOrDefer(k) {
			del = append(del, k)
		}
	}
	if len(del) == 0 {
		return stats, nil
	}
	if err := b.client().DeleteObjects(ctx, b.opts.Bucket, del); err != nil {
		stats.errors++
		return stats, b.mapErr(ctx, "delete orphans", err)
	}
	stats.deleted = len(del)
	return stats, nil
}

// recognizedBlob reports whether rest (a key below area) has a name this
// package generates.
func recognizedBlob(area, rest string) bool {
	first, second, ok := strings.Cut(rest, "/")
	if !ok || first == "" || second == "" {
		return false
	}
	switch area {
	case blobDataDir:
		return validHexID(first) && validHexID(second)
	case blobPartsDir:
		_, _, ok := parsePartName(second)
		return validHexID(first) && ok
	default:
		return true
	}
}
