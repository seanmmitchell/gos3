package s3store

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// Orphan recovery.
//
// Blobs can be left unreferenced: a write that failed after its upload, a
// journal record that never became durable before a fail-stop, a delete
// that failed. The recovery pass lists data/, parts/ and the staged blobs
// of the known aux namespaces (gateway/parts/<ns>/) and deletes a blob only
// when all of these hold:
//
//   - its name is exactly one this package generates (recognizedBlob;
//     staged blobs: a namespace the loaded state holds or AuxState handed
//     out, and a name in the generated-name charset);
//   - it is not pending (a write in flight), not being removed by a record
//     that is not durable yet and not held by an open reader;
//   - the loaded state does not reference it;
//   - its LastModified is older than Options.OrphanRetention;
//   - a HEAD shows it tagged with this backing's ID (blobTagHeader).
//
// A backing may share its remote bucket with other data, other backings or
// blobs of builds that did not tag them yet, so untagged objects and
// objects tagged by another backing are never deleted, whatever their name
// and age; they are counted and logged at debug level, and remembered (key,
// LastModified and size) so later passes skip them without a HEAD while
// they are unchanged. A pass sends at most maxRecoveryHeads HEADs; later
// passes check the rest.

// recoveryStats counts what a recovery pass did.
type recoveryStats struct {
	deleted, young, ignored, errors int
	// untagged and foreign count old unreferenced candidates kept because
	// they carry no backing tag, or another backing's (found now or
	// remembered from an earlier pass).
	untagged, foreign int
	// deferred counts candidates left to a later pass (HEAD limit).
	deferred int
}

// maxRecoveryHeads is the most HEAD requests one recovery pass sends.
const maxRecoveryHeads = 1000

// keptObject is an object the recovery keeps: untagged, or (foreign)
// tagged by another backing, as of its listed LastModified and size.
type keptObject struct {
	lastModified time.Time
	size         int64
	foreign      bool
}

// Lengths (in hex digits) of the generated IDs in blob names.
const (
	bucketIDLen = 32 // randomID(16), CreateBucket
	dataIDLen   = 32 // randomID(16), newDataWriter and assemble
	uploadIDLen = 48 // index.NewUploadID
)

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
				"backing", b.opts.Name, "deleted", st.deleted, "kept_young", st.young,
				"kept_untagged", st.untagged, "kept_foreign", st.foreign, "deferred", st.deferred, "errors", st.errors)
		default:
			b.log.Debug("s3store: recovery pass finished", "backing", b.opts.Name, "kept_young", st.young,
				"kept_untagged", st.untagged, "kept_foreign", st.foreign, "deferred", st.deferred, "errors", st.errors)
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

// auxNamespaces returns the aux namespaces whose staged blobs the recovery
// considers: those the state holds and those AuxState handed out.
func (b *Backend) auxNamespaces(st *state) []string {
	set := map[string]bool{}
	st.auxMu.Lock()
	for ns := range st.aux {
		set[ns] = true
	}
	st.auxMu.Unlock()
	b.auxNames.Range(func(k, _ any) bool {
		set[k.(string)] = true
		return true
	})
	return slices.Sorted(maps.Keys(set))
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
	id := b.eng.Marker().BackingID
	if id == "" {
		return stats, b.unavailableErr()
	}
	refs := b.referenced(st)
	cutoff := b.now().Add(-b.opts.OrphanRetention)

	// The areas listed, each with the recognizer of its blob names.
	type area struct {
		prefix     string
		recognized func(rest string) bool
	}
	areas := []area{
		{b.keys.prefix + blobDataDir, func(rest string) bool { return recognizedBlob(blobDataDir, rest) }},
		{b.keys.prefix + blobPartsDir, func(rest string) bool { return recognizedBlob(blobPartsDir, rest) }},
	}
	for _, ns := range b.auxNamespaces(st) {
		if validAuxNS(ns) == nil {
			areas = append(areas, area{b.stagedKey(ns, ""), reclaimableStagedName})
		}
	}
	b.recoveryMu.Lock()
	kept := b.recoveryKept // replaced, never modified
	b.recoveryMu.Unlock()
	nextKept := map[string]keptObject{}
	var untaggedKey, foreignKey string
	var candidates []s3client.ObjectAttrs
	for _, a := range areas {
		err := b.client().Walk(ctx, b.opts.Bucket, a.prefix, "", func(o s3client.ObjectAttrs) error {
			if !a.recognized(strings.TrimPrefix(o.Key, a.prefix)) {
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
			if k, ok := kept[o.Key]; ok && k.lastModified.Equal(o.LastModified) && k.size == o.Size {
				// Found untagged or foreign before, and unchanged.
				nextKept[o.Key] = k
				if k.foreign {
					stats.foreign++
					foreignKey = o.Key
				} else {
					stats.untagged++
					untaggedKey = o.Key
				}
				return nil
			}
			candidates = append(candidates, o)
			return nil
		})
		if err != nil {
			return stats, b.mapErr(ctx, "list "+a.prefix, err)
		}
	}

	// Only blobs tagged with this backing's ID are deleted.
	if limit := b.recoveryHeads; len(candidates) > limit {
		stats.deferred = len(candidates) - limit
		candidates = candidates[:limit]
		b.log.Info("s3store: recovery reached its limit of HEAD requests per pass; later passes check the rest",
			"backing", b.opts.Name, "location", b.Location(), "limit", limit, "deferred", stats.deferred)
	}
	var victims []string
	for _, o := range candidates {
		k := o.Key
		attrs, err := b.client().HeadObject(ctx, b.opts.Bucket, k)
		switch {
		case err == nil:
		case s3client.IsNotFound(err):
			continue // deleted meanwhile
		case ctx.Err() != nil || s3client.IsUnavailable(err):
			return stats, b.mapErr(ctx, "head "+k, err)
		default:
			stats.errors++
			continue
		}
		switch tag := blobTag(attrs); tag {
		case id:
			victims = append(victims, k)
		case "":
			stats.untagged++
			untaggedKey = k
			nextKept[k] = keptObject{lastModified: o.LastModified, size: o.Size}
		default:
			stats.foreign++
			foreignKey = k
			nextKept[k] = keptObject{lastModified: o.LastModified, size: o.Size, foreign: true}
		}
	}
	// Keys no longer listed (or changed, or now referenced) are forgotten.
	b.recoveryMu.Lock()
	b.recoveryKept = nextKept
	b.recoveryMu.Unlock()
	if stats.untagged+stats.foreign > 0 {
		b.log.Debug("s3store: recovery keeps old unreferenced objects not tagged by this backing",
			"backing", b.opts.Name, "location", b.Location(), "untagged", stats.untagged, "example_untagged", untaggedKey,
			"foreign", stats.foreign, "example_foreign", foreignKey)
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

// recognizedBlob reports whether rest (a key below area, data/ or parts/)
// is exactly a name this package generates: data/<bucket-id>/<data-id> or
// parts/<upload-id>/<nnnnn>-<data-id>.
func recognizedBlob(area, rest string) bool {
	first, second, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	switch area {
	case blobDataDir:
		return len(first) == bucketIDLen && isLowerHex(first) && len(second) == dataIDLen && isLowerHex(second)
	case blobPartsDir:
		_, id, ok := parsePartName(second)
		return ok && len(first) == uploadIDLen && isLowerHex(first) && len(id) == dataIDLen
	}
	return false
}
