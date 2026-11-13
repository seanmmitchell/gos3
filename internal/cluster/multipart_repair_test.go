package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	bolt "go.etcd.io/bbolt"
)

func TestReplicaRepairRestoresAcknowledgedMultipartParts(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	b := NewS3Backend(nodes["a"])
	if err := b.CreateBucket(ctx, "bucket", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	upload, err := b.CreateMultipartUpload(ctx, "bucket", "key", storage.ObjectMeta{}, "root")
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.NewPartWriter(ctx, "bucket", "key", upload.UploadID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, storage.PartMeta{ETag: "part"}); err != nil {
		t.Fatal(err)
	}
	raw, found, err := nodes["a"].GetMetadata(ctx, MetadataJobs, partKey("bucket", upload.UploadID, 1))
	if err != nil || !found {
		t.Fatalf("part metadata: %v %v", found, err)
	}
	part, err := decodeMultipartPart(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodes["a"].stages.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(stageBucket).Delete([]byte(part.OperationID)) }); err != nil {
		t.Fatal(err)
	}
	repaired, err := nodes["a"].RepairLocalPlacements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := nodes["a"].stages.Read(ctx, part.OperationID)
	if err != nil || string(payload) != "payload" {
		t.Fatalf("repair pass reported %d repaired and success but left acknowledged multipart bytes unavailable: %q %v", repaired, payload, err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1", repaired)
	}
	if err := nodes["b"].stages.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(stageBucket).Delete([]byte(part.OperationID)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CompleteMultipartUpload(ctx, "bucket", "key", upload.UploadID, []int{1}, storage.ObjectMeta{ETag: "complete"}, storage.CommitOptions{PartETags: []string{"part"}}); err != nil {
		t.Fatalf("repaired part could not complete after losing original peer bytes: %v", err)
	}
}
