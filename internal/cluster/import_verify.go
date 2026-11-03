package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// ImportInventory is a fully checked view of a stopped standalone server.
// It contains digests, not object bodies; bundle creation streams the bodies
// again while the source's directory lock remains held.
type ImportInventory struct {
	Buckets []ImportBucket
}

type ImportBucket struct {
	Info     storage.BucketInfo
	Configs  map[string][]byte
	Versions []ImportVersion
}

type ImportVersion struct {
	Info   storage.ObjectInfo
	SHA256 string
}

// ImportSource is the read-only portion of a backend needed to verify and
// export an offline namespace. A stopped cluster provides the same view after
// all old voters have been checked for identical committed state.
type ImportSource interface {
	ListBuckets(context.Context) ([]storage.BucketInfo, error)
	ListMultipartUploads(context.Context, string) ([]storage.UploadInfo, error)
	GetBucketConfig(context.Context, string, string) ([]byte, error)
	WalkVersions(context.Context, string, string, string, string, func(storage.ObjectInfo) error) error
	OpenObject(context.Context, string, string, string) (storage.ObjectInfo, io.ReadSeekCloser, error)
}

var importConfigKeys = []string{
	storage.ConfigPolicy, storage.ConfigCORS, storage.ConfigLifecycle,
	storage.ConfigTagging, storage.ConfigEncryption, storage.ConfigObjectLock,
	storage.ConfigACL, storage.ConfigWebsite, storage.ConfigOwnership,
	storage.ConfigNotification, storage.ConfigLogging, storage.ConfigPublicAccessBlock,
	storage.ConfigReplication, storage.ConfigReplicationState,
	storage.ConfigAccelerate, storage.ConfigRequestPayment,
	storage.ConfigCompression, storage.ConfigResticSnapshots,
}

