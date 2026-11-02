package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

const importBundleVersion = 1
const importManifestName = "manifest.json"

// ImportBundle is one immutable, offline export shared by every new voter.
// The manifest and payload files contain secrets and require private storage.
type ImportBundle struct {
	Version          int
	CredentialSHA256 string
	Settings         config.Settings
	IAMDocument      []byte
	Jobs             []JobRecord
	Buckets          []ImportBundleBucket
}

type ImportBundleBucket struct {
	Info       storage.BucketInfo
	InstanceID string
	Configs    map[string][]byte
	Versions   []ImportBundleVersion
}

type ImportBundleVersion struct {
	Info        storage.ObjectInfo
	OperationID string
	SHA256      string
}

func importCredentialDigest(accessKey, secretKey string) string {
	hash := sha256.Sum256([]byte(accessKey + "\x00" + secretKey))
	return hex.EncodeToString(hash[:])
}

// ExportStandaloneBundle verifies a stopped source and then writes an
// immutable bundle into a new directory. Every body is rechecked while it is
// copied; an error leaves no final bundle directory behind.
func ExportStandaloneBundle(ctx context.Context, backend ImportSource, settings config.Settings, iamDocument []byte, rootAccessKey, rootSecretKey, destination string, maxObjectBytes int64) (_ ImportBundle, err error) {
	if rootSecretKey == "" || destination == "" {
		return ImportBundle{}, errors.New("cluster: offline export needs root credentials and a destination")
	}
	inventory, err := VerifyStandalone(ctx, backend, settings, iamDocument, rootAccessKey, maxObjectBytes)
	if err != nil {
		return ImportBundle{}, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return ImportBundle{}, err
	}
	if _, err := os.Lstat(destination); err == nil {
		return ImportBundle{}, fmt.Errorf("cluster: import bundle destination %s already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ImportBundle{}, err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return ImportBundle{}, err
	}
	temporary, err := os.MkdirTemp(parent, ".gos3-import-*")
	if err != nil {
		return ImportBundle{}, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Mkdir(filepath.Join(temporary, "payloads"), 0o700); err != nil {
		return ImportBundle{}, err
	}
	bundle := ImportBundle{Version: importBundleVersion, CredentialSHA256: importCredentialDigest(rootAccessKey, rootSecretKey), Settings: settings, IAMDocument: iamDocument}
	if source, ok := backend.(interface {
		ListImportJobs(context.Context) ([]JobRecord, error)
	}); ok {
		bundle.Jobs, err = source.ListImportJobs(ctx)
		if err != nil {
			return ImportBundle{}, err
		}
		if err := validateImportJobs(bundle.Jobs); err != nil {
			return ImportBundle{}, err
		}
	}
	for _, source := range inventory.Buckets {
		var bucketID [16]byte
		if _, err := rand.Read(bucketID[:]); err != nil {
			return ImportBundle{}, err
		}
		bucket := ImportBundleBucket{Info: source.Info, InstanceID: hex.EncodeToString(bucketID[:]), Configs: source.Configs}
		for _, version := range source.Versions {
			if err := ctx.Err(); err != nil {
				return ImportBundle{}, err
			}
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return ImportBundle{}, err
			}
			entry := ImportBundleVersion{Info: version.Info, OperationID: hex.EncodeToString(id[:]), SHA256: version.SHA256}
			if !version.Info.DeleteMarker {
				opened, body, err := backend.OpenObject(ctx, source.Info.Name, version.Info.Key, version.Info.VersionID)
				if err != nil {
					return ImportBundle{}, err
				}
				if !reflect.DeepEqual(opened, version.Info) {
					_ = body.Close()
					return ImportBundle{}, fmt.Errorf("cluster: source version changed while exporting %s/%s@%s", source.Info.Name, version.Info.Key, version.Info.VersionID)
				}
				path := filepath.Join(temporary, "payloads", entry.OperationID)
				file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					_ = body.Close()
					return ImportBundle{}, err
				}
				hash := sha256.New()
				count, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, maxObjectBytes+1))
				closeBodyErr := body.Close()
				syncErr := file.Sync()
				closeFileErr := file.Close()
				if err := errors.Join(copyErr, closeBodyErr, syncErr, closeFileErr); err != nil {
					return ImportBundle{}, err
				}
				if count != version.Info.Size || hex.EncodeToString(hash.Sum(nil)) != version.SHA256 {
					return ImportBundle{}, fmt.Errorf("cluster: source version changed while exporting %s/%s@%s", source.Info.Name, version.Info.Key, version.Info.VersionID)
				}
			}
			bucket.Versions = append(bucket.Versions, entry)
		}
		bundle.Buckets = append(bundle.Buckets, bucket)
	}
	manifest, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return ImportBundle{}, err
	}
	manifestFile, err := os.OpenFile(filepath.Join(temporary, importManifestName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ImportBundle{}, err
	}
	_, writeErr := manifestFile.Write(append(manifest, '\n'))
	syncErr := manifestFile.Sync()
	closeErr := manifestFile.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return ImportBundle{}, err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return ImportBundle{}, fmt.Errorf("cluster: publishing offline bundle: %w", err)
	}
	return bundle, nil
}

