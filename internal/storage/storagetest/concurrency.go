package storagetest

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// concurrencyTests run many operations in parallel; they are meant to be
// run with -race and check that every individual result is self-consistent
// and that the final state matches what the workers did.
func concurrencyTests() []testCase {
	return []testCase{
		{"MixedOperations", testConcurrentMixed},
		{"VersionedPutsSameKey", testConcurrentVersionedPuts},
		{"ReadersDuringOverwrite", testConcurrentReaders},
		{"DeleteVersionsSameKey", testConcurrentDeleteVersions},
		{"CreateSameBucket", testConcurrentCreateBucket},
		{"BucketsAndConfigs", testConcurrentBuckets},
		{"PartsSameUpload", testConcurrentParts},
	}
}

// payload returns deterministic data for (worker, seq) of a size between
// 0 and 16 KiB.
func payload(worker, seq int) []byte {
	seed := uint64(worker)<<32 | uint64(seq)
	n := int((seed * 2654435761) % (16 << 10))
	return randBytes(n, seed)
}

// checkConsistentRead reads bucket/key and verifies that content, Size and
// ETag agree (the ETag is the MD5 of the content for every write in these
// tests). It returns the content, or found=false when the key does not
// exist or its latest version is a delete marker.
func checkConsistentRead(e *env, bucket, key string) (data []byte, found bool) {
	info, data, err := readObject(e.ctx, e.b, bucket, key, "")
	switch {
	case errors.Is(err, storage.ErrObjectNotFound):
		return nil, false
	case err != nil:
		e.t.Errorf("concurrent OpenObject(%q): %v", key, err)
		return nil, false
	case info.DeleteMarker:
		return nil, false
	}
	if info.Size != int64(len(data)) || info.ETag != etagOf(data) {
		e.t.Errorf("concurrent OpenObject(%q): torn read: Size %d ETag %s, content %d bytes with MD5 %s",
			key, info.Size, info.ETag, len(data), etagOf(data))
	}
	return data, true
}

// checkSortedUnique verifies a walk/list result is strictly ascending.
func checkSortedUnique(e *env, what string, keys []string) {
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			e.t.Errorf("concurrent %s: keys not strictly ascending at %d: %q then %q", what, i, keys[i-1], keys[i])
			return
		}
	}
}

