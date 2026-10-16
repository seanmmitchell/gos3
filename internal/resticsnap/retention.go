package resticsnap

import (
	"cmp"
	"slices"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// selectRetention splits snaps (any order) into the snapshots policy p
// keeps and those it forgets, like restic's forget policy but in UTC: the
// snapshots are walked newest first, and each keep rule with a count left
// keeps a snapshot whose period (the snapshot itself for keepLast, else
// its hour, day, ISO week, month or year) differs from the period of the
// rule's last kept snapshot. keepWithin keeps every snapshot within that
// duration of the newest. The newest snapshot is always kept. Both results
// are newest first.
func selectRetention(snaps []restic.Snapshot, p Retention) (keep, forget []restic.Snapshot) {
	sorted := slices.Clone(snaps)
	sortNewestFirst(sorted)
	if len(sorted) == 0 {
		return nil, nil
	}
	type rule struct {
		left   int
		period func(t time.Time, i int) int64
		last   int64
		used   bool
	}
	rules := []*rule{
		{left: p.KeepLast, period: func(_ time.Time, i int) int64 { return int64(i) }},
		{left: p.KeepHourly, period: func(t time.Time, _ int) int64 { return t.Unix() / 3600 }},
		{left: p.KeepDaily, period: func(t time.Time, _ int) int64 {
			return int64(t.Year())*10000 + int64(t.Month())*100 + int64(t.Day())
		}},
		{left: p.KeepWeekly, period: func(t time.Time, _ int) int64 {
			y, w := t.ISOWeek()
			return int64(y)*100 + int64(w)
		}},
		{left: p.KeepMonthly, period: func(t time.Time, _ int) int64 { return int64(t.Year())*100 + int64(t.Month()) }},
		{left: p.KeepYearly, period: func(t time.Time, _ int) int64 { return int64(t.Year()) }},
	}
	within := p.within()
	newest := sorted[0].Time
	for i, sn := range sorted {
		t := sn.Time.UTC()
		kept := i == 0
		for _, r := range rules {
			if r.left <= 0 {
				continue
			}
			if v := r.period(t, i); !r.used || v != r.last {
				r.last, r.used = v, true
				r.left--
				kept = true
			}
		}
		if within > 0 && !sn.Time.Before(newest.Add(-within)) {
			kept = true
		}
		if kept {
			keep = append(keep, sn)
		} else {
			forget = append(forget, sn)
		}
	}
	return keep, forget
}

// sortNewestFirst sorts snapshots newest first.
func sortNewestFirst(snaps []restic.Snapshot) {
	slices.SortFunc(snaps, func(a, b restic.Snapshot) int {
		if c := b.Time.Compare(a.Time); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
}
