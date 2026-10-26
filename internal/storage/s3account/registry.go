package s3account

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// The registry
//
// The backing's records live in the aux namespace "account" of the home
// (s3store.AuxState) or of the local state database (localstate), as
// key/value records:
//
//	v                           {"schema":1}
//	k/<container ID>/           containerRecord (a container other than the home)
//	b/<gos3 bucket>/            bucketRecord (every bucket, native and wrapped)
//	c/<gos3 bucket>/<config>/   a native bucket's configuration document (raw)
//	d/default-container         {"id":"<container ID>"} (absent: the home)
//
// A record x/meta-key/ (a metadata key of development builds, no longer
// used) is ignored.
//
// Keys of records that can be deleted end with "/": the record stores
// delete by key prefix, and "/" never occurs in bucket names, container
// IDs or configuration keys, so deleting one record never deletes another
// whose key merely starts with the same characters (the bucket "photos"
// and "photos2", the configurations "encryption" and
// "encryption-provider").
//
// The records are read once, when the backing loads, into the in-memory
// registry; every change is written through: the record is durable before
// the registry changes. Changes are serialized by Backend.adminMu.

// registrySchema is the format of the records written by this build.
// Records of a newer format are refused.
const registrySchema = 1

// accountNamespace and nativeNamespace are the aux namespaces of the
// registry and of the native store's state. "gateway" is the namespace
// legacy gateway backings kept their multipart state in, so their uploads
// survive the migration.
const (
	accountNamespace = "account"
	nativeNamespace  = "gateway"
)

// homeID is the ID of the home's implicit container.
const homeID = "home"

// Container roles (ContainerInfo.Role, containerRecord.Role).
const (
	containerRoleHome      = "home"
	containerRoleDedicated = "dedicated" // a remote bucket created for the container
	containerRoleExisting  = "existing"  // a prefix of an existing remote bucket
)

// Record keys.
const (
	keySchema           = "v"
	keyDefaultContainer = "d/default-container"
	keyLegacyMetaKey    = "x/meta-key/" // ignored (development builds)
	prefixContainers    = "k/"
	prefixBuckets       = "b/"
	prefixConfigs       = "c/"
)

func containerKey(id string) string       { return prefixContainers + id + "/" }
func bucketKey(name string) string        { return prefixBuckets + name + "/" }
func configPrefix(bucket string) string   { return prefixConfigs + bucket + "/" }
func configKey(bucket, key string) string { return configPrefix(bucket) + key + "/" }
func trimRecordKey(k, prefix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(k, prefix), "/")
}

// validContainerID reports whether id is a container ID this package
// generates: the home's, or "c" and 8 lowercase hex digits
// (newContainerID). The records of a home are not authenticated, and a
// container's ID names its temporary directory (storeOptions), so every
// other ID is refused.
func validContainerID(id string) bool {
	return id == homeID || len(id) == 9 && id[0] == 'c' && isLowerHex(id[1:])
}

// isLowerHex reports whether s is non-empty and only lowercase hex digits.
func isLowerHex(s string) bool { return s != "" && strings.Trim(s, "0123456789abcdef") == "" }

// schemaRecord is the record v.
type schemaRecord struct {
	Schema int `json:"schema"`
}

// containerRecord is the record k/<id>/ of a container other than the
// home.
type containerRecord struct {
	ID     string `json:"id"`
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
	Region string `json:"region,omitempty"`
	// Role is containerRoleDedicated or containerRoleExisting.
	Role string `json:"role"`
	// CreatedBucket is set when gos3 created the remote bucket.
	CreatedBucket bool `json:"createdBucket,omitempty"`
	// BackingID is the ID in the container's gos3.json: a registered
	// container is only ever opened with this ID (s3store ExpectBackingID).
	BackingID string    `json:"backingId"`
	Label     string    `json:"label,omitempty"`
	Added     time.Time `json:"added,omitzero"`
}

// location returns the container's remote location.
func (r containerRecord) location() Location { return Location{Bucket: r.Bucket, Prefix: r.Prefix} }