func testConcurrentMixed(e *env) {
	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningEnabled} {
		e.runFresh(versioningName(status), func(e *env) {
			const (
				workers   = 8
				ownKeys   = 12
				sharedN   = 4
				opsPerKey = 6
			)
			e.mkVersioned("b", status)
			// Each worker owns its keys (so their final state is known) and
			// all workers fight over the shared keys.
			final := make([]map[string][]byte, workers) // nil value = deleted
			var wg sync.WaitGroup
			for w := range workers {
				final[w] = map[string][]byte{}
				wg.Go(func() {
					rng := rand.New(rand.NewPCG(uint64(w), 99))
					seq := 0
					for range ownKeys * opsPerKey {
						seq++
						own := fmt.Sprintf("w%d/k%02d", w, rng.IntN(ownKeys))
						shared := fmt.Sprintf("shared/%d", rng.IntN(sharedN))
						switch op := rng.IntN(10); {
						case op < 4: // put own + shared
							data := payload(w, seq)
							if _, err := putObject(e.ctx, e.b, "b", own, data, simpleMeta(data), storage.CommitOptions{}); err != nil {
								e.t.Errorf("worker %d: put %q: %v", w, own, err)
								return
							}
							final[w][own] = data
							sd := payload(w+workers, seq)
							if _, err := putObject(e.ctx, e.b, "b", shared, sd, simpleMeta(sd), storage.CommitOptions{}); err != nil {
								e.t.Errorf("worker %d: put %q: %v", w, shared, err)
								return
							}
						case op < 6: // read own (exact) + shared (consistent)
							got, found := checkConsistentRead(e, "b", own)
							want, exists := final[w][own]
							if found != (exists && want != nil) || (found && !bytes.Equal(got, want)) {
								e.t.Errorf("worker %d: read %q: found=%v (%d bytes), want exists=%v (%d bytes)", w, own, found, len(got), exists && want != nil, len(want))
							}
							checkConsistentRead(e, "b", shared)
						case op < 8: // delete own + shared
							if _, err := e.b.DeleteObject(e.ctx, "b", own, ""); err != nil {
								e.t.Errorf("worker %d: delete %q: %v", w, own, err)
								return
							}
							if _, ok := final[w][own]; ok {
								final[w][own] = nil
							}
							if _, err := e.b.DeleteObject(e.ctx, "b", shared, ""); err != nil {
								e.t.Errorf("worker %d: delete %q: %v", w, shared, err)
								return
							}
						case op < 9: // walk
							var keys []string
							err := e.b.WalkObjects(e.ctx, "b", "", "", func(o storage.ObjectInfo) error {
								if o.DeleteMarker {
									return fmt.Errorf("WalkObjects yielded delete marker %q", o.Key)
								}
								keys = append(keys, o.Key)
								return nil
							})
							if err != nil {
								e.t.Errorf("worker %d: WalkObjects: %v", w, err)
								return
							}
							checkSortedUnique(e, "WalkObjects", keys)
						default: // list with delimiter + versions
							res, err := storage.ListObjects(e.ctx, e.b, "b", storage.ListParams{Delimiter: "/", MaxKeys: 3})
							if err != nil {
								e.t.Errorf("worker %d: ListObjects: %v", w, err)
								return
							}
							checkSortedUnique(e, "ListObjects prefixes", res.CommonPrefixes)
							if _, err := storage.ListObjectVersions(e.ctx, e.b, "b", storage.VersionListParams{Prefix: fmt.Sprintf("w%d/", w), MaxKeys: 5}); err != nil {
								e.t.Errorf("worker %d: ListObjectVersions: %v", w, err)
								return
							}
						}
					}
				})
			}
			wg.Wait()
			if e.t.Failed() {
				return
			}

			// Final state: owned keys exactly as tracked.
			var wantKeys []string
			for w := range workers {
				for k, data := range final[w] {
					if data == nil {
						_, found := checkConsistentRead(e, "b", k)
						if found {
							e.t.Errorf("deleted key %q still readable", k)
						}
						continue
					}
					wantKeys = append(wantKeys, k)
					e.wantContent("b", k, "", data)
				}
			}
			sort.Strings(wantKeys)
			var gotOwn []string
			for _, k := range e.walkKeys("b", "", "") {
				if strings.HasPrefix(k, "shared/") {
					checkConsistentRead(e, "b", k)
					continue
				}
				gotOwn = append(gotOwn, k)
			}
			if !eqStrings(gotOwn, wantKeys) {
				e.t.Errorf("final WalkObjects (owned keys):\n got  %q\n want %q", gotOwn, wantKeys)
			}
			// Every version listed is readable and consistent.
			for _, v := range e.walkVersions("b", "", "", "") {
				if v.DeleteMarker {
					continue
				}
				info, data, err := readObject(e.ctx, e.b, "b", v.Key, v.VersionID)
				if err != nil {
					e.t.Errorf("OpenObject(%q, %q) of a walked version: %v", v.Key, v.VersionID, err)
					continue
				}
				if info.ETag != etagOf(data) || info.Size != int64(len(data)) {
					e.t.Errorf("version %q@%s inconsistent: Size %d ETag %s vs %d bytes MD5 %s", v.Key, v.VersionID, info.Size, info.ETag, len(data), etagOf(data))
				}
			}
		})
	}
}

func testConcurrentVersionedPuts(e *env) {
	const workers, perWorker = 8, 10
	e.mkVersioned("b", storage.VersioningEnabled)
	type written struct {
		id   string
		data []byte
	}
	results := make([][]written, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				data := payload(w, i)
				info, err := putObject(e.ctx, e.b, "b", "k", data, simpleMeta(data), storage.CommitOptions{})
				if err != nil {
					e.t.Errorf("worker %d put %d: %v", w, i, err)
					return
				}
				results[w] = append(results[w], written{info.VersionID, data})
			}
		})
	}
	wg.Wait()
	if e.t.Failed() {
		return
	}
	seen := map[string]bool{}
	for w, rs := range results {
		for _, r := range rs {
			if seen[r.id] {
				e.t.Errorf("version ID %q returned to two concurrent writers", r.id)
			}
			seen[r.id] = true
			e.checkVersionID(fmt.Sprintf("worker %d", w), r.id)
			e.wantContent("b", "k", r.id, r.data)
		}
	}
	h := e.history("b", "k")
	if len(h) != workers*perWorker {
		e.t.Fatalf("history has %d versions, want %d", len(h), workers*perWorker)
	}
	for i, v := range h {
		if !seen[v.VersionID] || v.IsLatest != (i == 0) {
			e.t.Errorf("history entry %d = %v: unknown version or wrong IsLatest", i, v)
		}
	}
	// Each worker's own versions appear in its commit order (newest first).
	pos := map[string]int{}
	for i, v := range h {
		pos[v.VersionID] = i
	}
	for w, rs := range results {
		for i := 1; i < len(rs); i++ {
			if pos[rs[i].id] > pos[rs[i-1].id] {
				e.t.Errorf("worker %d: version %q committed after %q but listed as older", w, rs[i].id, rs[i-1].id)
			}
		}
	}
	if latest := e.info("b", "k", ""); latest.VersionID != h[0].VersionID {
		e.t.Errorf("GetObjectInfo latest = %q, WalkVersions newest = %q", latest.VersionID, h[0].VersionID)
	}
}

