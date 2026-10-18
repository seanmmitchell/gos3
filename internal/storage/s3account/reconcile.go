package s3account

import (
	"context"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Reconciliation
//
// A bucket's record is written after (creation) or deleted after
// (deletion) the change in its container, so a crash in between leaves a
// container and the records disagreeing. Each container is reconciled
// with the records when it has loaded (the home before the backing is
// ready, see Backend.load; the others when their store is ready):
//
//  1. a bucket of the container without a record is adopted as a wrapped
//     bucket of the container (this is also how a plain home of a build
//     before s3 account backings, whose buckets have no records, migrates);
//  2. a record of a wrapped bucket of the container that the container
//     does not hold is dropped, with a warning.
//
// A legacy gateway home is migrated instead (migrate.go): its buckets are
// records of remote buckets, never wrapped buckets. Every step is
// idempotent, so an interrupted reconciliation is completed by the next.

// step runs the test hook before a step (nil in production).
func (b *Backend) step(name string) error {
	if b.hooks.step != nil {
		return b.hooks.step(name)
	}
	return nil
}

// reconcileContainer reconciles container c with the records (adminMu
// held).
func (b *Backend) reconcileContainer(ctx context.Context, c *container) error {
	if c == nil || c.store == nil {
		return nil
	}
	if c.store.Marker().Gateway {
		if c.home {
			return b.migrateGateway(ctx, c)
		}
		b.log.Error("s3account: a container is the metadata prefix of a legacy gateway backing; its buckets are not adopted",
			"backing", b.name, "container", c.id, "location", c.locationString())
		return nil
	}
	list, err := c.store.ListBuckets(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool, len(list))
	for _, info := range list {
		have[info.Name] = true
		b.mu.RLock()
		e := b.reg.buckets[info.Name]
		var rec bucketRecord
		if e != nil {
			rec = e.bucketRecord
		}
		b.mu.RUnlock()
		switch {
		case e == nil:
			if err := validBucketName(info.Name); err != nil {
				b.log.Warn("s3account: a container holds a bucket whose name cannot be recorded; it is not served",
					"backing", b.name, "container", c.id, "bucket", info.Name)
				continue
			}
			if err := b.step("reconcile:adopt"); err != nil {
				return err
			}
			rec := bucketRecord{ID: newIncarnation(), Kind: storage.BucketKindWrapped, Container: c.id, Info: info, Added: time.Now().UTC()}
			if err := b.putBucket(ctx, info.Name, rec, nil); err != nil {
				return err
			}
			b.log.Info("s3account: recorded a wrapped bucket found in a container", "backing", b.name,
				"bucket", info.Name, "container", c.id)
		case rec.Kind == storage.BucketKindWrapped && rec.Container == c.id:
			if si := storedInfo(info); rec.Info != si {
				rec.Info = si
				if err := b.putBucket(ctx, info.Name, rec, nil); err != nil {
					return err
				}
			}
		default:
			b.log.Warn("s3account: a container holds a bucket named like another bucket of the backing; it is not served",
				"backing", b.name, "container", c.id, "bucket", info.Name, "kind", rec.Kind, "other_container", rec.Container)
		}
	}
	b.mu.RLock()
	var gone []string
	for name, e := range b.reg.buckets {
		if e.Kind == storage.BucketKindWrapped && e.Container == c.id && !have[name] {
			gone = append(gone, name)
		}
	}
	b.mu.RUnlock()
	for _, name := range gone {
		if err := b.step("reconcile:drop"); err != nil {
			return err
		}
		b.log.Warn("s3account: a wrapped bucket is missing from its container; its record is removed",
			"backing", b.name, "bucket", name, "container", c.id, "location", c.locationString())
		if err := b.dropBucket(ctx, name); err != nil {
			return err
		}
	}
	return nil
}
