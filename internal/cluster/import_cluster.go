package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// OfflineClusterMember is one stopped voter and its node-specific durable
// replica store. The caller must hold every member's exclusive system lock
// until the export finishes and close every Store afterward.
type OfflineClusterMember struct {
	Config    config.ClusterConfig
	SystemDir string
	Store     DurableReplicaStore
}

// OfflineClusterSource reads a single, verified committed namespace from all
// old voters. It never starts Raft or writes to their stores.
type OfflineClusterSource struct {
	clusterID string
	peers     []string
	replicas  int
	durable   int
	image     metadataImage
	stores    map[string]DurableReplicaStore
}

var _ ImportSource = (*OfflineClusterSource)(nil)

// OpenOfflineClusterSource requires every configured voter to be stopped,
// locked, fully applied, and in agreement about the complete state image.
// It refuses pending multipart records and preserves shared background jobs.
func OpenOfflineClusterSource(ctx context.Context, members []OfflineClusterMember) (*OfflineClusterSource, config.Settings, []byte, error) {
	if len(members) < 2 {
		return nil, config.Settings{}, nil, errors.New("cluster: offline migration needs every voter")
	}
	first := members[0].Config
	if !first.Enabled || first.ID == "" || len(first.Peers) != len(members) || first.ReplicaCount < 2 || first.MinDurableAcks < 1 {
		return nil, config.Settings{}, nil, errors.New("cluster: invalid source membership for offline migration")
	}
	peerURLs := make(map[string]string, len(first.Peers))
	voters := make([]uint64, 0, len(first.Peers))
	peerIDs := make([]string, 0, len(first.Peers))
	for _, peer := range first.Peers {
		if peer.ID == "" || peer.URL == "" || peerURLs[peer.ID] != "" {
			return nil, config.Settings{}, nil, errors.New("cluster: duplicate or invalid source peer")
		}
		peerURLs[peer.ID] = peer.URL
		peerIDs = append(peerIDs, peer.ID)
		voters = append(voters, RaftID(first.ID, peer.ID))
	}
	slices.Sort(voters)
	for i := 1; i < len(voters); i++ {
		if voters[i] == voters[i-1] {
			return nil, config.Settings{}, nil, errors.New("cluster: source Raft voter IDs collide")
		}
	}
	source := &OfflineClusterSource{clusterID: first.ID, peers: peerIDs, replicas: first.ReplicaCount,
		durable: first.MinDurableAcks, stores: make(map[string]DurableReplicaStore, len(members))}
	var reference []byte
	var applied uint64
	for _, member := range members {
		if err := ctx.Err(); err != nil {
			return nil, config.Settings{}, nil, err
		}
		cfg := member.Config
		if !cfg.Enabled || cfg.ID != first.ID || cfg.NodeID == "" || source.stores[cfg.NodeID] != nil ||
			cfg.ReplicaCount != first.ReplicaCount || cfg.MinMemoryAcks != first.MinMemoryAcks || cfg.MinDurableAcks != first.MinDurableAcks ||
			len(cfg.Peers) != len(peerURLs) || member.SystemDir == "" || member.Store == nil || member.Store.MaxMemoryBytes() != cfg.StageMemoryBytes {
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: incompatible offline voter %q", cfg.NodeID)
		}
		if _, ok := peerURLs[cfg.NodeID]; !ok {
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: unexpected offline voter %q", cfg.NodeID)
		}
		for _, peer := range cfg.Peers {
			if peerURLs[peer.ID] != peer.URL {
				return nil, config.Settings{}, nil, fmt.Errorf("cluster: peer set differs on voter %q", cfg.NodeID)
			}
		}
		clusterDir := filepath.Join(member.SystemDir, "cluster")
		metadataPath, raftPath := filepath.Join(clusterDir, "metadata.db"), filepath.Join(clusterDir, "raft.db")
		for _, path := range []string{metadataPath, raftPath} {
			info, err := os.Stat(path)
			if err != nil {
				return nil, config.Settings{}, nil, fmt.Errorf("cluster: missing offline voter state %s: %w", path, err)
			}
			if !info.Mode().IsRegular() {
				return nil, config.Settings{}, nil, fmt.Errorf("cluster: offline voter state %s is not a regular file", path)
			}
		}
		metadata, err := OpenMetadataStore(metadataPath)
		if err != nil {
			return nil, config.Settings{}, nil, err
		}
		image, snapshotErr := metadata.Snapshot(^uint64(0))
		closeErr := metadata.Close()
		if err := errors.Join(snapshotErr, closeErr); err != nil {
			return nil, config.Settings{}, nil, err
		}
		raftLog, restarted, err := openRaftStore(raftPath)
		if err != nil {
			return nil, config.Settings{}, nil, err
		}
		hard, _, stateErr := raftLog.memory.InitialState()
		actualVoters := slices.Clone(raftLog.conf.GetVoters())
		localApplied := raftLog.applied
		closeErr = raftLog.close()
		if err := errors.Join(stateErr, closeErr); err != nil {
			return nil, config.Settings{}, nil, err
		}
		slices.Sort(actualVoters)
		if !restarted || localApplied == 0 || hard.GetCommit() != localApplied || !slices.Equal(actualVoters, voters) {
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: voter %s has unapplied or mismatched Raft state; restart and stop the old cluster cleanly", cfg.NodeID)
		}
		if reference == nil {
			reference, applied = image, localApplied
			if err := json.Unmarshal(image, &source.image); err != nil {
				return nil, config.Settings{}, nil, err
			}
		} else if localApplied != applied || !bytes.Equal(image, reference) {
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: voter %s disagrees with the old cluster's committed namespace", cfg.NodeID)
		}
		source.stores[cfg.NodeID] = member.Store
	}
	if len(source.image.Records[MetadataRoles]) != 0 {
		return nil, config.Settings{}, nil, errors.New("cluster: unsupported role state in offline migration")
	}
	for key, value := range source.image.Records[MetadataJobs] {
		if !strings.HasPrefix(key, backgroundJobPrefix) || !validJobRecordID(strings.TrimPrefix(key, backgroundJobPrefix)) || !json.Valid(value) || len(value) > 1<<20 {
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: pending multipart or invalid background job record %q must be resolved before migration", key)
		}
	}
	for key := range source.image.Records[MetadataIAM] {
		if key != sharedIAMKey {
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: unsupported IAM record %q", key)
		}
	}
	for key := range source.image.Records[MetadataSettings] {
		if key != sharedSettingsKey && key != importFingerprintKey {
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: unsupported settings record %q", key)
		}
	}
	for key := range source.image.Records[MetadataBuckets] {
		switch {
		case strings.HasPrefix(key, "bucket/"):
		case strings.HasPrefix(key, "config\x00"):
			rest := strings.TrimPrefix(key, "config\x00")
			bucket, name, found := strings.Cut(rest, "\x00")
			if !found || source.image.Records[MetadataBuckets][bucketKey(bucket)] == nil || !slices.Contains(importConfigKeys, name) {
				return nil, config.Settings{}, nil, fmt.Errorf("cluster: unsupported bucket configuration record %q", key)
			}
		default:
			return nil, config.Settings{}, nil, fmt.Errorf("cluster: unsupported bucket record %q", key)
		}
	}
	var settings config.Settings
	if err := json.Unmarshal(source.image.Records[MetadataSettings][sharedSettingsKey], &settings); err != nil {
		return nil, config.Settings{}, nil, fmt.Errorf("cluster: invalid shared settings: %w", err)
	}
	if err := config.ValidateSettings(settings); err != nil {
		return nil, config.Settings{}, nil, err
	}
	return source, settings, bytes.Clone(source.image.Records[MetadataIAM][sharedIAMKey]), nil
}

