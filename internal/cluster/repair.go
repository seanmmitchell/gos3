package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

const repairPageSize = 128

// RepairLocalPlacements checks each committed object version placed on this
// node and restores missing or divergent durable bytes from a verified peer.
// The Raft version digest, not a peer-supplied checksum, authorizes repair.
func (n *Node) RepairLocalPlacements(ctx context.Context) (repaired int, err error) {
	defer func() { n.recordRepair(repaired, err) }()
	if err := n.ReadBarrier(ctx); err != nil {
		return 0, err
	}
	peerIDs := make([]string, 0, len(n.cfg.Peers))
	for _, peer := range n.cfg.Peers {
		peerIDs = append(peerIDs, peer.ID)
	}
	const prefix = "version\x00"
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		page, err := n.metadata.ListPage(MetadataVersions, prefix, after, repairPageSize)
		if err != nil {
			return repaired, err
		}
		if len(page) == 0 {
			return repaired, nil
		}
		for _, item := range page {
			var raw objectRecord
			if err := json.Unmarshal(item.Value, &raw); err != nil {
				return repaired, fmt.Errorf("cluster: decoding repair version %q: %w", item.Key, err)
			}
			record, err := decodeObjectRecord(raw.Info.Bucket, raw.Info.Key, raw.Info.VersionID, item.Value)
			if err != nil || item.Key != versionObjectKey(record.Info.Bucket, record.Info.Key, record.Info.VersionID) {
				return repaired, fmt.Errorf("cluster: invalid repair version %q: %w", item.Key, errors.Join(err, errInvalidMetadataCommand))
			}
			if record.Info.DeleteMarker {
				continue
			}
			placements, err := SelectReplicas(n.cfg.ID, record.Info.Bucket, record.Info.Key, peerIDs, n.cfg.ReplicaCount)
			if err != nil {
				return repaired, err
			}
			if !slices.Contains(placements, n.cfg.NodeID) {
				continue
			}
			var expected [sha256.Size]byte
			digest, _ := hex.DecodeString(record.Digest) // decodeObjectRecord validated it.
			copy(expected[:], digest)
			if local, readErr := n.payloads.Read(ctx, record.OperationID); readErr == nil && int64(len(local)) == record.Info.Size && sha256.Sum256(local) == expected {
				continue
			}
			var id [16]byte
			decoded, _ := hex.DecodeString(record.OperationID)
			copy(id[:], decoded)
			fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, err = n.ReadVerifiedPayload(fetchCtx, id, record.Info.Bucket, record.Info.Key, expected, record.Info.Size)
			cancel()
			if err != nil {
				return repaired, fmt.Errorf("cluster: repairing %s/%s@%s: %w", record.Info.Bucket, record.Info.Key, record.Info.VersionID, err)
			}
			repaired++
		}
		after = page[len(page)-1].Key
		if len(page) < repairPageSize {
			return repaired, nil
		}
	}
}

func (n *Node) repairLoop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_, err := n.RepairLocalPlacements(ctx)
			if err != nil {
				timer.Reset(15 * time.Second)
			} else {
				timer.Reset(5 * time.Minute)
			}
		}
	}
}
