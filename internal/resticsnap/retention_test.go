package resticsnap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

func TestSelectRetention(t *testing.T) {
	at := func(month time.Month, day, hour int) time.Time {
		return time.Date(2024, month, day, hour, 0, 0, 0, time.UTC)
	}
	times := map[string]time.Time{
		"d1-10": at(6, 1, 10), "d1-12": at(6, 1, 12), "d2-09": at(6, 2, 9), "d3-08": at(6, 3, 8),
		"d3-20": at(6, 3, 20), "d4-01": at(6, 4, 1), "m5": at(5, 20, 0), "m4": at(4, 20, 0),
		"y23": time.Date(2023, 12, 31, 23, 0, 0, 0, time.UTC),
	}
	var snaps []restic.Snapshot
	for id, tm := range times {
		snaps = append(snaps, restic.Snapshot{ID: id, Time: tm})
	}
	for _, c := range []struct {
		name string
		p    Retention
		keep []string
	}{
		{"last 2", Retention{KeepLast: 2}, []string{"d4-01", "d3-20"}},
		{"newest always", Retention{KeepYearly: 0, KeepWithin: "1s"}, []string{"d4-01"}},
		{"daily 3", Retention{KeepDaily: 3}, []string{"d4-01", "d3-20", "d2-09"}},
		{"hourly 2", Retention{KeepHourly: 2}, []string{"d4-01", "d3-20"}},
		{"weekly 2", Retention{KeepWeekly: 2}, []string{"d4-01", "d2-09"}}, // ISO weeks start on Monday, June 3
		{"monthly 3", Retention{KeepMonthly: 3}, []string{"d4-01", "m5", "m4"}},
		{"yearly 2", Retention{KeepYearly: 2}, []string{"d4-01", "y23"}},
		{"within 36h", Retention{KeepWithin: "36h"}, []string{"d4-01", "d3-20", "d3-08"}},
		{"last 1 and daily 2", Retention{KeepLast: 1, KeepDaily: 2}, []string{"d4-01", "d3-20"}},
		{"many rules", Retention{KeepLast: 1, KeepDaily: 4, KeepMonthly: 2, KeepYearly: 5},
			[]string{"d4-01", "d3-20", "d2-09", "d1-12", "m5", "y23"}},
	} {
		keep, forget := selectRetention(snaps, c.p)
		var got []string
		for _, s := range keep {
			got = append(got, s.ID)
		}
		if !slices.Equal(got, c.keep) || len(keep)+len(forget) != len(snaps) {
			t.Errorf("%s: keep %v, want %v", c.name, got, c.keep)
		}
	}
	if keep, forget := selectRetention(nil, Retention{KeepLast: 1}); keep != nil || forget != nil {
		t.Fatal("retention of nothing")
	}
}

// Retention and Forget only ever forget the bucket's whole-bucket
// snapshots: not per-version archive snapshots, not snapshots marked both,
// not other buckets' snapshots in the same repository.
func TestRetentionGuard(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("g1", "", "")
	e.put("g1", "k", "v", storage.ObjectMeta{})
	e.configure("g1", `{"enabled":false,"retention":{"keepLast":1},"target":{"kind":"backing","backing":"disk"}}`)
	loc := e.location("g1")

	a := e.svc.rt.NewArchiverWith(ctx, loc, restic.ArchiveOptions{Bucket: "g1", Key: "k"})
	if _, err := io.WriteString(a, "archived version"); err != nil {
		t.Fatal(err)
	}
	object, err := a.Finish(nil)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	tree := func(name string, tags ...string) string {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		sum, err := e.svc.rt.BackupTree(ctx, loc, dir, restic.TreeOptions{Host: SnapshotHost, Tags: tags, Time: old})
		if err != nil {
			t.Fatal(err)
		}
		return sum.SnapshotID
	}
	marked := tree("marked", TagSnapshot, restic.BucketTag("g1"), restic.TagObject)
	other := tree("other", TagSnapshot, restic.BucketTag("g2"))
	ours := tree("ours", TagSnapshot, restic.BucketTag("g1"))

	var last string
	for range 3 {
		last = e.snapshot("g1").Snapshot
	}
	all, err := e.svc.rt.Snapshots(ctx, loc, restic.SnapshotFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range all {
		ids = append(ids, s.ID)
	}
	for _, id := range []string{object, marked, other, last} {
		if !slices.Contains(ids, id) {
			t.Fatalf("snapshot %s was forgotten (left: %v)", id[:8], ids)
		}
	}
	if len(ids) != 4 || slices.Contains(ids, ours) {
		t.Fatalf("retention left %d snapshots: %v", len(ids), ids)
	}
	listed, err := e.svc.Snapshots(ctx, "g1")
	if err != nil || len(listed) != 1 || listed[0].ID != last {
		t.Fatalf("Snapshots = %+v, %v", listed, err)
	}

	for _, id := range []string{object, marked, other} {
		err := e.svc.Forget(ctx, "g1", id)
		if !errors.Is(err, restic.ErrForgetRefused) || !errors.Is(err, jobs.ErrInvalidSpec) {
			t.Fatalf("Forget(%s): %v", id[:8], err)
		}
	}
	missing := fmt.Sprintf("%064d", 0)
	if err := e.svc.Forget(ctx, "g1", missing); !errors.Is(err, restic.ErrSnapshotNotFound) {
		t.Fatalf("Forget(missing): %v", err)
	}
	if err := e.svc.Forget(ctx, "g1", "latest"); !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Fatalf("Forget(latest): %v", err)
	}
	if err := e.svc.Forget(ctx, "g1", last); err != nil {
		t.Fatal(err)
	}
	if listed, err := e.svc.Snapshots(ctx, "g1"); err != nil || len(listed) != 0 {
		t.Fatalf("Snapshots after Forget = %+v, %v", listed, err)
	}
}
