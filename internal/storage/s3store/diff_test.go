package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/index"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// The differential test applies the same random operations to the memory
// backend and to an s3store backing that is closed and reopened (with and
// without snapshots) along the way, and compares their full states: the
// s3store state must survive every reopen exactly, and equal the memory
// state up to generated identities and times.

type dumpVersion struct {
	Info    storage.ObjectInfo
	Content string
}

type dumpUpload struct {
	Info  storage.UploadInfo
	Parts []storage.PartInfo
}

type dumpBucket struct {
	Info     storage.BucketInfo
	Configs  map[string]string
	Versions []dumpVersion
	Uploads  []dumpUpload
}

var diffConfigKeys = []string{storage.ConfigTagging, storage.ConfigCORS, storage.ConfigPolicy}

func dumpState(t *testing.T, b storage.Backend) []dumpBucket {
	t.Helper()
	ctx := context.Background()
	infos, err := b.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []dumpBucket
	for _, info := range infos {
		db := dumpBucket{Info: info, Configs: map[string]string{}}
		for _, k := range diffConfigKeys {
			if data, err := b.GetBucketConfig(ctx, info.Name, k); err == nil {
				db.Configs[k] = string(data)
			}
		}
		err := b.WalkVersions(ctx, info.Name, "", "", "", func(o storage.ObjectInfo) error {
			dv := dumpVersion{Info: o}
			if !o.DeleteMarker {
				_, r, err := b.OpenObject(ctx, info.Name, o.Key, o.VersionID)
				if err != nil {
					return err
				}
				h := sha256.New()
				_, err = io.Copy(h, r)
				_ = r.Close()
				if err != nil {
					return err
				}
				dv.Content = hex.EncodeToString(h.Sum(nil))
			}
			db.Versions = append(db.Versions, dv)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		uploads, err := b.ListMultipartUploads(ctx, info.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range uploads {
			parts, err := b.ListParts(ctx, info.Name, u.Key, u.UploadID)
			if err != nil {
				t.Fatal(err)
			}
			db.Uploads = append(db.Uploads, dumpUpload{Info: u, Parts: parts})
		}
		out = append(out, db)
	}
	return out
}

// normalize removes what legitimately differs between two backends:
// generated version and upload IDs, creation times.
func normalize(state []dumpBucket) []dumpBucket {
	out := make([]dumpBucket, len(state))
	for i, db := range state {
		db.Info.CreatedAt, db.Info.Backing = time.Time{}, ""
		vs := make([]dumpVersion, len(db.Versions))
		for j, v := range db.Versions {
			if !strings.HasPrefix(v.Info.VersionID, "imp-") {
				v.Info.LastModified = time.Time{}
				if _, ok := index.StampFromVersionID(v.Info.VersionID); ok {
					v.Info.VersionID = "generated"
				}
			}
			vs[j] = v
		}
		db.Versions = vs
		us := make([]dumpUpload, len(db.Uploads))
		for j, u := range db.Uploads {
			u.Info.UploadID, u.Info.Initiated = "", time.Time{}
			ps := make([]storage.PartInfo, len(u.Parts))
			for k, p := range u.Parts {
				p.LastModified = time.Time{}
				ps[k] = p
			}
			u.Parts = ps
			us[j] = u
		}
		db.Uploads = us
		out[i] = db
	}
	return out
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// firstDiff returns the first differing line of two texts.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return fmt.Sprintf("line %d:\n  %s\n  %s", i+1, al[i], bl[i])
		}
	}
	return fmt.Sprintf("lengths %d and %d lines", len(al), len(bl))
}

type diffUpload struct {
	bucket, key string
	mem, s3     string
	parts       []int
}

func TestDifferentialReopen(t *testing.T) {
	v := newBenv(t)
	opts := v.options("diff/")
	s3b := v.ready(opts)
	mem := memory.New()
	t.Cleanup(func() { _ = mem.Close() })
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(1, 2))
	backends := func() []storage.Backend { return []storage.Backend{mem, s3b} }

	for _, b := range backends() {
		for _, name := range []string{"b0", "b1", "b2"} {
			if err := b.CreateBucket(ctx, name, storage.CreateBucketOptions{Owner: "o", Backing: "x"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.SetBucketVersioning(ctx, "b1", storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	buckets := []string{"b0", "b1", "b2"}
	keys := []string{"k0", "k1", "k2", "dir/k3", "dir/k4"}
	var uploads []*diffUpload
	pick := func(s []string) string { return s[rng.IntN(len(s))] }
	data := func() []byte {
		n := []int{0, 7, 3000, 70000}[rng.IntN(4)]
		return bytes.Repeat([]byte(fmt.Sprintf("%d-", rng.IntN(1000))), n+1)[:n]
	}
	// both runs op on both backends and requires the same outcome.
	both := func(what string, op func(b storage.Backend, s3 bool) error) {
		t.Helper()
		em, es := op(mem, false), op(s3b, true)
		if (em == nil) != (es == nil) {
			t.Fatalf("%s: memory error %v, s3store error %v", what, em, es)
		}
	}
	// versionIDs returns the version IDs of key in b, newest first.
	versionIDs := func(b storage.Backend, bucket, key string) []string {
		var ids []string
		_ = b.WalkVersions(ctx, bucket, key, "", "", func(o storage.ObjectInfo) error {
			if o.Key == key {
				ids = append(ids, o.VersionID)
			}
			return nil
		})
		return ids
	}

	reopen := func(snapshot bool) {
		t.Helper()
		before := dumpState(t, s3b)
		if snapshot {
			if err := s3b.eng.Snapshot(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := s3b.Close(); err != nil {
			t.Fatal(err)
		}
		s3b = v.ready(opts)
		after := dumpState(t, s3b)
		if a, b := jsonOf(t, before), jsonOf(t, after); a != b {
			t.Fatalf("state changed across reopen (snapshot %v): %s", snapshot, firstDiff(a, b))
		}
	}

	const ops = 400
	for i := range ops {
		bucket, key := pick(buckets), pick(keys)
		switch op := rng.IntN(12); op {
		case 0, 1, 2:
			content := data()
			var wopts storage.WriteOptions
			if rng.IntN(2) == 0 {
				wopts.Compression = storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1 + rng.IntN(9)}
			}
			meta := storage.ObjectMeta{ETag: fmt.Sprint("e", i), UserMeta: map[string]string{"op": fmt.Sprint(i)}, ContentType: "text/plain"}
			both("put", func(b storage.Backend, _ bool) error {
				w, err := b.(storage.CompressingBackend).NewObjectWriterWith(ctx, bucket, key, wopts)
				if err != nil {
					return err
				}
				_, _ = w.Write(content)
				_, err = w.Commit(ctx, meta, storage.CommitOptions{})
				return err
			})
		case 3:
			both("delete", func(b storage.Backend, _ bool) error {
				_, err := b.DeleteObject(ctx, bucket, key, "")
				return err
			})
		case 4:
			pos := rng.IntN(4)
			both("delete version", func(b storage.Backend, _ bool) error {
				ids := versionIDs(b, bucket, key)
				if pos >= len(ids) {
					return nil
				}
				_, err := b.DeleteObject(ctx, bucket, key, ids[pos])
				return err
			})
		case 5:
			status := []storage.VersioningStatus{storage.VersioningEnabled, storage.VersioningSuspended}[rng.IntN(2)]
			both("versioning", func(b storage.Backend, _ bool) error { return b.SetBucketVersioning(ctx, bucket, status) })
		case 6:
			k := diffConfigKeys[rng.IntN(len(diffConfigKeys))]
			if rng.IntN(3) == 0 {
				both("delete config", func(b storage.Backend, _ bool) error { return b.DeleteBucketConfig(ctx, bucket, k) })
			} else {
				doc := []byte(fmt.Sprint("doc", i))
				if rng.IntN(5) == 0 {
					doc = []byte{}
				}
				both("put config", func(b storage.Backend, _ bool) error { return b.PutBucketConfig(ctx, bucket, k, doc) })
			}
		case 7:
			tag := fmt.Sprint("t", i)
			both("update meta", func(b storage.Backend, _ bool) error {
				_, err := b.UpdateObjectMeta(ctx, bucket, key, "", func(m *storage.ObjectMeta) error {
					m.Tags = map[string]string{"t": tag}
					return nil
				})
				return err
			})
		case 8:
			u := &diffUpload{bucket: bucket, key: key}
			for _, b := range backends() {
				info, err := b.CreateMultipartUpload(ctx, bucket, key, storage.ObjectMeta{ContentType: "mp"}, "me")
				if err != nil {
					t.Fatal(err)
				}
				if b == storage.Backend(mem) {
					u.mem = info.UploadID
				} else {
					u.s3 = info.UploadID
				}
			}
			uploads = append(uploads, u)
		case 9:
			if len(uploads) == 0 {
				continue
			}
			ui := rng.IntN(len(uploads))
			u := uploads[ui]
			switch rng.IntN(4) {
			case 0, 1:
				n := 1 + rng.IntN(3)
				content := data()
				both("part", func(b storage.Backend, s3 bool) error {
					id := u.mem
					if s3 {
						id = u.s3
					}
					w, err := b.NewPartWriter(ctx, u.bucket, u.key, id, n)
					if err != nil {
						return err
					}
					_, _ = w.Write(content)
					_, err = w.Commit(ctx, storage.PartMeta{ETag: fmt.Sprint("p", i)})
					return err
				})
				if !containsInt(u.parts, n) {
					u.parts = append(u.parts, n)
				}
			case 2:
				nums := sortedInts(u.parts)
				var wopts storage.WriteOptions
				if rng.IntN(2) == 0 {
					wopts.Compression = storage.Compression{Algorithm: storage.CompressionDeflate, Level: 3}
				}
				both("complete", func(b storage.Backend, s3 bool) error {
					id := u.mem
					if s3 {
						id = u.s3
					}
					_, err := b.(storage.CompressingBackend).CompleteMultipartUploadWith(ctx, u.bucket, u.key, id, nums,
						storage.ObjectMeta{ETag: fmt.Sprint("mp", i)}, storage.CommitOptions{}, wopts)
					return err
				})
				uploads = append(uploads[:ui], uploads[ui+1:]...)
			case 3:
				both("abort", func(b storage.Backend, s3 bool) error {
					id := u.mem
					if s3 {
						id = u.s3
					}
					return b.AbortMultipartUpload(ctx, u.bucket, u.key, id)
				})
				uploads = append(uploads[:ui], uploads[ui+1:]...)
			}
		case 10:
			id := []string{fmt.Sprint("imp-", i), storage.NullVersionID}[rng.IntN(2)]
			dm := rng.IntN(4) == 0
			content := data()
			if dm {
				content = nil
			}
			info := storage.ObjectInfo{VersionID: id, DeleteMarker: dm, LastModified: time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC)}
			if !dm {
				info.ObjectMeta = storage.ObjectMeta{ETag: "imported", Tags: map[string]string{"a": "b"}}
			}
			both("import", func(b storage.Backend, _ bool) error {
				w, err := b.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
				if err != nil {
					return err
				}
				_, _ = w.Write(content)
				_, err = w.Commit(ctx, info)
				return err
			})
		case 11:
			lock := rng.IntN(2) == 0
			both("object lock", func(b storage.Backend, _ bool) error { return b.SetBucketObjectLock(ctx, bucket, lock) })
		}
		if i%50 == 25 && i < ops-50 {
			if err := s3b.eng.Snapshot(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if i%40 == 39 {
			reopen(i%80 == 79)
		}
	}
	reopen(false)
	reopen(true)
	final := dumpState(t, s3b)
	var versions, uploadsLeft, compressed int
	for _, db := range final {
		versions += len(db.Versions)
		uploadsLeft += len(db.Uploads)
		for _, v := range db.Versions {
			if v.Info.Compression != "" {
				compressed++
			}
		}
	}
	t.Logf("final state: %d versions (%d compressed), %d uploads, %d journal objects, %d snapshots",
		versions, compressed, uploadsLeft, len(v.list("diff/meta/journal/")), len(v.list("diff/meta/snapshots/")))
	if versions < 20 || compressed == 0 {
		t.Fatalf("the random operations built too little state (%d versions, %d compressed)", versions, compressed)
	}
	if a, b := jsonOf(t, normalize(dumpState(t, mem))), jsonOf(t, normalize(final)); a != b {
		t.Fatalf("s3store state differs from memory: %s", firstDiff(a, b))
	}
	// Garbage: only referenced blobs remain once deletes are flushed.
	s3b.flushDeletes()
	st, err := s3b.current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refs := s3b.referenced(st)
	for _, area := range []string{"diff/data/", "diff/parts/"} {
		for _, k := range v.list(area) {
			if !refs[k] {
				t.Errorf("unreferenced blob left behind: %s", k)
			}
		}
	}
}

func containsInt(s []int, n int) bool {
	for _, v := range s {
		if v == n {
			return true
		}
	}
	return false
}

func sortedInts(s []int) []int {
	out := append([]int(nil), s...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