// VerifyStandalone reads every source object through the backend while the
// caller holds the stopped source's exclusive data lock. It fails before any
// cluster initialization when data cannot be represented or read exactly.
func VerifyStandalone(ctx context.Context, backend ImportSource, settings config.Settings, iamDocument []byte, rootAccessKey string, maxObjectBytes int64) (ImportInventory, error) {
	if backend == nil || rootAccessKey == "" || maxObjectBytes < 1 {
		return ImportInventory{}, errors.New("cluster: offline import needs a source, root access key and object limit")
	}
	if err := config.ValidateSettings(settings); err != nil {
		return ImportInventory{}, fmt.Errorf("cluster: invalid standalone settings: %w", err)
	}
	if len(iamDocument) > 1<<20 {
		return ImportInventory{}, errors.New("cluster: standalone IAM exceeds the consensus record limit")
	}
	if err := iam.ValidateStateDocument(iamDocument, rootAccessKey); err != nil {
		return ImportInventory{}, fmt.Errorf("cluster: invalid standalone IAM: %w", err)
	}
	buckets, err := backend.ListBuckets(ctx)
	if err != nil {
		return ImportInventory{}, fmt.Errorf("cluster: listing standalone buckets: %w", err)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	out := ImportInventory{Buckets: make([]ImportBucket, 0, len(buckets))}
	for i, bucket := range buckets {
		if err := ctx.Err(); err != nil {
			return ImportInventory{}, err
		}
		if bucket.Name == "" || bucket.CreatedAt.IsZero() || i > 0 && buckets[i-1].Name == bucket.Name {
			return ImportInventory{}, fmt.Errorf("cluster: invalid standalone bucket %q", bucket.Name)
		}
		entry := ImportBucket{Info: bucket, Configs: make(map[string][]byte)}
		pending, err := backend.ListMultipartUploads(ctx, bucket.Name)
		if err != nil {
			return ImportInventory{}, fmt.Errorf("cluster: listing multipart uploads of %s: %w", bucket.Name, err)
		}
		if len(pending) != 0 {
			return ImportInventory{}, fmt.Errorf("cluster: bucket %s has %d unfinished multipart uploads; complete or abort them before import", bucket.Name, len(pending))
		}
		for _, key := range importConfigKeys {
			value, err := backend.GetBucketConfig(ctx, bucket.Name, key)
			if errors.Is(err, storage.ErrConfigNotFound) {
				continue
			}
			if err != nil {
				return ImportInventory{}, fmt.Errorf("cluster: reading %s configuration of %s: %w", key, bucket.Name, err)
			}
			if len(value) > 1<<20 {
				return ImportInventory{}, fmt.Errorf("cluster: %s configuration of %s exceeds the consensus record limit", key, bucket.Name)
			}
			entry.Configs[key] = value
		}
		var versions []storage.ObjectInfo
		if err := backend.WalkVersions(ctx, bucket.Name, "", "", "", func(info storage.ObjectInfo) error {
			versions = append(versions, info)
			return nil
		}); err != nil {
			return ImportInventory{}, fmt.Errorf("cluster: walking versions of %s: %w", bucket.Name, err)
		}
		seen := make(map[string]bool, len(versions))
		seenLatest := make(map[string]bool)
		seenKey := make(map[string]bool)
		lastKey := ""
		for _, version := range versions {
			if err := ctx.Err(); err != nil {
				return ImportInventory{}, err
			}
			identity := version.Key + "\x00" + version.VersionID
			if version.Bucket != bucket.Name || version.Key == "" || strings.ContainsRune(version.Key, 0) ||
				lastKey != "" && version.Key < lastKey || !storage.ValidImportedVersionID(version.VersionID) ||
				version.LastModified.IsZero() || version.Size < 0 || seen[identity] || version.Size > maxObjectBytes {
				return ImportInventory{}, fmt.Errorf("cluster: invalid or oversized standalone version %s/%s@%s", bucket.Name, version.Key, version.VersionID)
			}
			lastKey = version.Key
			seen[identity] = true
			if !seenKey[version.Key] && !version.IsLatest {
				return ImportInventory{}, fmt.Errorf("cluster: key %s/%s has no latest version", bucket.Name, version.Key)
			}
			seenKey[version.Key] = true
			if version.IsLatest {
				if seenLatest[version.Key] {
					return ImportInventory{}, fmt.Errorf("cluster: key %s/%s has multiple latest versions", bucket.Name, version.Key)
				}
				seenLatest[version.Key] = true
			}
			if version.Encryption != "" || version.SSE != "" || version.SSEKMSKeyID != "" {
				return ImportInventory{}, fmt.Errorf("cluster: encrypted standalone version %s/%s@%s requires cluster encryption support", bucket.Name, version.Key, version.VersionID)
			}
			if version.DeleteMarker {
				if version.Size != 0 {
					return ImportInventory{}, fmt.Errorf("cluster: delete marker %s/%s@%s has a body", bucket.Name, version.Key, version.VersionID)
				}
				entry.Versions = append(entry.Versions, ImportVersion{Info: version, SHA256: hex.EncodeToString(sha256.New().Sum(nil))})
				continue
			}
			opened, body, err := backend.OpenObject(ctx, bucket.Name, version.Key, version.VersionID)
			if err != nil {
				return ImportInventory{}, fmt.Errorf("cluster: opening %s/%s@%s: %w", bucket.Name, version.Key, version.VersionID, err)
			}
			if opened.Bucket != version.Bucket || opened.Key != version.Key || opened.VersionID != version.VersionID || opened.Size != version.Size || !opened.LastModified.Equal(version.LastModified) || !reflect.DeepEqual(opened.ObjectMeta, version.ObjectMeta) {
				_ = body.Close()
				return ImportInventory{}, fmt.Errorf("cluster: version changed during offline import verification: %s/%s@%s", bucket.Name, version.Key, version.VersionID)
			}
			hash := sha256.New()
			count, copyErr := io.Copy(hash, io.LimitReader(body, maxObjectBytes+1))
			closeErr := body.Close()
			if copyErr != nil || closeErr != nil || count != version.Size {
				return ImportInventory{}, fmt.Errorf("cluster: unreadable or truncated version %s/%s@%s: %w", bucket.Name, version.Key, version.VersionID, errors.Join(copyErr, closeErr))
			}
			entry.Versions = append(entry.Versions, ImportVersion{Info: version, SHA256: hex.EncodeToString(hash.Sum(nil))})
		}
		out.Buckets = append(out.Buckets, entry)
	}
	return out, nil
}