// ListImportJobs returns the complete stopped cluster job catalog. The
// exporter includes these records in the bundle instead of dropping work.
func (s *OfflineClusterSource) ListImportJobs(ctx context.Context) ([]JobRecord, error) {
	var out []JobRecord
	for key, value := range s.image.Records[MetadataJobs] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, JobRecord{ID: strings.TrimPrefix(key, backgroundJobPrefix), Data: bytes.Clone(value)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *OfflineClusterSource) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	var buckets []storage.BucketInfo
	for key, raw := range s.image.Records[MetadataBuckets] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(key, "bucket/") {
			continue
		}
		info, err := decodeBucketRecord(strings.TrimPrefix(key, "bucket/"), raw)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, info)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })
	return buckets, nil
}

func (s *OfflineClusterSource) ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error) {
	return nil, ctx.Err() // OpenOfflineClusterSource already refused nonempty MetadataJobs.
}

func (s *OfflineClusterSource) GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.image.Records[MetadataBuckets][bucketKey(bucket)] == nil {
		return nil, storage.ErrBucketNotFound
	}
	value, ok := s.image.Records[MetadataBuckets][bucketConfigKey(bucket, key)]
	if !ok {
		return nil, storage.ErrConfigNotFound
	}
	return bytes.Clone(value), nil
}

