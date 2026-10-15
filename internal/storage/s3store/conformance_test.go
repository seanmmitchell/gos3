package s3store

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/storagetest"
)

// TestConformance runs the storage conformance suite with no skips: every
// subtest gets a fresh backing under its own prefix of one shared remote.
// The KEK variant also stages the parts of uploads that record SSE
// encrypted.
func TestConformance(t *testing.T) {
	for _, kek := range [][]byte{nil, bytes.Repeat([]byte{1}, 32)} {
		name := "NoKEK"
		if kek != nil {
			name = "KEK"
		}
		t.Run(name, func(t *testing.T) {
			r := s3clienttest.Start(t)
			r.CreateBucket(t, "store")
			client := r.Client(t)
			var n atomic.Int64
			storagetest.Run(t, func(t *testing.T) storage.Backend {
				b, err := Open(Options{
					Name:             "conformance",
					Client:           client,
					Bucket:           "store",
					Prefix:           fmt.Sprintf("c%05d/", n.Add(1)),
					PartSize:         5 << 20,
					KEK:              kek,
					TempDir:          t.TempDir(),
					LeaseInterval:    200 * time.Millisecond,
					RecoveryInterval: -1,
					Logger:           testLogger(),
					retryMin:         20 * time.Millisecond,
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := b.WaitReady(ctx); err != nil {
					t.Fatal(err)
				}
				return b
			})
		})
	}
}
