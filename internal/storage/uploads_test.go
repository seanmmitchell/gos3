package storage_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// referenceUploads is the specification of ListUploads: S3
// ListMultipartUploads paging over the complete, sorted upload list (the
// algorithm the S3 layer used before ListUploads existed).
func referenceUploads(all []storage.UploadInfo, p storage.UploadListParams) storage.UploadListResult {
	var out storage.UploadListResult
	if p.MaxUploads <= 0 {
		return out
	}
	skipUnder := ""
	if cp, ok := storage.CommonPrefix(p.KeyMarker, p.Prefix, p.Delimiter); ok && cp == p.KeyMarker {
		skipUnder = p.KeyMarker
	}
	markerFound := false
	if p.KeyMarker != "" && p.UploadIDMarker != "" {
		for _, u := range all {
			if u.Key == p.KeyMarker && u.UploadID == p.UploadIDMarker {
				markerFound = true
			}
		}
	}
	pastMarker, lastCP, count := false, "", 0
	for _, u := range all {
		if !strings.HasPrefix(u.Key, p.Prefix) {
			continue
		}
		if p.KeyMarker != "" {
			if skipUnder != "" && strings.HasPrefix(u.Key, skipUnder) {
				continue
			}
			if u.Key < p.KeyMarker {
				continue
			}
			if u.Key == p.KeyMarker {
				if p.UploadIDMarker == "" {
					continue
				}
				if markerFound {
					if !pastMarker {
						pastMarker = u.UploadID == p.UploadIDMarker
						continue
					}
				} else if u.UploadID <= p.UploadIDMarker {
					continue
				}
			}
		}
		cp, isCP := storage.CommonPrefix(u.Key, p.Prefix, p.Delimiter)
		if isCP && cp == lastCP {
			continue
		}
		if count == p.MaxUploads {
			out.IsTruncated = true
			break
		}
		count++
		if isCP {
			out.CommonPrefixes = append(out.CommonPrefixes, cp)
			lastCP = cp
			out.NextKeyMarker, out.NextUploadIDMarker = cp, ""
			continue
		}
		out.Uploads = append(out.Uploads, u)
		out.NextKeyMarker, out.NextUploadIDMarker = u.Key, u.UploadID
	}
	if !out.IsTruncated {
		out.NextKeyMarker, out.NextUploadIDMarker = "", ""
	}
	return out
}

// plainUploads hides the UploadWalker of the wrapped backend, exercising
// the ListMultipartUploads fallback.
type plainUploads struct{ storage.Backend }

// countingWalker counts the uploads its walks deliver.
type countingWalker struct {
	*memory.Backend
	yielded int
}

func (c *countingWalker) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	return c.Backend.WalkMultipartUploads(ctx, bucket, prefix, after, func(u storage.UploadInfo) error {
		c.yielded++
		return fn(u)
	})
}

func summarize(r storage.UploadListResult) string {
	var b strings.Builder
	for _, u := range r.Uploads {
		fmt.Fprintf(&b, "%s@%s ", u.Key, u.UploadID)
	}
	fmt.Fprintf(&b, "| %v | trunc=%v next=%q/%q", r.CommonPrefixes, r.IsTruncated, r.NextKeyMarker, r.NextUploadIDMarker)
	return b.String()
}

func TestListUploadsMatchesReference(t *testing.T) {
	m := memory.New()
	defer m.Close()
	if err := m.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	keys := []string{"a", "a/1", "a/2", "a/b/c", "b", "b/", "c/x/y", "c/z", "d", "e-1", "日/本"}
	for range 60 {
		k := keys[rng.IntN(len(keys))]
		if _, err := m.CreateMultipartUpload(ctx, "b", k, storage.ObjectMeta{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	all, err := m.ListMultipartUploads(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	keyMarkers := []string{"", "a", "a/", "a/1", "a/b/", "b", "c/", "c/x/", "zz"}
	idMarkers := []string{"", "nonexistent", "zzzzzzzz", all[0].UploadID, all[len(all)/2].UploadID}
	for _, u := range all[:8] {
		idMarkers = append(idMarkers, u.UploadID)
	}
	for _, b := range []storage.Backend{m, plainUploads{m}} {
		for _, prefix := range []string{"", "a", "a/", "c/", "x"} {
			for _, delim := range []string{"", "/", "-"} {
				for _, km := range keyMarkers {
					for _, im := range idMarkers {
						for _, max := range []int{0, 1, 2, 5, 1000} {
							p := storage.UploadListParams{Prefix: prefix, Delimiter: delim, KeyMarker: km, UploadIDMarker: im, MaxUploads: max}
							got, err := storage.ListUploads(ctx, b, "b", p)
							if err != nil {
								t.Fatal(err)
							}
							if want := referenceUploads(all, p); summarize(got) != summarize(want) {
								t.Fatalf("%+v:\n got  %s\n want %s", p, summarize(got), summarize(want))
							}
						}
					}
				}
			}
		}
	}

	// Paging with the returned markers covers each entry exactly once.
	for _, delim := range []string{"", "/"} {
		var got []string
		p := storage.UploadListParams{Delimiter: delim, MaxUploads: 3}
		for {
			r, err := storage.ListUploads(ctx, m, "b", p)
			if err != nil {
				t.Fatal(err)
			}
			for _, u := range r.Uploads {
				got = append(got, u.UploadID)
			}
			got = append(got, r.CommonPrefixes...)
			if !r.IsTruncated {
				break
			}
			p.KeyMarker, p.UploadIDMarker = r.NextKeyMarker, r.NextUploadIDMarker
		}
		want := referenceUploads(all, storage.UploadListParams{Delimiter: delim, MaxUploads: 1 << 20})
		var wantIDs []string
		for _, u := range want.Uploads {
			wantIDs = append(wantIDs, u.UploadID)
		}
		wantIDs = append(wantIDs, want.CommonPrefixes...)
		slices.Sort(got)
		slices.Sort(wantIDs)
		if !slices.Equal(got, wantIDs) {
			t.Fatalf("delimiter %q: paged %v, want %v", delim, got, wantIDs)
		}
	}
}

// TestListUploadsCostsAPage: a page reads O(page) uploads from a backend
// with an UploadWalker, however many uploads the bucket holds.
func TestListUploadsCostsAPage(t *testing.T) {
	c := &countingWalker{Backend: memory.New()}
	defer c.Close()
	if err := c.CreateBucket(ctx, "b", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	var last storage.UploadInfo
	for i := range 5000 {
		u, err := c.CreateMultipartUpload(ctx, "b", fmt.Sprintf("dir%d/k%04d", i%3, i), storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		last = u
	}
	for _, p := range []storage.UploadListParams{
		{MaxUploads: 1},
		{MaxUploads: 10, KeyMarker: last.Key, UploadIDMarker: last.UploadID},
		{MaxUploads: 10, KeyMarker: "dir1/k2000"},
		{MaxUploads: 2, Delimiter: "/"},
	} {
		c.yielded = 0
		r, err := storage.ListUploads(ctx, c, "b", p)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Uploads)+len(r.CommonPrefixes) == 0 {
			t.Fatalf("%+v: empty page", p)
		}
		if c.yielded > 2*p.MaxUploads+2 {
			t.Errorf("%+v: read %d uploads for a page of %d", p, c.yielded, p.MaxUploads)
		}
	}
	n, err := storage.CountMultipartUploads(ctx, c, "b")
	if err != nil || n != 5000 {
		t.Fatalf("CountMultipartUploads = %d, %v", n, err)
	}
}
