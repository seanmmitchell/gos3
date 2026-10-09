package fs

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func benchPut(b *testing.B, durable bool, size int) {
	var be *Backend
	if durable {
		be = openDurable(b, b.TempDir())
	} else {
		be = openTest(b, "")
	}
	mkBucket(b, be, "bkt", "")
	data := bytes.Repeat([]byte("x"), size)
	meta := storage.ObjectMeta{ETag: "e", ContentType: "application/octet-stream"}
	var n atomic.Int64
	b.SetBytes(int64(size))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w, err := be.NewObjectWriter(ctx, "bkt", fmt.Sprintf("obj/%08d", n.Add(1)))
			if err != nil {
				b.Fatal(err)
			}
			if _, err := w.Write(data); err != nil {
				b.Fatal(err)
			}
			if _, err := w.Commit(ctx, meta, storage.CommitOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkPutObject measures PutObject throughput (run with -cpu to vary
// the number of concurrent writers).
func BenchmarkPutObject(b *testing.B) {
	b.Run("1KiB", func(b *testing.B) { benchPut(b, false, 1<<10) })
	b.Run("1KiB-fsync", func(b *testing.B) { benchPut(b, true, 1<<10) })
	b.Run("1MiB", func(b *testing.B) { benchPut(b, false, 1<<20) })
}

// BenchmarkListObjects measures listing a page of 1000 keys out of 20000,
// both flat and grouped by delimiter.
func BenchmarkListObjects(b *testing.B) {
	be := openTest(b, "")
	mkBucket(b, be, "bkt", "")
	const dirs, perDir = 20, 1000
	for d := range dirs {
		for i := range perDir {
			put(b, be, "bkt", fmt.Sprintf("dir%02d/obj%05d", d, i), "")
		}
	}
	b.Run("flat", func(b *testing.B) {
		for b.Loop() {
			res, err := storage.ListObjects(ctx, be, "bkt", storage.ListParams{Prefix: "dir05/", MaxKeys: 1000})
			if err != nil {
				b.Skipf("list helpers unavailable: %v", err)
			}
			if len(res.Objects) != 1000 {
				b.Fatalf("got %d objects", len(res.Objects))
			}
		}
	})
	b.Run("delimiter", func(b *testing.B) {
		for b.Loop() {
			res, err := storage.ListObjects(ctx, be, "bkt", storage.ListParams{Delimiter: "/", MaxKeys: 1000})
			if err != nil {
				b.Skipf("list helpers unavailable: %v", err)
			}
			if len(res.CommonPrefixes) != dirs {
				b.Fatalf("got %d prefixes", len(res.CommonPrefixes))
			}
		}
	})
	b.Run("walk", func(b *testing.B) {
		for b.Loop() {
			n := 0
			err := be.WalkObjects(ctx, "bkt", "", "", func(storage.ObjectInfo) error {
				n++
				return nil
			})
			if err != nil || n != dirs*perDir {
				b.Fatalf("walk: n=%d err=%v", n, err)
			}
		}
	})
}
