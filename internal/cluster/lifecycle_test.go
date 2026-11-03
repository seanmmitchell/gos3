package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/lifecycle"
	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/raft/v3"
)

func TestClusterLifecycleExpiresThroughLeaderAndReplicatesResult(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	writerBackend := NewS3Backend(nodes["a"])
	if err := writerBackend.CreateBucket(ctx, "expiry", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := writerBackend.SetBucketVersioning(ctx, "expiry", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	var old storage.ObjectInfo
	for i := range 2 {
		writer, err := writerBackend.NewObjectWriter(ctx, "expiry", "key")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
		info, err := writer.Commit(ctx, storage.ObjectMeta{ETag: "etag"}, storage.CommitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			old = info
		}
	}
	const rules = `<LifecycleConfiguration><Rule><ID>expire</ID><Filter/><Status>Enabled</Status><Expiration><Days>1</Days></Expiration><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule></LifecycleConfiguration>`
	if _, err := lifecycle.Parse([]byte(rules)); err != nil {
		t.Fatal(err)
	}
	if err := writerBackend.PutBucketConfig(ctx, "expiry", storage.ConfigLifecycle, []byte(rules)); err != nil {
		t.Fatal(err)
	}
	var leader *S3Backend
	deadline := time.Now().Add(10 * time.Second)
	for leader == nil && time.Now().Before(deadline) {
		for _, node := range nodes {
			if node.consensus.Status().RaftState == raft.StateLeader {
				leader = NewS3Backend(node)
				break
			}
		}
		if leader == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if leader == nil {
		t.Fatal("no cluster leader")
	}
	worker := lifecycle.NewWorker(leader, lifecycle.Options{Now: func() time.Time { return time.Now().Add(72 * time.Hour) }})
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats.Expired != 1 || stats.NoncurrentExpired != 1 || stats.Errors != 0 {
		t.Fatalf("cluster lifecycle: %+v, %v", stats, err)
	}
	for name, node := range nodes {
		backend := NewS3Backend(node)
		latest, err := backend.GetObjectInfo(ctx, "expiry", "key", "")
		if err != nil || !latest.DeleteMarker {
			t.Fatalf("%s latest version: %+v, %v", name, latest, err)
		}
		if _, err := backend.GetObjectInfo(ctx, "expiry", "key", old.VersionID); !errors.Is(err, storage.ErrVersionNotFound) {
			t.Fatalf("%s old version remained: %v", name, err)
		}
	}
}