func testConcurrentReaders(e *env) {
	for _, status := range []storage.VersioningStatus{storage.VersioningUnversioned, storage.VersioningEnabled} {
		e.runFresh(versioningName(status), func(e *env) {
			e.mkVersioned("b", status)
			const writes, readers = 40, 6
			valid := map[string]bool{} // ETags of every payload ever written
			for i := range writes {
				valid[etagOf(payload(0, i))] = true
			}
			e.put("b", "k", payload(0, 0))
			done := make(chan struct{})
			var wg sync.WaitGroup
			for r := range readers {
				wg.Go(func() {
					for {
						select {
						case <-done:
							return
						default:
						}
						data, found := checkConsistentRead(e, "b", "k")
						if !found {
							e.t.Errorf("reader %d: object vanished during overwrites", r)
							return
						}
						if !valid[etagOf(data)] {
							e.t.Errorf("reader %d: read %d bytes that were never written", r, len(data))
							return
						}
					}
				})
			}
			for i := 1; i < writes; i++ {
				data := payload(0, i)
				if _, err := putObject(e.ctx, e.b, "b", "k", data, simpleMeta(data), storage.CommitOptions{}); err != nil {
					e.t.Errorf("overwrite %d: %v", i, err)
					break
				}
			}
			close(done)
			wg.Wait()
			e.wantContent("b", "k", "", payload(0, writes-1))
		})
	}
}

func testConcurrentDeleteVersions(e *env) {
	e.mkVersioned("b", storage.VersioningEnabled)
	vs := e.putVersions("b", "k", 24)
	e.put("b", "other", []byte("survivor"))
	var wg sync.WaitGroup
	for _, v := range vs {
		wg.Go(func() {
			res, err := e.b.DeleteObject(e.ctx, "b", "k", v.VersionID)
			if err != nil {
				e.t.Errorf("DeleteObject(k, %q): %v", v.VersionID, err)
				return
			}
			if res.VersionID != v.VersionID || res.DeleteMarker {
				e.t.Errorf("DeleteObject(k, %q) = %+v", v.VersionID, res)
			}
		})
	}
	// Concurrent latest-version deletes create delete markers alongside.
	markers := make([]string, 4)
	for i := range markers {
		wg.Go(func() {
			res, err := e.b.DeleteObject(e.ctx, "b", "k", "")
			if err != nil {
				e.t.Errorf("DeleteObject(k) creating a marker: %v", err)
				return
			}
			markers[i] = res.VersionID
		})
	}
	wg.Wait()
	h := e.history("b", "k")
	var got []string
	for _, v := range h {
		if !v.DeleteMarker {
			e.t.Errorf("version %v survived concurrent deletion", v)
		}
		got = append(got, v.VersionID)
	}
	slices.Sort(got)
	slices.Sort(markers)
	if !slices.Equal(got, markers) {
		e.t.Errorf("remaining versions of k = %q, want exactly the %d delete markers %q", got, len(markers), markers)
	}
	if info, err := e.b.GetObjectInfo(e.ctx, "b", "k", ""); err != nil || !info.DeleteMarker {
		e.t.Errorf("GetObjectInfo(k) = %+v, %v; want a delete marker as latest", info, err)
	}
	e.wantContent("b", "other", "", []byte("survivor"))
}

