package multi

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestBeforePublishRejectionKeepsPrimaryHealthy(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		multipart, compressed bool
	}{
		{"object", false, false},
		{"compressed-object", false, true},
		{"multipart", true, false},
		{"compressed-multipart", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			bk := memBackings(2)
			r := openRouter(t, Options{
				Backings: bk, NewBucketMode: ModeActiveActive, NewBucketReplicas: []string{"b"},
				// Do not let a periodic probe conceal an erroneous unhealthy mark.
				HealthInterval: time.Hour,
			})
			if err := r.CreateBucket(ctx, "bucket", storage.CreateBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			waitState(t, r, "bucket", StateInSync)
			up, err := r.CreateMultipartUpload(ctx, "bucket", "object", storage.ObjectMeta{}, "")
			if err != nil {
				t.Fatal(err)
			}
			part, err := r.NewPartWriter(ctx, "bucket", "object", up.UploadID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(part, "payload"); err != nil {
				t.Fatal(err)
			}
			if _, err := part.Commit(ctx, storage.PartMeta{ETag: "part-etag"}); err != nil {
				t.Fatal(err)
			}

			wopts := storage.WriteOptions{}
			if tc.compressed {
				wopts.Compression = storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1}
			}
			commit := func(opts storage.CommitOptions) (storage.ObjectInfo, error) {
				meta := storage.ObjectMeta{ETag: "object-etag"}
				if tc.multipart {
					return r.CompleteMultipartUploadWith(ctx, "bucket", "object", up.UploadID, []int{1}, meta, opts, wopts)
				}
				w, err := r.NewObjectWriterWith(ctx, "bucket", "object", wopts)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Abort()
				if _, err := io.WriteString(w, "payload"); err != nil {
					t.Fatal(err)
				}
				return w.Commit(ctx, meta, opts)
			}
			denied := errors.New("publication denied")
			_, err = commit(storage.CommitOptions{BeforePublish: func(*storage.ObjectMeta, *storage.ObjectMeta, time.Time, func(string) []byte) error {
				return denied
			}})
			if err != denied {
				t.Fatalf("caller error changed: %v", err)
			}
			if !r.backings["a"].healthy.Load() {
				t.Fatal("publication rejection marked the primary unhealthy")
			}
			primary, err := r.primary("bucket")
			if err != nil || primary.Name != "a" {
				t.Fatalf("primary changed: %v, %v", primary, err)
			}
			for _, backing := range bk {
				if _, err := backing.Backend.GetObjectInfo(ctx, "bucket", "object", ""); !errors.Is(err, storage.ErrObjectNotFound) {
					t.Fatalf("%s published rejected object: %v", backing.Name, err)
				}
			}
			// Multipart state must remain reachable, including after a denied
			// ordinary PUT on the same bucket.
			if parts, err := r.ListParts(ctx, "bucket", "object", up.UploadID); err != nil || len(parts) != 1 {
				t.Fatalf("upload lost after rejection: %v, %v", parts, err)
			}
			info, err := commit(storage.CommitOptions{BeforePublish: func(meta, current *storage.ObjectMeta, created time.Time, config func(string) []byte) error {
				meta.ContentType = "application/approved"
				return nil
			}})
			if err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			for _, backing := range bk {
				got, err := backing.Backend.GetObjectInfo(ctx, "bucket", "object", info.VersionID)
				if err != nil || got.ContentType != "application/approved" {
					t.Fatalf("%s retry metadata: %+v, %v", backing.Name, got, err)
				}
				if got := read(t, backing.Backend, "bucket", "object", info.VersionID); got != "payload" {
					t.Fatalf("%s data: %q", backing.Name, got)
				}
			}
		})
	}
}