func (s *OfflineClusterSource) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if s.image.Records[MetadataBuckets][bucketKey(bucket)] == nil {
		return storage.ErrBucketNotFound
	}
	if fn == nil {
		return errors.New("cluster: nil offline version callback")
	}
	versions := s.image.Records[MetadataVersions]
	root := "latest\x00" + bucket + "\x00"
	var keys []string
	for key := range versions {
		if strings.HasPrefix(key, root) && strings.HasPrefix(strings.TrimPrefix(key, root), prefix) {
			keys = append(keys, strings.TrimPrefix(key, root))
		}
	}
	slices.Sort(keys)
	seenRecords := make(map[string]bool)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		version := string(versions[latestObjectKey(bucket, key)])
		seen := map[string]bool{}
		for latest := true; version != ""; latest = false {
			if seen[version] {
				return fmt.Errorf("cluster: cycle in offline version chain %s/%s", bucket, key)
			}
			seen[version] = true
			recordKey := versionObjectKey(bucket, key, version)
			seenRecords[recordKey] = true
			record, err := decodeObjectRecord(bucket, key, version, versions[recordKey])
			if err != nil {
				return err
			}
			info := record.Info
			info.IsLatest = latest
			if key > keyMarker || key == keyMarker && (versionIDMarker == "" || version != versionIDMarker) {
				if err := fn(info); err != nil {
					if errors.Is(err, storage.ErrStopWalk) {
						return nil
					}
					return err
				}
			}
			version = record.Prior
		}
	}
	// Reject unreachable live records. Dropping them in a migration would hide
	// a corrupt old namespace instead of preserving it.
	if prefix == "" && keyMarker == "" && versionIDMarker == "" {
		for key := range versions {
			if strings.HasPrefix(key, "version\x00"+bucket+"\x00") && !seenRecords[key] {
				return fmt.Errorf("cluster: unreachable offline version record %q", key)
			}
		}
	}
	return nil
}

func (s *OfflineClusterSource) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	versions := s.image.Records[MetadataVersions]
	if versionID == "" {
		versionID = string(versions[latestObjectKey(bucket, key)])
	}
	record, err := decodeObjectRecord(bucket, key, versionID, versions[versionObjectKey(bucket, key, versionID)])
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	if record.Info.DeleteMarker {
		return storage.ObjectInfo{}, nil, storage.ErrObjectNotFound
	}
	placements, err := SelectReplicas(s.clusterID, bucket, key, s.peers, s.replicas)
	if err != nil {
		return storage.ObjectInfo{}, nil, err
	}
	expected, err := hex.DecodeString(record.Digest)
	if err != nil || len(expected) != sha256.Size {
		return storage.ObjectInfo{}, nil, errors.New("cluster: invalid offline object digest")
	}
	var body []byte
	var durable int
	for _, node := range placements {
		if err := ctx.Err(); err != nil {
			return storage.ObjectInfo{}, nil, err
		}
		candidate, err := s.stores[node].Read(ctx, record.OperationID)
		if err != nil || int64(len(candidate)) != record.Info.Size {
			continue
		}
		digest := sha256.Sum256(candidate)
		if !bytes.Equal(digest[:], expected) {
			continue
		}
		if body == nil {
			body = candidate
		}
		durable++
	}
	if durable < s.durable {
		return storage.ObjectInfo{}, nil, fmt.Errorf("cluster: offline version %s/%s@%s has %d/%d verified durable copies", bucket, key, versionID, durable, s.durable)
	}
	info := record.Info
	info.IsLatest = string(versions[latestObjectKey(bucket, key)]) == versionID
	return info, byteReadSeekCloser{bytes.NewReader(body)}, nil
}
