package copyjob

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
)

// Status is a copy or mirror job's progress report (CopyStatus of the admin
// API, Job.Status).
//
// KeysScanned, BytesDone, Conflicts and ConflictSamples describe the
// current (or last) full pass and restart with each one. Incremental runs
// only update lifetime activity and unresolved work.
type Status struct {
	// PassComplete and TotalsKnown qualify full-pass progress. Incremental
	// work changes lifetime counters only; a comparison is not proof that
	// all keys copied successfully.
	PassComplete   bool  `json:"passComplete"`
	TotalsKnown    bool  `json:"totalsKnown"`
	SourceKeysDone int64 `json:"sourceKeysDone"`
	// Unresolved counts distinct keys with an unresolved error or conflict.
	// CheckRequired means a full comparison is pending or incomplete.
	Unresolved    int64 `json:"unresolved"`
	CheckRequired bool  `json:"checkRequired"`
	// KeysScanned counts the keys compared; BytesDone the source bytes
	// they hold (of the versions copied), for the progress against
	// TotalBytes.
	KeysScanned int64 `json:"keysScanned"`
	// KeysCopied counts the keys the job wrote to (copied versions or
	// updated metadata); VersionsCopied the versions and delete markers it
	// wrote and BytesCopied their content bytes.
	KeysCopied     int64 `json:"keysCopied"`
	VersionsCopied int64 `json:"versionsCopied"`
	BytesCopied    int64 `json:"bytesCopied"`
	BytesDone      int64 `json:"bytesDone"`
	// KeysDeleted counts the target keys removed because the source no
	// longer has them.
	KeysDeleted int64 `json:"keysDeleted"`
	// Conflicts counts what the job could not do and will not retry by
	// itself (a locked target version it must not change, a key it cannot
	// map); ConflictSamples are the last 20.
	Conflicts       int64            `json:"conflicts"`
	ConflictSamples []ConflictSample `json:"conflictSamples,omitempty"`
	// TotalKeys and TotalBytes measure the source at the start of this full
	// pass (the versions it copies); TotalsKnown qualifies the estimate.
	TotalKeys  int64 `json:"totalKeys"`
	TotalBytes int64 `json:"totalBytes"`
	// BytesPerSecond is the copy rate (exponentially weighted over a
	// minute); ETASeconds the estimated time left in the current pass.
	BytesPerSecond int64 `json:"bytesPerSecond"`
	ETASeconds     int64 `json:"etaSeconds"`
	// Mirror: Pending source keys queued or in flight (live in the admin
	// API) and LagSeconds the age of the oldest; LastPassAt and
	// NextPassAt the last and next full comparison; Passes the number of
	// full passes done (a copy's too).
	Pending    int64     `json:"pending"`
	LagSeconds int64     `json:"lagSeconds"`
	LastPassAt time.Time `json:"lastPassAt,omitzero"`
	NextPassAt time.Time `json:"nextPassAt,omitzero"`
	Passes     int64     `json:"passes"`
	// Errors are the last 20 keys that could not be copied.
	Errors []KeyError `json:"errors,omitempty"`
	// Mode is how keys are compared: "identity" or "fingerprint" (see the
	// package documentation).
	Mode string `json:"mode,omitempty"`
	// Warnings are things the job skipped on purpose, such as a bucket
	// policy that cannot be rewritten for the target.
	Warnings []string `json:"warnings,omitempty"`
}

// ConflictSample is one conflict of Status.ConflictSamples.
type ConflictSample struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// KeyError is one entry of Status.Errors.
type KeyError struct {
	Key   string    `json:"key"`
	Error string    `json:"error"`
	At    time.Time `json:"at"`
}

// Modes of Status.Mode.
const (
	modeIdentity    = "identity"
	modeFingerprint = "fingerprint"
)

const (
	maxSamples = 20
	// rateWindow is the time constant of the copy rate's average.
	rateWindow = time.Minute
)

// tracker accumulates a run's progress. Safe for concurrent use.
type tracker struct {
	mu sync.Mutex
	st Status

	// Rate averaging: copied (BytesCopied) and done (BytesDone) rates,
	// exponentially weighted over rateWindow and corrected for their
	// start (the weight of the averages so far).
	start, last          time.Time
	lastCopied, lastDone int64
	copyRate, doneRate   float64
	weight               float64
}

func newTracker(st Status) *tracker {
	return &tracker{st: st}
}

// snapshot returns a copy of the status.
func (t *tracker) snapshot() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.st
	st.ConflictSamples = append([]ConflictSample(nil), st.ConflictSamples...)
	st.Errors = append([]KeyError(nil), st.Errors...)
	st.Warnings = append([]string(nil), st.Warnings...)
	return st
}