// bucketRecord is the record b/<name>/ of a bucket.
type bucketRecord struct {
	// ID identifies this incarnation of the bucket name (random, set when
	// the bucket is created, adopted or migrated): a bucket deleted and
	// created again under the same name has another ID.
	ID   string             `json:"id,omitempty"`
	Kind storage.BucketKind `json:"kind"`
	// Container is the container of a wrapped bucket.
	Container string `json:"container,omitempty"`
	// Remote and RemoteRegion are the remote bucket of a native bucket.
	Remote       string `json:"remote,omitempty"`
	RemoteRegion string `json:"remoteRegion,omitempty"`
	// Created is set when gos3 created a native bucket's remote bucket:
	// deleting the bucket deletes it. Adopted remote buckets are never
	// deleted.
	Created bool `json:"created,omitempty"`
	// Deleting is persisted before detaching the remote bucket. A restart
	// discards this record without contacting a possibly reused remote name.
	Deleting bool `json:"deleting,omitempty"`
	// Info is the bucket's info: authoritative for native buckets, a cache
	// of the container's for wrapped ones (served while the container is
	// not loaded). Kind and Location are never stored.
	Info storage.BucketInfo `json:"info"`
	// Caps are a native bucket's remote capabilities as last probed (nil:
	// not probed yet, assumed available).
	Caps  *native.Caps `json:"caps,omitempty"`
	Added time.Time    `json:"added,omitzero"`
}

// newIncarnation returns a random bucket record ID.
func newIncarnation() string {
	var r [8]byte
	_, _ = rand.Read(r[:])
	return hex.EncodeToString(r[:])
}

// sameIncarnation reports whether two records describe the same bucket
// (not one deleted and created again under the same name).
func sameIncarnation(a, b bucketRecord) bool {
	return a.ID == b.ID && a.Kind == b.Kind && a.Added.Equal(b.Added) && a.Remote == b.Remote && a.Container == b.Container
}

// binding returns a native bucket's binding.
func (r bucketRecord) binding() native.Binding {
	return native.Binding{Remote: r.Remote, Region: r.RemoteRegion, Owned: r.Created}
}

// location describes where a native bucket lives.
func (r bucketRecord) nativeLocation() string { return "s3://" + r.Remote }

// bucketEntry is a bucket in the in-memory registry.
type bucketEntry struct {
	bucketRecord
	// release forgets a native bucket's claim on its remote bucket.
	release func()
	// claimErr is set when a native bucket's remote bucket could not be
	// claimed while loading (it is used elsewhere): the bucket is
	// unavailable.
	claimErr error
}

// registry is the in-memory copy of the records. Its maps are only changed
// under Backend.mu (write-locked) and after the matching record was
// written; configuration documents are replaced, never modified in place,
// so readers may use a document after releasing the lock.
type registry struct {
	schema           int // 0: the schema record was never written
	containers       map[string]containerRecord
	buckets          map[string]*bucketEntry
	configs          map[string]map[string][]byte // native bucket -> config key -> document
	defaultContainer string                       // "" = the home (if any)
}

func newRegistry() *registry {
	return &registry{
		containers: map[string]containerRecord{},
		buckets:    map[string]*bucketEntry{},
		configs:    map[string]map[string][]byte{},
	}
}

// errNewerRecords refuses records written by a newer build.
var errNewerRecords = errors.New("s3account: the backing's records were written by a newer gos3 version")