// LoadImportBundle verifies every record and payload before a node uses the
// bundle. The returned fingerprint is the digest of the exact manifest bytes;
// all voters initialized from a migration must advertise the same value.
func LoadImportBundle(ctx context.Context, directory, rootAccessKey, rootSecretKey string, maxObjectBytes int64) (ImportBundle, string, error) {
	if directory == "" || rootAccessKey == "" || rootSecretKey == "" || maxObjectBytes < 1 {
		return ImportBundle{}, "", errors.New("cluster: invalid offline bundle inputs")
	}
	manifestPath := filepath.Join(directory, importManifestName)
	file, err := os.Open(manifestPath)
	if err != nil {
		return ImportBundle{}, "", err
	}
	defer file.Close()
	const maxManifestBytes = 512 << 20
	raw, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil || len(raw) > maxManifestBytes {
		return ImportBundle{}, "", fmt.Errorf("cluster: import manifest is unreadable or oversized: %w", err)
	}
	var bundle ImportBundle
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&bundle); err != nil {
		return ImportBundle{}, "", fmt.Errorf("cluster: invalid import manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ImportBundle{}, "", errors.New("cluster: import manifest has trailing data")
	}
	if bundle.Version != importBundleVersion || bundle.CredentialSHA256 != importCredentialDigest(rootAccessKey, rootSecretKey) {
		return ImportBundle{}, "", errors.New("cluster: import bundle version or root credentials differ")
	}
	if err := config.ValidateSettings(bundle.Settings); err != nil {
		return ImportBundle{}, "", fmt.Errorf("cluster: invalid import settings: %w", err)
	}
	if len(bundle.IAMDocument) > 1<<20 {
		return ImportBundle{}, "", errors.New("cluster: import IAM state exceeds the consensus record limit")
	}
	if err := iam.ValidateStateDocument(bundle.IAMDocument, rootAccessKey); err != nil {
		return ImportBundle{}, "", fmt.Errorf("cluster: invalid import IAM state: %w", err)
	}
	if err := validateImportJobs(bundle.Jobs); err != nil {
		return ImportBundle{}, "", err
	}
	seenBuckets := make(map[string]bool, len(bundle.Buckets))
	seenOperations := make(map[string]bool)
	for _, bucket := range bundle.Buckets {
		bucketID, idErr := hex.DecodeString(bucket.InstanceID)
		if bucket.Info.Name == "" || bucket.Info.CreatedAt.IsZero() || len(bucketID) != 16 || idErr != nil || seenBuckets[bucket.Info.Name] {
			return ImportBundle{}, "", fmt.Errorf("cluster: invalid import bucket %q", bucket.Info.Name)
		}
		seenBuckets[bucket.Info.Name] = true
		for key, value := range bucket.Configs {
			if !slices.Contains(importConfigKeys, key) || len(value) > 1<<20 {
				return ImportBundle{}, "", fmt.Errorf("cluster: invalid import bucket configuration %s/%s", bucket.Info.Name, key)
			}
		}
		seenVersions := make(map[string]bool, len(bucket.Versions))
		lastKey := ""
		for _, version := range bucket.Versions {
			if err := ctx.Err(); err != nil {
				return ImportBundle{}, "", err
			}
			info := version.Info
			operationID, opErr := hex.DecodeString(version.OperationID)
			digest, digestErr := hex.DecodeString(version.SHA256)
			identity := info.Key + "\x00" + info.VersionID
			if info.Bucket != bucket.Info.Name || info.Key == "" || strings.ContainsRune(info.Key, 0) || !storage.ValidImportedVersionID(info.VersionID) || info.LastModified.IsZero() || info.Size < 0 || info.Size > maxObjectBytes || len(operationID) != 16 || opErr != nil || len(digest) != sha256.Size || digestErr != nil || seenVersions[identity] || seenOperations[version.OperationID] || info.Encryption != "" || info.SSE != "" || info.SSEKMSKeyID != "" {
				return ImportBundle{}, "", fmt.Errorf("cluster: invalid import version %s/%s@%s", bucket.Info.Name, info.Key, info.VersionID)
			}
			if lastKey != "" && info.Key < lastKey || info.Key == lastKey && info.IsLatest || info.Key != lastKey && !info.IsLatest {
				return ImportBundle{}, "", fmt.Errorf("cluster: import versions of %s are not in source walk order", bucket.Info.Name)
			}
			lastKey = info.Key
			seenVersions[identity], seenOperations[version.OperationID] = true, true
			if info.DeleteMarker {
				if info.Size != 0 || version.SHA256 != hex.EncodeToString(sha256.New().Sum(nil)) {
					return ImportBundle{}, "", fmt.Errorf("cluster: invalid import delete marker %s/%s@%s", bucket.Info.Name, info.Key, info.VersionID)
				}
				continue
			}
			payload, err := os.Open(filepath.Join(directory, "payloads", version.OperationID))
			if err != nil {
				return ImportBundle{}, "", err
			}
			hash := sha256.New()
			count, copyErr := io.Copy(hash, io.LimitReader(payload, maxObjectBytes+1))
			closeErr := payload.Close()
			if copyErr != nil || closeErr != nil || count != info.Size || hex.EncodeToString(hash.Sum(nil)) != version.SHA256 {
				return ImportBundle{}, "", fmt.Errorf("cluster: import payload is missing, truncated or corrupt: %s/%s@%s: %w", bucket.Info.Name, info.Key, info.VersionID, errors.Join(copyErr, closeErr))
			}
		}
	}
	fingerprint := sha256.Sum256(raw)
	return bundle, hex.EncodeToString(fingerprint[:]), nil
}

func validateImportJobs(records []JobRecord) error {
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if !validJobRecordID(record.ID) || seen[record.ID] || !json.Valid(record.Data) || len(record.Data) > 1<<20 {
			return fmt.Errorf("cluster: invalid import background job %q", record.ID)
		}
		seen[record.ID] = true
	}
	return nil
}
