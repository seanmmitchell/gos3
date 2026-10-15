package s3account

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// Migration of legacy homes
//
// A plain s3 backing of a build before s3 account backings is an s3store
// whose buckets have no records: its home is reconciled like any
// container, which records every bucket as a wrapped bucket of the home
// (reconcile.go).
//
// A legacy gateway backing kept its buckets' records and configuration
// documents in an s3store marked as a gateway (Marker.Gateway), and their
// objects natively in remote buckets named like the gos3 buckets. Its
// home is migrated bucket by bucket (migrateGatewayBucket):
//
//  1. the configuration documents are copied to the records (c/);
//  2. the bucket is recorded as a native bucket bound to the remote bucket
//     of its name, as adopted (deleting it never deletes the remote
//     bucket);
//  3. the bucket is deleted from the home's store.
//
// When the home holds no bucket any more, the marker's gateway flag is
// cleared (s3store.Backend.ClearLegacyGateway) and the home is an ordinary
// home. Each step is idempotent and the bucket record is the commit point,
// so a migration interrupted anywhere is completed by the next load. The
// gateway's multipart state is in the aux namespace the native store uses
// ("gateway"). A migrated home cannot be opened by an older build.

// migrateGateway migrates a legacy gateway home (adminMu held).
func (b *Backend) migrateGateway(ctx context.Context, c *container) error {
	list, err := c.store.ListBuckets(ctx)
	if err != nil {
		return err
	}
	if len(list) > 0 {
		b.log.Info("s3account: migrating a legacy gateway backing: its buckets become native buckets", "backing", b.name,
			"buckets", len(list))
	}
	complete := true
	for _, info := range list {
		ok, err := b.migrateGatewayBucket(ctx, c, info)
		if err != nil {
			return err
		}
		complete = complete && ok
	}
	if !complete {
		// Retried at the next load; the gateway flag stays.
		return nil
	}
	if err := b.step("migrate:clear-gateway"); err != nil {
		return err
	}
	if err := c.store.ClearLegacyGateway(ctx); err != nil {
		return err
	}
	b.log.Info("s3account: legacy gateway backing migrated", "backing", b.name)
	return nil
}

// migrateGatewayBucket migrates one bucket of a legacy gateway home. ok is
// false when the bucket had to be left in the home (its name is taken).
func (b *Backend) migrateGatewayBucket(ctx context.Context, c *container, info storage.BucketInfo) (ok bool, err error) {
	name := info.Name
	b.mu.RLock()
	e := b.reg.buckets[name]
	var rec bucketRecord
	if e != nil {
		rec = e.bucketRecord
	}
	b.mu.RUnlock()
	switch {
	case e == nil:
		if err := validBucketName(name); err != nil {
			b.log.Error("s3account: a legacy gateway bucket's name cannot be recorded; it stays unmigrated",
				"backing", b.name, "bucket", name)
			return false, nil
		}
		// Records of an earlier bucket of the name (or of an interrupted
		// run of this migration) are replaced, never merged.
		if err := b.purgeConfigs(ctx, name); err != nil {
			return false, err
		}
		docs := map[string][]byte{}
		for _, key := range storage.AllConfigKeys {
			data, err := c.store.GetBucketConfig(ctx, name, key)
			if errors.Is(err, storage.ErrConfigNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			if err := b.step("migrate:config"); err != nil {
				return false, err
			}
			if err := b.records.PutRecord(ctx, configKey(name, key), data); err != nil {
				return false, err
			}
			docs[key] = bytes.Clone(data)
		}
		if err := b.step("migrate:record"); err != nil {
			return false, err
		}
		rec := bucketRecord{ID: newIncarnation(), Kind: storage.BucketKindNative, Remote: name, Created: false, Info: info, Added: time.Now().UTC()}
		release, cerr := b.claim(Location{Bucket: name}, RoleNative, name)
		if err := b.putBucket(ctx, name, rec, release); err != nil {
			if release != nil {
				release()
			}
			return false, err
		}
		b.mu.Lock()
		if len(docs) > 0 {
			b.reg.configs[name] = docs
		}
		if cerr != nil {
			b.reg.buckets[name].claimErr = cerr
		}
		b.mu.Unlock()
		if cerr != nil {
			b.log.Error("s3account: a migrated gateway bucket's remote bucket is used elsewhere; the bucket is unavailable",
				"backing", b.name, "bucket", name, "err", cerr)
		} else {
			// The remote bucket's versioning is read in the background.
			b.onDrift(name, "migrated from a gateway backing")
		}
	case rec.Kind == storage.BucketKindNative && strings.EqualFold(rec.Remote, name):
		// Recorded by an interrupted migration: finish it.
	default:
		b.log.Error("s3account: a legacy gateway bucket is named like another bucket of the backing; it stays unmigrated",
			"backing", b.name, "bucket", name, "kind", rec.Kind)
		return false, nil
	}
	if err := b.step("migrate:delete"); err != nil {
		return false, err
	}
	err = c.store.DeleteBucket(ctx, name)
	switch {
	case err == nil, errors.Is(err, storage.ErrBucketNotFound):
		b.log.Info("s3account: migrated a gateway bucket to a native bucket", "backing", b.name, "bucket", name)
		return true, nil
	case errors.Is(err, storage.ErrBucketNotEmpty):
		b.log.Error("s3account: a legacy gateway bucket holds objects in the home; it stays in the home unmigrated",
			"backing", b.name, "bucket", name)
		return false, nil
	}
	return false, err
}