// decodeRegistry builds the registry from the records. orphans are the
// configuration prefixes of buckets that are not native buckets (left by an
// interrupted bucket creation or deletion), which the caller deletes.
//
// The records of a home are read from the remote and not authenticated:
// records this build would never write (a container ID, backing ID, bucket
// name or configuration key of another form) are skipped, each reported in
// invalid, which the caller logs. They are kept, not deleted.
func decodeRegistry(recs map[string][]byte) (reg *registry, orphans []string, invalid []error, err error) {
	reg = newRegistry()
	skip := func(k, format string, args ...any) {
		invalid = append(invalid, fmt.Errorf("s3account: record %q skipped: "+format, append([]any{k}, args...)...))
	}
	for k, v := range recs {
		switch {
		case k == keySchema:
			var s schemaRecord
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, nil, nil, fmt.Errorf("s3account: record %s: %w", k, err)
			}
			if s.Schema > registrySchema {
				return nil, nil, nil, fmt.Errorf("%w (schema %d; this build reads %d)", errNewerRecords, s.Schema, registrySchema)
			}
			reg.schema = s.Schema
		case k == keyDefaultContainer:
			var d struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(v, &d); err != nil {
				return nil, nil, nil, fmt.Errorf("s3account: record %s: %w", k, err)
			}
			if d.ID != "" && !validContainerID(d.ID) {
				skip(k, "invalid container ID %q", d.ID)
				continue
			}
			reg.defaultContainer = d.ID
		case k == keyLegacyMetaKey:
			// The metadata key of development builds: no longer used.
		case strings.HasPrefix(k, prefixContainers) && strings.HasSuffix(k, "/"):
			var c containerRecord
			if err := json.Unmarshal(v, &c); err != nil {
				return nil, nil, nil, fmt.Errorf("s3account: record %s: %w", k, err)
			}
			c.ID = trimRecordKey(k, prefixContainers)
			if !validContainerID(c.ID) {
				skip(k, "invalid container ID %q", c.ID)
				continue
			}
			if c.ID != homeID && !s3store.ValidBackingID(c.BackingID) {
				// AddContainer records the ID of the loaded store, which is
				// always set.
				skip(k, "invalid backing ID %q", c.BackingID)
				continue
			}
			reg.containers[c.ID] = c
		case strings.HasPrefix(k, prefixBuckets) && strings.HasSuffix(k, "/"):
			var b bucketRecord
			if err := json.Unmarshal(v, &b); err != nil {
				return nil, nil, nil, fmt.Errorf("s3account: record %s: %w", k, err)
			}
			name := trimRecordKey(k, prefixBuckets)
			if err := validBucketName(name); err != nil {
				skip(k, "%w", err)
				continue
			}
			b.Info.Name, b.Info.Kind, b.Info.Location = name, "", ""
			reg.buckets[name] = &bucketEntry{bucketRecord: b}
		case strings.HasPrefix(k, prefixConfigs) && strings.HasSuffix(k, "/"):
			rest := strings.TrimSuffix(strings.TrimPrefix(k, prefixConfigs), "/")
			bucket, key, ok := strings.Cut(rest, "/")
			if !ok || bucket == "" || key == "" {
				continue
			}
			if err := validBucketName(bucket); err != nil || strings.Contains(key, "/") || !utf8.ValidString(key) {
				skip(k, "invalid bucket name or configuration key")
				continue
			}
			if reg.configs[bucket] == nil {
				reg.configs[bucket] = map[string][]byte{}
			}
			reg.configs[bucket][key] = v
		}
		// Other keys are ignored (written by a newer build of the same
		// schema).
	}
	for bucket := range reg.configs {
		if e := reg.buckets[bucket]; e == nil || e.Kind != storage.BucketKindNative {
			orphans = append(orphans, configPrefix(bucket))
			delete(reg.configs, bucket)
		}
	}
	return reg, orphans, invalid, nil
}

// equalRecords reports whether two record sets are identical.
func equalRecords(a, b map[string][]byte) bool {
	return maps.EqualFunc(a, b, bytes.Equal)
}

// recordJSON encodes a record value.
func recordJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		// Only plain structs are encoded.
		panic("s3account: encode record: " + err.Error())
	}
	return data
}

// storedInfo returns info as stored in a bucket record: Kind, Location and
// Backing are output only.
func storedInfo(info storage.BucketInfo) storage.BucketInfo {
	info.Kind, info.Location = "", ""
	return info
}

// --- write-through (callers hold adminMu) ---

// putBucket writes the record of bucket name and installs it.
func (b *Backend) putBucket(ctx context.Context, name string, rec bucketRecord, release func()) error {
	rec.Info = storedInfo(rec.Info)
	rec.Info.Name = name
	if err := b.records.PutRecord(ctx, bucketKey(name), recordJSON(rec)); err != nil {
		return fmt.Errorf("s3account %q: write the record of bucket %q: %w", b.name, name, err)
	}
	b.mu.Lock()
	e := b.reg.buckets[name]
	if e == nil {
		e = &bucketEntry{}
		b.reg.buckets[name] = e
	}
	e.bucketRecord = rec
	if release != nil {
		if old := e.release; old != nil {
			defer old()
		}
		e.release, e.claimErr = release, nil
	}
	b.mu.Unlock()
	return nil
}

