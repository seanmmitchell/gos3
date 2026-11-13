package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestConditionalImportCanReplaceHistoricalVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// Use the conformance fixture's durable-write budget: this test commits
	// several two-voter mutations and is not testing storage latency.
	b := newClusterBackend(t).(*S3Backend)
	if err := b.CreateBucket(ctx, "target", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBucketVersioning(ctx, "target", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"historical", "latest", "historical"} {
		w, err := b.NewVersionWriter(ctx, "target", "key")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(id)); err != nil {
			t.Fatal(err)
		}
		info := storage.ObjectInfo{VersionID: id, LastModified: time.Now().UTC()}
		latest, err := b.GetObjectInfo(ctx, "target", "key", "")
		found := err == nil
		if _, err := w.(storage.ConditionalVersionWriter).CommitIf(ctx, info, func(current storage.ObjectInfo, exists bool) error {
			if exists != found || exists && current.VersionID != latest.VersionID {
				return storage.ErrPreconditionFailed
			}
			return nil
		}); err != nil {
			t.Fatalf("conditionally importing %s: %v", id, err)
		}
	}
	latest, err := b.GetObjectInfo(ctx, "target", "key", "")
	if err != nil || latest.VersionID != "historical" {
		t.Fatalf("latest after historical replacement: %+v, %v", latest, err)
	}
}
