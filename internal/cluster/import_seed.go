package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/seanmmitchell/gos3/internal/config"
	"go.etcd.io/raft/v3"
	pb "go.etcd.io/raft/v3/raftpb"
)

const importFingerprintKey = "import-fingerprint"

// SeedOfflineNode initializes one empty cluster voter from a verified bundle.
// Repeat with the same bundle on every configured voter while all nodes are
// stopped. The peer transport refuses a mismatched bundle fingerprint.
func SeedOfflineNode(ctx context.Context, cfg config.ClusterConfig, bundleDir, systemDir, rootAccessKey, rootSecretKey string, maxMemoryBytes int64, external DurableReplicaStore) (fingerprint string, err error) {
	if !cfg.Enabled || cfg.ID == "" || cfg.NodeID == "" || cfg.ReplicaCount < 2 || cfg.ReplicaCount > len(cfg.Peers) || systemDir == "" || maxMemoryBytes < 1 {
		return "", errors.New("cluster: invalid offline node configuration")
	}
	if err := (AckPolicy{Memory: cfg.MinMemoryAcks, Durable: cfg.MinDurableAcks}).Validate(cfg.ReplicaCount); err != nil {
		return "", err
	}
	bundle, fingerprint, err := LoadImportBundle(ctx, bundleDir, rootAccessKey, rootSecretKey, maxMemoryBytes)
	if err != nil {
		return "", err
	}
	peerIDs := make([]string, 0, len(cfg.Peers))
	voters := make([]uint64, 0, len(cfg.Peers))
	seen := map[string]bool{}
	for _, peer := range cfg.Peers {
		if peer.ID == "" || seen[peer.ID] {
			return "", errors.New("cluster: offline import peer IDs must be distinct")
		}
		seen[peer.ID] = true
		peerIDs = append(peerIDs, peer.ID)
		voters = append(voters, RaftID(cfg.ID, peer.ID))
	}
	if !seen[cfg.NodeID] {
		return "", errors.New("cluster: offline import node is not a voter")
	}
	slices.Sort(voters)
	for i := 1; i < len(voters); i++ {
		if voters[i] == voters[i-1] {
			return "", errors.New("cluster: offline import Raft voter IDs collide")
		}
	}
	systemDir, err = filepath.Abs(systemDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(systemDir, 0o700); err != nil {
		return "", err
	}
	clusterDir := filepath.Join(systemDir, "cluster")
	if _, err := os.Lstat(clusterDir); err == nil {
		return "", fmt.Errorf("cluster: target %s already has cluster state", clusterDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, name := range []string{"meta.db", "iam.json", "settings.json", "placements.json", "backings.json", "root-credentials.json", "server-id"} {
		if _, err := os.Lstat(filepath.Join(systemDir, name)); err == nil {
			return "", fmt.Errorf("cluster: target %s contains standalone state %s", systemDir, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	entries, err := os.ReadDir(systemDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.Name() != "lock" {
			return "", fmt.Errorf("cluster: offline target %s is not empty (contains %s)", systemDir, entry.Name())
		}
	}
	temporary, err := os.MkdirTemp(systemDir, "cluster-import-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temporary)
	store := external
	if store == nil {
		store, err = OpenStageStore(filepath.Join(temporary, "stages.db"), maxMemoryBytes)
		if err != nil {
			return "", err
		}
	} else if store.MaxMemoryBytes() != maxMemoryBytes {
		return "", errors.New("cluster: external replica store has the wrong memory limit")
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()
	for _, bucket := range bundle.Buckets {
		for _, version := range bucket.Versions {
			if version.Info.DeleteMarker {
				continue
			}
			placements, err := SelectReplicas(cfg.ID, bucket.Info.Name, version.Info.Key, peerIDs, cfg.ReplicaCount)
			if err != nil {
				return "", err
			}
			if !slices.Contains(placements, cfg.NodeID) {
				continue
			}
			payload, err := os.ReadFile(filepath.Join(bundleDir, "payloads", version.OperationID))
			if err != nil {
				return "", err
			}
			digestBytes, _ := hex.DecodeString(version.SHA256)
			var digest [sha256.Size]byte
			copy(digest[:], digestBytes)
			if int64(len(payload)) != version.Info.Size || sha256.Sum256(payload) != digest {
				return "", fmt.Errorf("cluster: import payload changed after verification: %s/%s@%s", bucket.Info.Name, version.Info.Key, version.Info.VersionID)
			}
			if err := store.RepairCommitted(ctx, version.OperationID, payload, digest); err != nil {
				return "", fmt.Errorf("cluster: seeding replica %s/%s@%s: %w", bucket.Info.Name, version.Info.Key, version.Info.VersionID, err)
			}
		}
	}
	if err := store.Close(); err != nil {
		return "", err
	}
	closed = true
	image, err := buildImportImage(bundle, fingerprint)
	if err != nil {
		return "", err
	}
	metadata, err := OpenMetadataStore(filepath.Join(temporary, "metadata.db"))
	if err != nil {
		return "", err
	}
	if err := metadata.Restore(1, image); err != nil {
		_ = metadata.Close()
		return "", err
	}
	if err := metadata.Close(); err != nil {
		return "", err
	}
	if err := seedImportRaft(filepath.Join(temporary, "raft.db"), RaftID(cfg.ID, cfg.NodeID), voters, image); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, clusterDir); err != nil {
		return "", fmt.Errorf("cluster: publishing imported node: %w", err)
	}
	return fingerprint, nil
}

func buildImportImage(bundle ImportBundle, fingerprint string) ([]byte, error) {
	image := metadataImage{Applied: 1, Records: make(map[MetadataNamespace]map[string][]byte, len(metadataNamespaces)), Outcomes: map[string][]byte{}}
	for _, ns := range metadataNamespaces {
		image.Records[ns] = map[string][]byte{}
	}
	settings, err := json.Marshal(bundle.Settings)
	if err != nil {
		return nil, err
	}
	image.Records[MetadataSettings][sharedSettingsKey] = settings
	image.Records[MetadataSettings][importFingerprintKey] = []byte(fingerprint)
	if len(bundle.IAMDocument) != 0 {
		image.Records[MetadataIAM][sharedIAMKey] = bundle.IAMDocument
	}
	for _, record := range bundle.Jobs {
		image.Records[MetadataJobs][jobRecordKey(record.ID)] = record.Data
	}
	for _, bucket := range bundle.Buckets {
		info := bucket.Info
		info.InstanceID = bucket.InstanceID
		info.Backing, info.Kind, info.Location = "", "", ""
		encoded, err := encodeBucketRecord(info)
		if err != nil {
			return nil, err
		}
		image.Records[MetadataBuckets][bucketKey(info.Name)] = encoded
		for key, value := range bucket.Configs {
			image.Records[MetadataBuckets][bucketConfigKey(info.Name, key)] = value
		}
		for i, version := range bucket.Versions {
			objectInfo := version.Info
			objectInfo.Compression, objectInfo.StoredSize = "", 0
			record := objectRecord{Schema: objectRecordSchema, Info: objectInfo, OperationID: version.OperationID, Digest: version.SHA256}
			if i+1 < len(bucket.Versions) && bucket.Versions[i+1].Info.Key == objectInfo.Key {
				record.Prior = bucket.Versions[i+1].Info.VersionID
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				return nil, err
			}
			image.Records[MetadataVersions][versionObjectKey(info.Name, objectInfo.Key, objectInfo.VersionID)] = encoded
			image.Records[MetadataVersions][objectOperationKey(version.OperationID)] = encoded
			if objectInfo.IsLatest {
				image.Records[MetadataVersions][latestObjectKey(info.Name, objectInfo.Key)] = []byte(objectInfo.VersionID)
			}
		}
	}
	return json.Marshal(image)
}

func seedImportRaft(path string, nodeID uint64, voters []uint64, image []byte) error {
	store, restarted, err := openRaftStore(path)
	if err != nil {
		return err
	}
	defer store.close()
	if restarted {
		return errors.New("cluster: offline import Raft target is not empty")
	}
	if err := store.bindNodeID(nodeID); err != nil {
		return err
	}
	if err := store.bindVoters(voters); err != nil {
		return err
	}
	index, term := uint64(1), uint64(1)
	conf := &pb.ConfState{Voters: slices.Clone(voters)}
	snapshot := &pb.Snapshot{Data: image, Metadata: &pb.SnapshotMetadata{Index: &index, Term: &term, ConfState: conf}}
	if err := store.save(raft.Ready{Snapshot: snapshot, HardState: &pb.HardState{Term: &term, Commit: &index}}); err != nil {
		return err
	}
	return store.markApplied(index, conf)
}
