// Package metrics collects request metrics for the S3 and admin APIs and
// renders them as a JSON snapshot (admin API / UI dashboard) and in the
// Prometheus text exposition format (GET /_gos3/metrics).
//
// CONTRACT: exported names/signatures are fixed. Implementation is owned by
// the s3-core work item (replace the stub bodies, add files freely).
package metrics

import (
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// OpStats aggregates one API operation (e.g. "PutObject").
type OpStats struct {
	Op           string  `json:"op"`
	Requests     int64   `json:"requests"`
	Errors4xx    int64   `json:"errors4xx"`
	Errors5xx    int64   `json:"errors5xx"`
	BytesIn      int64   `json:"bytesIn"`
	BytesOut     int64   `json:"bytesOut"`
	AvgLatencyMs float64 `json:"avgLatencyMs"`
	P50LatencyMs float64 `json:"p50LatencyMs"`
	P99LatencyMs float64 `json:"p99LatencyMs"`
}

// Snapshot is a point-in-time view of all metrics.
type Snapshot struct {
	StartedAt     time.Time `json:"startedAt"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
	Requests      int64     `json:"requests"`
	Errors4xx     int64     `json:"errors4xx"`
	Errors5xx     int64     `json:"errors5xx"`
	BytesIn       int64     `json:"bytesIn"`
	BytesOut      int64     `json:"bytesOut"`
	InFlight      int64     `json:"inFlight"`
	Ops           []OpStats `json:"ops"` // sorted by Op
	// Recent is per-minute request counts for the last 60 minutes, oldest first.
	Recent []MinuteBucket `json:"recent"`
}

// MinuteBucket is one minute of traffic.
type MinuteBucket struct {
	Minute   time.Time `json:"minute"`
	Requests int64     `json:"requests"`
	Errors   int64     `json:"errors"`
	BytesIn  int64     `json:"bytesIn"`
	BytesOut int64     `json:"bytesOut"`
}

const (
	// reservoirSize bounds the per-operation latency samples kept for the
	// p50/p99 estimates (the most recent samples win).
	reservoirSize = 1024
	// maxOps bounds the number of distinct operation names tracked; further
	// names are folded into OtherOp so hostile or buggy callers cannot grow
	// memory without bound.
	maxOps = 256
	// recentMinutes is the length of the per-minute ring.
	recentMinutes = 60
	// OtherOp is the operation name used once maxOps distinct names exist,
	// and for empty operation names.
	OtherOp = "Other"
)

// durationBuckets are the upper bounds (seconds) of the Prometheus request
// duration histogram.
var durationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// opStats is the mutable per-operation state, guarded by its own mutex.
type opStats struct {
	mu        sync.Mutex
	requests  int64
	classes   [6]int64 // index 1..5 = 1xx..5xx; 0 = anything else
	bytesIn   int64
	bytesOut  int64
	latSum    float64                         // seconds
	hist      [len(durationBuckets) + 1]int64 // per-bucket counts; last = +Inf
	reservoir [reservoirSize]float64          // seconds, ring buffer
	resLen    int
	resNext   int
}

// minuteSlot is one slot of the per-minute ring.
type minuteSlot struct {
	minute   int64 // unix minute this slot currently holds
	requests int64
	errors   int64
	bytesIn  int64
	bytesOut int64
}

// Registry is safe for concurrent use.
type Registry struct {
	started time.Time
	now     func() time.Time

	requests atomic.Int64
	errors4  atomic.Int64
	errors5  atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
	inFlight atomic.Int64

	opsMu sync.RWMutex
	ops   map[string]*opStats

	minMu   sync.Mutex
	minutes [recentMinutes]minuteSlot
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return newRegistry(time.Now) }

func newRegistry(now func() time.Time) *Registry {
	return &Registry{started: now(), now: now, ops: make(map[string]*opStats)}
}

// Begin marks a request in flight; call the returned func exactly once when
// it finishes. Extra calls of the returned func are ignored. op is the
// operation name (e.g. "GetObject"), status the HTTP status code sent, and
// bytesIn/bytesOut the request/response body sizes.
func (r *Registry) Begin() (done func(op string, status int, bytesIn, bytesOut int64)) {
	if r == nil {
		return func(string, int, int64, int64) {}
	}
	start := r.now()
	r.inFlight.Add(1)
	var once atomic.Bool
	return func(op string, status int, bytesIn, bytesOut int64) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		r.inFlight.Add(-1)
		r.record(op, status, bytesIn, bytesOut, r.now().Sub(start))
	}
}

// record adds one finished request.
func (r *Registry) record(op string, status int, bytesIn, bytesOut int64, d time.Duration) {
	if bytesIn < 0 {
		bytesIn = 0
	}
	if bytesOut < 0 {
		bytesOut = 0
	}
	if d < 0 {
		d = 0
	}
	class := statusClass(status)
	r.requests.Add(1)
	r.bytesIn.Add(bytesIn)
	r.bytesOut.Add(bytesOut)
	switch class {
	case 4:
		r.errors4.Add(1)
	case 5:
		r.errors5.Add(1)
	}

	secs := d.Seconds()
	st := r.opStats(op)
	st.mu.Lock()
	st.requests++
	st.classes[class]++
	st.bytesIn += bytesIn
	st.bytesOut += bytesOut
	st.latSum += secs
	st.hist[bucketIndex(secs)]++
	st.reservoir[st.resNext] = secs
	st.resNext = (st.resNext + 1) % reservoirSize
	if st.resLen < reservoirSize {
		st.resLen++
	}
	st.mu.Unlock()

	m := r.now().Unix() / 60
	r.minMu.Lock()
	slot := &r.minutes[m%recentMinutes]
	if slot.minute != m {
		*slot = minuteSlot{minute: m}
	}
	slot.requests++
	if class >= 4 {
		slot.errors++
	}
	slot.bytesIn += bytesIn
	slot.bytesOut += bytesOut
	r.minMu.Unlock()
}

// opStats returns (creating if needed) the stats for op.
func (r *Registry) opStats(op string) *opStats {
	if op == "" {
		op = OtherOp
	}
	r.opsMu.RLock()
	st, ok := r.ops[op]
	r.opsMu.RUnlock()
	if ok {
		return st
	}
	r.opsMu.Lock()
	defer r.opsMu.Unlock()
	if st, ok = r.ops[op]; ok {
		return st
	}
	if len(r.ops) >= maxOps {
		op = OtherOp
		if st, ok = r.ops[op]; ok {
			return st
		}
	}
	st = &opStats{}
	r.ops[op] = st
	return st
}

// statusClass returns 1..5 for 1xx..5xx and 0 otherwise.
func statusClass(status int) int {
	if status == 0 {
		status = 200 // nothing written means net/http sends 200
	}
	if status < 100 || status > 599 {
		return 0
	}
	return status / 100
}

// bucketIndex returns the histogram slot for secs (len(durationBuckets) is
// the +Inf slot).
func bucketIndex(secs float64) int {
	return sort.SearchFloat64s(durationBuckets[:], secs)
}

// Snapshot returns current metrics.
func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{Ops: []OpStats{}, Recent: []MinuteBucket{}}
	}
	now := r.now()
	s := Snapshot{
		StartedAt:     r.started,
		UptimeSeconds: int64(now.Sub(r.started) / time.Second),
		Requests:      r.requests.Load(),
		Errors4xx:     r.errors4.Load(),
		Errors5xx:     r.errors5.Load(),
		BytesIn:       r.bytesIn.Load(),
		BytesOut:      r.bytesOut.Load(),
		InFlight:      r.inFlight.Load(),
	}
	if s.UptimeSeconds < 0 {
		s.UptimeSeconds = 0
	}
	s.Ops = make([]OpStats, 0, 32)
	for _, e := range r.sortedOps() {
		e.st.mu.Lock()
		o := OpStats{
			Op:        e.name,
			Requests:  e.st.requests,
			Errors4xx: e.st.classes[4],
			Errors5xx: e.st.classes[5],
			BytesIn:   e.st.bytesIn,
			BytesOut:  e.st.bytesOut,
		}
		if e.st.requests > 0 {
			o.AvgLatencyMs = e.st.latSum / float64(e.st.requests) * 1000
		}
		samples := make([]float64, e.st.resLen)
		copy(samples, e.st.reservoir[:e.st.resLen])
		e.st.mu.Unlock()
		sort.Float64s(samples)
		o.P50LatencyMs = percentile(samples, 0.50) * 1000
		o.P99LatencyMs = percentile(samples, 0.99) * 1000
		s.Ops = append(s.Ops, o)
	}

	cur := now.Unix() / 60
	s.Recent = make([]MinuteBucket, 0, recentMinutes)
	r.minMu.Lock()
	for m := cur - recentMinutes + 1; m <= cur; m++ {
		b := MinuteBucket{Minute: time.Unix(m*60, 0).UTC()}
		if slot := r.minutes[mod(m, recentMinutes)]; slot.minute == m {
			b.Requests, b.Errors, b.BytesIn, b.BytesOut = slot.requests, slot.errors, slot.bytesIn, slot.bytesOut
		}
		s.Recent = append(s.Recent, b)
	}
	r.minMu.Unlock()
	return s
}

func mod(a, n int64) int64 {
	m := a % n
	if m < 0 {
		m += n
	}
	return m
}

// percentile returns the nearest-rank percentile p (0..1) of sorted samples.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p*float64(len(sorted))+0.999999999) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

type namedOp struct {
	name string
	st   *opStats
}

func (r *Registry) sortedOps() []namedOp {
	r.opsMu.RLock()
	out := make([]namedOp, 0, len(r.ops))
	for name, st := range r.ops {
		out = append(out, namedOp{name, st})
	}
	r.opsMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// WritePrometheus writes metrics in Prometheus text format (metric prefix
// "gos3_"). Extra gauges (e.g. bucket counts) may be supplied by the caller;
// their names are sanitized and prefixed with "gos3_" unless they already
// carry it.
func (r *Registry) WritePrometheus(w io.Writer, extra map[string]float64) error {
	return r.writePrometheus(w, extra)
}