// dropBucket deletes the records of bucket name (bucket record first: an
// interrupted drop leaves configuration documents, which the next load
// removes) and forgets it, releasing its claim.
func (b *Backend) dropBucket(ctx context.Context, name string) error {
	if err := b.records.DeleteRecords(ctx, bucketKey(name)); err != nil {
		return fmt.Errorf("s3account %q: delete the record of bucket %q: %w", b.name, name, err)
	}
	b.mu.Lock()
	e := b.reg.buckets[name]
	delete(b.reg.buckets, name)
	delete(b.reg.configs, name)
	b.mu.Unlock()
	if e != nil && e.release != nil {
		e.release()
	}
	if err := b.records.DeleteRecords(ctx, configPrefix(name)); err != nil {
		b.log.Warn("s3account: delete the configuration records of a deleted bucket (retried at the next start)",
			"backing", b.name, "bucket", name, "err", err)
	}
	return nil
}

// purgeConfigs deletes every configuration record of bucket name (before a
// native bucket of that name is recorded, so it never inherits the
// documents of an earlier bucket of the name whose deletion could not
// remove them).
func (b *Backend) purgeConfigs(ctx context.Context, name string) error {
	b.mu.Lock()
	delete(b.reg.configs, name)
	b.mu.Unlock()
	if err := b.records.DeleteRecords(ctx, configPrefix(name)); err != nil {
		return fmt.Errorf("s3account %q: delete the configuration records of an earlier bucket %q: %w", b.name, name, err)
	}
	return nil
}

// putConfig writes a native bucket's configuration document.
func (b *Backend) putConfig(ctx context.Context, bucket, key string, data []byte) error {
	data = bytes.Clone(data)
	if data == nil {
		data = []byte{}
	}
	if err := b.records.PutRecord(ctx, configKey(bucket, key), data); err != nil {
		return fmt.Errorf("s3account %q: write configuration %q of bucket %q: %w", b.name, key, bucket, err)
	}
	b.mu.Lock()
	if b.reg.configs[bucket] == nil {
		b.reg.configs[bucket] = map[string][]byte{}
	}
	b.reg.configs[bucket][key] = data
	b.mu.Unlock()
	return nil
}

// deleteConfig deletes a native bucket's configuration document.
func (b *Backend) deleteConfig(ctx context.Context, bucket, key string) error {
	if err := b.records.DeleteRecords(ctx, configKey(bucket, key)); err != nil {
		return fmt.Errorf("s3account %q: delete configuration %q of bucket %q: %w", b.name, key, bucket, err)
	}
	b.mu.Lock()
	delete(b.reg.configs[bucket], key)
	if len(b.reg.configs[bucket]) == 0 {
		delete(b.reg.configs, bucket)
	}
	b.mu.Unlock()
	return nil
}

// putContainerRecord writes a container's record and installs it.
func (b *Backend) putContainerRecord(ctx context.Context, rec containerRecord) error {
	if err := b.records.PutRecord(ctx, containerKey(rec.ID), recordJSON(rec)); err != nil {
		return fmt.Errorf("s3account %q: write the record of container %s: %w", b.name, rec.ID, err)
	}
	b.mu.Lock()
	b.reg.containers[rec.ID] = rec
	b.mu.Unlock()
	return nil
}

// putDefaultContainer writes the default container.
func (b *Backend) putDefaultContainer(ctx context.Context, id string) error {
	if err := b.records.PutRecord(ctx, keyDefaultContainer, recordJSON(map[string]string{"id": id})); err != nil {
		return fmt.Errorf("s3account %q: write the default container: %w", b.name, err)
	}
	b.mu.Lock()
	b.reg.defaultContainer = id
	b.mu.Unlock()
	return nil
}

// ensureSchema writes the schema record when it was never written.
func (b *Backend) ensureSchema(ctx context.Context) error {
	b.mu.RLock()
	done := b.reg.schema != 0
	b.mu.RUnlock()
	if done {
		return nil
	}
	if err := b.records.PutRecord(ctx, keySchema, recordJSON(schemaRecord{Schema: registrySchema})); err != nil {
		return fmt.Errorf("s3account %q: write the schema record: %w", b.name, err)
	}
	b.mu.Lock()
	b.reg.schema = registrySchema
	b.mu.Unlock()
	return nil
}
