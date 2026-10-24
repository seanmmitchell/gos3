package restic

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// pattern returns n bytes in which every short run of bytes identifies its
// offset, so that a read from the wrong place is noticed.
func pattern(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return string(b)
}

// waitFor polls cond until it holds, failing the test after 10 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Distinct ranges of one object each restore on their own, but no more
// than MaxRestores restic dumps run at once (N-03).
func TestConcurrentRestoresAreLimited(t *testing.T) {
	log := restictest.Install(t, "current")
	loc, _ := localRepo(t)
	const slots = 3
	rt := &Runtime{TempDir: t.TempDir(), MaxRestores: slots}
	t.Cleanup(rt.Wait)
	if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	data := pattern(1000)
	snap, err := archive(t, rt, loc, data)
	if err != nil {
		t.Fatal(err)
	}
	active := t.TempDir()
	t.Setenv(restictest.EnvDumpActive, active)
	t.Setenv(restictest.EnvDumpDelay, "100ms")

	stop := make(chan struct{})
	peak := make(chan int, 1)
	go func() {
		most := 0
		for {
			most = max(most, restictest.ActiveDumps(active))
			select {
			case <-stop:
				peak <- most
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	const ranges = 64
	var wg sync.WaitGroup
	errs := make(chan error, ranges)
	for i := range ranges {
		wg.Go(func() {
			r, err := rt.Open(t.Context(), loc, snap, int64(len(data)))
			if err != nil {
				errs <- err
				return
			}
			defer r.Close()
			if _, err := r.Seek(int64(i), io.SeekStart); err != nil {
				errs <- err
				return
			}
			if err := r.PrepareRange(int64(i), 1); err != nil {
				errs <- err
				return
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != data[i:i+1] {
				errs <- errors.New("wrong range content")
			}
		})
	}
	wg.Wait()
	close(stop)
	most := <-peak
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if most > slots {
		t.Fatalf("%d restic dumps ran at once, limit %d", most, slots)
	}
	if most == 0 {
		t.Fatal("the probe saw no dump")
	}
	if n := restictest.Calls(t, log, "dump"); n != ranges {
		t.Fatalf("%d dumps for %d distinct ranges", n, ranges)
	}
}

// A restore that finds every slot taken waits for one only as long as its
// request lasts, then fails with the restore limit (503); a reader joining
// a running restore needs no slot (N-03).
func TestRestoreSlotWaitEndsWithTheRequest(t *testing.T) {
	log := restictest.Install(t, "current")
	loc, _ := localRepo(t)
	rt := &Runtime{TempDir: t.TempDir(), MaxRestores: 1}
	t.Cleanup(rt.Wait)
	if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	busy, err := archive(t, rt, loc, "aaaa")
	if err != nil {
		t.Fatal(err)
	}
	other, err := archive(t, rt, loc, "bbbb")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(restictest.EnvDumpDelay, "10s") // until canceled below
	busyCtx, cancelBusy := context.WithCancel(t.Context())
	defer cancelBusy()
	a, err := rt.Open(busyCtx, loc, busy, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	prepared := make(chan error, 1)
	go func() { prepared <- a.Prepare() }()
	waitFor(t, "the first dump", func() bool { return restictest.Calls(t, log, "dump") == 1 })

	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	b, err := rt.Open(ctx, loc, other, 4)
	if err != nil {
		t.Fatal(err)
	}
	err = b.Prepare()
	_ = b.Close()
	if !errors.Is(err, ErrRestoreSpoolLimit) || !errors.Is(err, storage.ErrResourceLimit) {
		t.Fatalf("restore without a free slot: %v", err)
	}
	// A reader of the running restore shares it instead of waiting.
	joinCtx, cancelJoin := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancelJoin()
	c, err := rt.Open(joinCtx, loc, busy, 4)
	if err != nil {
		t.Fatal(err)
	}
	err = c.Prepare()
	_ = c.Close()
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRestoreSpoolLimit) {
		t.Fatalf("reader joining a running restore: %v", err)
	}
	// A request without a deadline waits for at most restoreSlotWait.
	defer func(wait time.Duration) { restoreSlotWait = wait }(restoreSlotWait)
	restoreSlotWait = 200 * time.Millisecond
	d, err := rt.Open(t.Context(), loc, other, 4)
	if err != nil {
		t.Fatal(err)
	}
	err = d.Prepare()
	_ = d.Close()
	if !errors.Is(err, ErrRestoreSpoolLimit) || t.Context().Err() != nil {
		t.Fatalf("restore waiting past restoreSlotWait: %v", err)
	}
	if n := restictest.Calls(t, log, "dump"); n != 1 {
		t.Fatalf("%d dumps ran while the only slot was taken", n)
	}
	cancelBusy()
	<-prepared
}

// A reader that stalls on a large object holds at most half the restore
// budget, so other objects stay readable (N-03).
func TestStalledReaderLeavesRestoreBudget(t *testing.T) {
	restictest.Install(t, "current")
	loc, _ := localRepo(t)
	limit := 4*crypt.EncryptedSize(crypt.ChunkSize) + 1
	rt := &Runtime{TempDir: t.TempDir(), MaxRestoreSpoolBytes: limit}
	t.Cleanup(rt.Wait)
	if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	data := pattern(3 * crypt.ChunkSize)
	first, err := archive(t, rt, loc, data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := archive(t, rt, loc, data[1:]+"x")
	if err != nil {
		t.Fatal(err)
	}
	stalled, err := rt.Open(t.Context(), loc, first, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	buf := make([]byte, 10)
	if _, err := io.ReadFull(stalled, buf); err != nil || string(buf) != data[:10] {
		t.Fatalf("stalled reader: %q, %v", buf, err)
	}
	if got := readAll(t, rt, loc, second, int64(len(data))); got != data[1:]+"x" {
		t.Fatal("second object differs")
	}
	if rt.spoolBytes > limit/2 {
		t.Fatalf("one reader reserves %d of %d bytes", rt.spoolBytes, limit)
	}
	if err := stalled.Close(); err != nil {
		t.Fatal(err)
	}
	if rt.spoolBytes != 0 || len(tempFiles(t, rt.TempDir)) != 0 {
		t.Fatalf("spools not released: %d bytes, %v", rt.spoolBytes, tempFiles(t, rt.TempDir))
	}
}

// An object larger than the restore budget is read in windows, each at
// most half the budget: sequentially, at random offsets and as a range
// spanning several windows (N-03).
func TestObjectsLargerThanTheSpoolLimitAreReadInWindows(t *testing.T) {
	log := restictest.Install(t, "current")
	loc, _ := localRepo(t)
	limit := 4*crypt.EncryptedSize(crypt.ChunkSize) + 1
	rt := &Runtime{TempDir: t.TempDir(), MaxRestoreSpoolBytes: limit}
	t.Cleanup(rt.Wait)
	if err := rt.Prepare(t.Context(), "b1", loc, &memRecords{}); err != nil {
		t.Fatal(err)
	}
	const chunk = crypt.ChunkSize
	data := pattern(5*chunk + 123)
	if crypt.EncryptedSize(int64(len(data))) <= limit {
		t.Fatal("the object must exceed the limit")
	}
	snap, err := archive(t, rt, loc, data)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(data))
	checkReserved := func(what string) {
		t.Helper()
		if rt.spoolBytes > limit/2 {
			t.Fatalf("%s: one reader reserves %d of %d bytes", what, rt.spoolBytes, limit)
		}
	}

	r, err := rt.Open(t.Context(), loc, snap, size)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	checkReserved("prepare")
	got, err := io.ReadAll(r)
	if err != nil || string(got) != data {
		t.Fatalf("sequential read: %d bytes, %v", len(got), err)
	}
	checkReserved("sequential read")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = rt.Open(t.Context(), loc, snap, size)
	if err != nil {
		t.Fatal(err)
	}
	for _, rd := range []struct{ off, n int64 }{
		{chunk + 7, 3 * chunk}, // across three windows
		{0, 10},                // backwards
		{2*chunk - 1, 2},       // a window boundary
		{4 * chunk, chunk + 123},
		{5 * chunk, 200}, // past the end
		{3 * chunk, 1},
	} {
		buf := make([]byte, rd.n)
		n, err := r.ReadAt(buf, rd.off)
		end := min(rd.off+rd.n, size)
		if string(buf[:n]) != data[rd.off:end] {
			t.Fatalf("ReadAt(%d, %d): %d bytes differ", rd.off, rd.n, n)
		}
		if end < rd.off+rd.n && err != io.EOF || end == rd.off+rd.n && err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", rd.off, rd.n, err)
		}
		checkReserved("ReadAt")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	r, err = rt.Open(t.Context(), loc, snap, size)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := r.PrepareRange(100, 4*chunk); err != nil {
		t.Fatalf("prepare range: %v", err)
	}
	got, err = io.ReadAll(r)
	if err != nil || string(got) != data[100:100+4*chunk] {
		t.Fatalf("range read: %d bytes, %v", len(got), err)
	}
	checkReserved("range read")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if rt.spoolBytes != 0 || len(tempFiles(t, rt.TempDir)) != 0 {
		t.Fatalf("spools not released: %d bytes, %v", rt.spoolBytes, tempFiles(t, rt.TempDir))
	}
	if n := restictest.Calls(t, log, "dump"); n < 3 {
		t.Fatalf("%d dumps: the object was not restored in windows", n)
	}
}
