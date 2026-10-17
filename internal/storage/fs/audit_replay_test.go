package fs

import (
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"go.etcd.io/bbolt"
)

func TestBeforePublishReplayStartsWithOriginalMetadata(t *testing.T) {
	b := openTest(t, "")
	if err := b.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	bucketID, err := b.encryptionBucketID(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	// Queue the successful commit before a co-batched failure, forcing its
	// publication callback to run once in the rolled-back batch and again.
	b.commits.mu.Lock()
	b.commits.running = true
	b.commits.mu.Unlock()
	result := make(chan error, 1)
	calls := 0
	go func() {
		_, err := b.commitObject(commitReq{bucket: "b", bucketID: bucketID, key: "k", meta: storage.ObjectMeta{Tags: map[string]string{"input": "original"}}, opts: storage.CommitOptions{BeforePublish: func(m, current *storage.ObjectMeta, created time.Time, config func(string) []byte) error {
			calls++
			if _, ok := m.Tags["previous-attempt"]; ok {
				return errors.New("callback received mutated metadata from rolled-back attempt")
			}
			m.Tags["previous-attempt"] = "yes"
			return nil
		}}})
		result <- err
	}()
	waitQueued := func(n int) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			b.commits.mu.Lock()
			queued := len(b.commits.queue)
			b.commits.mu.Unlock()
			if queued == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("commit did not queue")
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitQueued(1)
	failure := make(chan error, 1)
	go func() {
		failure <- b.commits.do(b.db, func(*bbolt.Tx) error { return errors.New("injected co-batched failure") })
	}()
	waitQueued(2)
	go b.commits.loop(b.db)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := <-failure; err == nil {
		t.Fatal("failed callback unexpectedly succeeded")
	}
	if calls != 2 {
		t.Fatalf("callback calls=%d, want replay", calls)
	}
}
