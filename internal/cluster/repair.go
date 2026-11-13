package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
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
			break
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
			payload, err := n.readVerifiedPayload(fetchCtx, id, record.Info.Bucket, record.Info.Key, expected, record.Info.Size, false)
			if err == nil {
				err = n.payloads.RepairCommitted(fetchCtx, record.OperationID, payload, expected)
			}
			cancel()
			if err != nil {
				return repaired, fmt.Errorf("cluster: repairing %s/%s@%s: %w", record.Info.Bucket, record.Info.Key, record.Info.VersionID, err)
			}
			repaired++
		}
		after = page[len(page)-1].Key
		if len(page) < repairPageSize {
			break
		}
	}
	parts, err := n.repairMultipartParts(ctx, peerIDs)
	return repaired + parts, err
}

func (n *Node) repairMultipartParts(ctx context.Context, peerIDs []string) (int, error) {
	repaired, after := 0, ""
	for {
		if err := ctx.Err(); err != nil {
			return repaired, err
		}
		page, err := n.metadata.ListPage(MetadataJobs, "part\x00", after, repairPageSize)
		if err != nil {
			return repaired, err
		}
		for _, item := range page {
			pieces := strings.Split(item.Key, "\x00")
			if len(pieces) != 4 {
				return repaired, errors.New("cluster: invalid repair part key")
			}
			number, err := strconv.Atoi(pieces[3])
			if err != nil || number < 1 || number > 10000 || item.Key != partKey(pieces[1], pieces[2], number) {
				return repaired, errors.New("cluster: invalid repair part number")
			}
			part, err := decodeMultipartPart(item.Value, number)
			if err != nil {
				return repaired, err
			}
			raw, found, err := n.metadata.Get(MetadataJobs, uploadKey(pieces[1], pieces[2]))
			if err != nil {
				return repaired, err
			}
			if !found {
				continue
			} // Upload completed or was aborted during the scan.
			var upload storage.UploadInfo
			if err := json.Unmarshal(raw, &upload); err != nil || upload.Bucket != pieces[1] || upload.UploadID != pieces[2] || upload.Key == "" {
				return repaired, errors.New("cluster: invalid multipart upload during repair")
			}
			placements, err := SelectReplicas(n.cfg.ID, upload.Bucket, upload.Key, peerIDs, n.cfg.ReplicaCount)
			if err != nil {
				return repaired, err
			}
			if !slices.Contains(placements, n.cfg.NodeID) {
				continue
			}
			var id [16]byte
			decoded, _ := hex.DecodeString(part.OperationID)
			copy(id[:], decoded)
			var digest [32]byte
			decoded, _ = hex.DecodeString(part.Digest)
			copy(digest[:], decoded)
			changed, err := n.repairPayload(ctx, payloadRepair{id: id, bucket: upload.Bucket, key: upload.Key, digest: digest, size: part.Info.Size})
			if err != nil {
				return repaired, fmt.Errorf("cluster: repairing multipart %s: %w", item.Key, err)
			}
			if changed {
				repaired++
			}
		}
		if len(page) < repairPageSize {
			return repaired, nil
		}
		after = page[len(page)-1].Key
	}
}

func (n *Node) repairPayload(ctx context.Context, request payloadRepair) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id := hex.EncodeToString(request.id[:])
	if local, err := n.payloads.Read(ctx, id); err == nil && int64(len(local)) == request.size && sha256.Sum256(local) == request.digest {
		return false, nil
	}
	payload, err := n.readVerifiedPayload(ctx, request.id, request.bucket, request.key, request.digest, request.size, false)
	if err != nil {
		return false, err
	}
	if err := n.payloads.RepairCommitted(ctx, id, payload, request.digest); err != nil {
		return false, err
	}
	return true, nil
}

func (n *Node) repairLoop(ctx context.Context) {
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-n.repairs:
			changed, err := n.repairPayload(ctx, request)
			count := 0
			if changed {
				count = 1
			}
			n.recordRepair(count, err)
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