func testConcurrentCreateBucket(e *env) {
	const n = 16
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for i := range n {
		wg.Go(func() {
			err := e.b.CreateBucket(e.ctx, "contended", storage.CreateBucketOptions{Owner: fmt.Sprint(i)})
			switch {
			case err == nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case !errors.Is(err, storage.ErrBucketExists):
				e.t.Errorf("concurrent CreateBucket #%d: %v, want nil or ErrBucketExists", i, err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		e.t.Errorf("%d concurrent CreateBucket calls succeeded, want exactly 1", wins)
	}
	list, err := e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets", err)
	if len(list) != 1 {
		e.t.Errorf("ListBuckets = %+v, want one bucket", list)
	}
}

func testConcurrentBuckets(e *env) {
	const n = 12
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			name := fmt.Sprintf("bucket-%02d", i)
			if err := e.b.CreateBucket(e.ctx, name, storage.CreateBucketOptions{}); err != nil {
				e.t.Errorf("CreateBucket(%s): %v", name, err)
				return
			}
			for _, k := range []string{storage.ConfigPolicy, storage.ConfigCORS, storage.ConfigTagging} {
				if err := e.b.PutBucketConfig(e.ctx, name, k, []byte(name+"/"+k)); err != nil {
					e.t.Errorf("PutBucketConfig(%s, %s): %v", name, k, err)
				}
			}
			data := []byte(name)
			if _, err := putObject(e.ctx, e.b, name, "obj", data, simpleMeta(data), storage.CommitOptions{}); err != nil {
				e.t.Errorf("put into %s: %v", name, err)
			}
			if _, err := e.b.ListBuckets(e.ctx); err != nil {
				e.t.Errorf("ListBuckets: %v", err)
			}
			if i%2 == 1 { // odd buckets are emptied and deleted again
				if _, err := e.b.DeleteObject(e.ctx, name, "obj", ""); err != nil {
					e.t.Errorf("DeleteObject in %s: %v", name, err)
				}
				if err := e.b.DeleteBucket(e.ctx, name); err != nil {
					e.t.Errorf("DeleteBucket(%s): %v", name, err)
				}
			}
		})
	}
	wg.Wait()
	list, err := e.b.ListBuckets(e.ctx)
	e.noErr("ListBuckets", err)
	var got, want []string
	for _, b := range list {
		got = append(got, b.Name)
	}
	for i := 0; i < n; i += 2 {
		name := fmt.Sprintf("bucket-%02d", i)
		want = append(want, name)
		e.wantContent(name, "obj", "", []byte(name))
		for _, k := range []string{storage.ConfigPolicy, storage.ConfigCORS, storage.ConfigTagging} {
			doc, err := e.b.GetBucketConfig(e.ctx, name, k)
			if err != nil || string(doc) != name+"/"+k {
				e.t.Errorf("GetBucketConfig(%s, %s) = %q, %v", name, k, doc, err)
			}
		}
	}
	if !eqStrings(got, want) {
		e.t.Errorf("ListBuckets after concurrent create/delete = %q, want %q", got, want)
	}
}

func testConcurrentParts(e *env) {
	e.mkBucket("b")
	u := e.createUpload("b", "k", storage.ObjectMeta{})
	const n = 16
	parts := make([][]byte, n)
	for i := range parts {
		parts[i] = payload(7, i)
	}
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			// Upload each part twice concurrently with itself; the
			// replacement semantics make the result identical.
			for range 2 {
				if _, err := uploadPart(e.ctx, e.b, "b", "k", u.UploadID, i+1, parts[i], storage.PartMeta{ETag: etagOf(parts[i])}); err != nil {
					e.t.Errorf("upload part %d: %v", i+1, err)
				}
			}
		})
	}
	// Concurrent ListParts must always be sorted.
	wg.Go(func() {
		for range 20 {
			ps, err := e.b.ListParts(e.ctx, "b", "k", u.UploadID)
			if err != nil {
				e.t.Errorf("concurrent ListParts: %v", err)
				return
			}
			for j := 1; j < len(ps); j++ {
				if ps[j-1].PartNumber >= ps[j].PartNumber {
					e.t.Errorf("concurrent ListParts not sorted: %d then %d", ps[j-1].PartNumber, ps[j].PartNumber)
				}
			}
		}
	})
	wg.Wait()
	if e.t.Failed() {
		return
	}
	nums := make([]int, n)
	for i := range nums {
		nums[i] = i + 1
	}
	ps, err := e.b.ListParts(e.ctx, "b", "k", u.UploadID)
	e.noErr("ListParts", err)
	if len(ps) != n {
		e.t.Fatalf("ListParts returned %d parts, want %d", len(ps), n)
	}
	e.complete("b", "k", u.UploadID, nums, completeMeta(storage.ObjectMeta{}, parts...))
	e.wantContent("b", "k", "", slices.Concat(parts...))
}