// update changes the status under the lock.
func (t *tracker) update(f func(*Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f(&t.st)
}

// startPass resets the counters of a full pass.
func (t *tracker) startPass() {
	t.update(func(st *Status) {
		st.KeysScanned, st.BytesDone, st.Conflicts, st.ConflictSamples = 0, 0, 0, nil
		st.SourceKeysDone, st.PassComplete = 0, false
		st.TotalsKnown = false
		st.ETASeconds = 0
	})
	t.mu.Lock()
	t.lastDone = 0
	t.mu.Unlock()
}

// record adds the outcome of synchronising pair p (as listed before the
// sync) with result res.
func (t *tracker) record(p copier.KeyPair, all bool, res copier.Result, full bool) {
	var bytes int64
	for _, v := range p.Src {
		if !v.DeleteMarker {
			bytes += v.Size
		}
		if !all {
			break
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := &t.st
	if full {
		st.KeysScanned++
		st.BytesDone += bytes
		if hasSource(p, all) {
			st.SourceKeysDone++
		}
		if p.Err != nil || st.BytesDone > st.TotalBytes || st.SourceKeysDone > st.TotalKeys {
			st.TotalsKnown = false
		}
	}
	if res.VersionsCopied > 0 || res.MetaUpdated > 0 {
		st.KeysCopied++
	}
	st.VersionsCopied += res.VersionsCopied
	st.BytesCopied += res.BytesCopied
	if res.VersionsDeleted > 0 && !hasSource(p, all) {
		st.KeysDeleted++
	}
	for _, c := range res.Conflicts {
		if full {
			st.Conflicts++
		}
		reason := c.Reason
		if c.VersionID != "" {
			reason = "target version " + c.VersionID + ": " + reason
		}
		if full {
			st.ConflictSamples = appendLast(st.ConflictSamples, ConflictSample{Key: c.Key, Reason: reason})
		}
	}
}

// keyError records an error of one key.
func (t *tracker) keyError(key string, err error, at time.Time) {
	t.update(func(st *Status) {
		st.Errors = appendLast(st.Errors, KeyError{Key: key, Error: err.Error(), At: at.UTC()})
	})
}

// tick updates the rates and the ETA at time now.
func (t *tracker) tick(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := &t.st
	if t.start.IsZero() {
		t.start, t.last = now, now
		t.lastCopied, t.lastDone = st.BytesCopied, st.BytesDone
		return
	}
	dt := now.Sub(t.last)
	if dt <= 0 {
		return
	}
	alpha := 1 - math.Exp(-float64(dt)/float64(rateWindow))
	sec := dt.Seconds()
	t.copyRate += alpha * (float64(st.BytesCopied-t.lastCopied)/sec - t.copyRate)
	done := max(st.BytesDone-t.lastDone, 0)
	t.doneRate += alpha * (float64(done)/sec - t.doneRate)
	t.weight += alpha * (1 - t.weight)
	t.last, t.lastCopied, t.lastDone = now, st.BytesCopied, st.BytesDone
	copyRate, doneRate := t.copyRate/t.weight, t.doneRate/t.weight
	st.BytesPerSecond = int64(math.Round(copyRate))
	st.ETASeconds = 0
	if left := st.TotalBytes - st.BytesDone; st.TotalsKnown && left > 0 && doneRate > 0 {
		st.ETASeconds = int64(math.Ceil(float64(left) / doneRate))
	}
}

// idle clears the rates (a mirror waiting for changes copies nothing).
func (t *tracker) idle() {
	t.update(func(st *Status) { st.BytesPerSecond, st.ETASeconds = 0, 0 })
}

// appendLast appends v to s, keeping the last maxSamples entries.
func appendLast[T any](s []T, v T) []T {
	s = append(s, v)
	if n := len(s) - maxSamples; n > 0 {
		s = append(s[:0:0], s[n:]...)
	}
	return s
}

// hasSource reports whether the source has the key of p as the copy sees
// it: some version (every version is copied) or a current version.
func hasSource(p copier.KeyPair, all bool) bool {
	if all {
		return len(p.Src) > 0
	}
	return len(p.Src) > 0 && !p.Src[0].DeleteMarker
}

// measure returns the number of keys under the source prefix and the
// bytes of the versions the job copies, for the progress report.
func measure(ctx context.Context, r *run) (keys, bytes int64, err error) {
	sp := &r.sp
	last := ""
	fn := func(v storage.ObjectInfo) error {
		if v.Key != last {
			keys++
			last = v.Key
		}
		if !v.DeleteMarker {
			bytes += v.Size
		}
		return nil
	}
	if sp.all() {
		err = r.s.router.WalkVersions(ctx, sp.Source.Bucket, sp.Source.Prefix, "", "", fn)
	} else {
		err = r.s.router.WalkObjects(ctx, sp.Source.Bucket, sp.Source.Prefix, "", fn)
	}
	return keys, bytes, err
}
