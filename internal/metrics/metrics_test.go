package metrics

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 2, 3, 4, 30, 0, time.UTC)}
}

func TestSnapshotCounters(t *testing.T) {
	c := newClock()
	r := newRegistry(c.now)
	for _, tc := range []struct {
		op      string
		status  int
		in, out int64
		lat     time.Duration
	}{
		{"GetObject", 200, 0, 100, 10 * time.Millisecond},
		{"GetObject", 404, 0, 50, 20 * time.Millisecond},
		{"PutObject", 200, 1000, 0, 30 * time.Millisecond},
		{"PutObject", 503, 10, 0, 40 * time.Millisecond},
		{"PutObject", 0, -5, -5, time.Millisecond}, // 0 = 200, negatives clamp
	} {
		done := r.Begin()
		if got := r.Snapshot().InFlight; got != 1 {
			t.Fatalf("in flight = %d", got)
		}
		c.add(tc.lat)
		done(tc.op, tc.status, tc.in, tc.out)
		done(tc.op, tc.status, tc.in, tc.out) // second call ignored
	}
	s := r.Snapshot()
	if s.Requests != 5 || s.Errors4xx != 1 || s.Errors5xx != 1 || s.BytesIn != 1010 || s.BytesOut != 150 || s.InFlight != 0 {
		t.Fatalf("totals: %+v", s)
	}
	if len(s.Ops) != 2 || s.Ops[0].Op != "GetObject" || s.Ops[1].Op != "PutObject" {
		t.Fatalf("ops: %+v", s.Ops)
	}
	g := s.Ops[0]
	if g.Requests != 2 || g.Errors4xx != 1 || g.BytesOut != 150 || g.AvgLatencyMs != 15 || g.P50LatencyMs != 10 || g.P99LatencyMs != 20 {
		t.Fatalf("GetObject: %+v", g)
	}
	p := s.Ops[1]
	if p.Requests != 3 || p.Errors5xx != 1 || p.BytesIn != 1010 || p.P50LatencyMs != 30 {
		t.Fatalf("PutObject: %+v", p)
	}
	if s.UptimeSeconds != 0 || !s.StartedAt.Equal(time.Date(2026, 1, 2, 3, 4, 30, 0, time.UTC)) {
		t.Fatalf("uptime %d started %v", s.UptimeSeconds, s.StartedAt)
	}
}

func TestRecentMinutes(t *testing.T) {
	c := newClock()
	r := newRegistry(c.now)
	rec := func(status int, in, out int64) { r.Begin()("Op", status, in, out) }
	rec(200, 1, 2)
	rec(500, 0, 0)
	c.add(time.Minute)
	rec(404, 3, 4)
	s := r.Snapshot()
	if len(s.Recent) != 60 {
		t.Fatalf("recent len %d", len(s.Recent))
	}
	last, prev := s.Recent[59], s.Recent[58]
	if last.Requests != 1 || last.Errors != 1 || last.BytesIn != 3 || last.BytesOut != 4 {
		t.Fatalf("last %+v", last)
	}
	if prev.Requests != 2 || prev.Errors != 1 || prev.BytesIn != 1 {
		t.Fatalf("prev %+v", prev)
	}
	if !last.Minute.Equal(time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)) || last.Minute.Sub(prev.Minute) != time.Minute {
		t.Fatalf("minutes %v %v", prev.Minute, last.Minute)
	}
	// An hour later the old slots are stale and must not be reported, even
	// though the ring position is reused.
	c.add(60 * time.Minute)
	rec(200, 0, 0)
	s = r.Snapshot()
	var total int64
	for _, b := range s.Recent {
		total += b.Requests
	}
	if total != 1 || s.Recent[59].Requests != 1 || s.Requests != 4 {
		t.Fatalf("after an hour: total %d last %+v", total, s.Recent[59])
	}
	if s.UptimeSeconds != 61*60 {
		t.Fatalf("uptime %d", s.UptimeSeconds)
	}
}

func TestReservoirBounded(t *testing.T) {
	c := newClock()
	r := newRegistry(c.now)
	// 2000 slow requests, then reservoirSize fast ones: only the recent
	// (fast) samples remain in the reservoir.
	for i := 0; i < 2000; i++ {
		done := r.Begin()
		c.add(time.Second)
		done("Op", 200, 0, 0)
	}
	for i := 0; i < reservoirSize; i++ {
		done := r.Begin()
		c.add(time.Millisecond)
		done("Op", 200, 0, 0)
	}
	o := r.Snapshot().Ops[0]
	if o.P99LatencyMs != 1 || o.P50LatencyMs != 1 {
		t.Fatalf("percentiles %+v", o)
	}
	if o.Requests != 2000+reservoirSize {
		t.Fatalf("requests %d", o.Requests)
	}
}

func TestOpCardinalityBounded(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < maxOps+50; i++ {
		r.Begin()(fmt.Sprintf("op%d", i), 200, 0, 0)
	}
	r.Begin()("", 200, 0, 0)
	s := r.Snapshot()
	if len(s.Ops) > maxOps+1 {
		t.Fatalf("ops not bounded: %d", len(s.Ops))
	}
	var other int64
	for _, o := range s.Ops {
		if o.Op == OtherOp {
			other = o.Requests
		}
	}
	if other < 50 {
		t.Fatalf("other = %d", other)
	}
}

func TestNilRegistry(t *testing.T) {
	var r *Registry
	r.Begin()("x", 200, 1, 1)
	if s := r.Snapshot(); s.Requests != 0 {
		t.Fatal(s)
	}
	var b bytes.Buffer
	if err := r.WritePrometheus(&b, nil); err != nil || !strings.Contains(b.String(), "gos3_inflight_requests 0") {
		t.Fatalf("%v %s", err, b.String())
	}
}

func TestWritePrometheus(t *testing.T) {
	c := newClock()
	r := newRegistry(c.now)
	done := r.Begin()
	c.add(20 * time.Millisecond)
	done("GetObject", 200, 5, 100)
	done = r.Begin()
	c.add(2 * time.Second)
	done("GetObject", 404, 0, 0)
	r.Begin()(`we"ird\op`+"\n", 500, 0, 0)
	inflight := r.Begin()
	defer inflight("x", 200, 0, 0)
	c.add(10 * time.Second)

	var b bytes.Buffer
	if err := r.WritePrometheus(&b, map[string]float64{"buckets": 3, "objects total": 12.5, "gos3_custom": 1, "  ": 9, "uptime_seconds": 99}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE gos3_requests_total counter\n",
		`gos3_requests_total{op="GetObject",code_class="2xx"} 1` + "\n",
		`gos3_requests_total{op="GetObject",code_class="4xx"} 1` + "\n",
		`gos3_requests_total{op="we\"ird\\op\n",code_class="5xx"} 1` + "\n",
		"# TYPE gos3_request_duration_seconds histogram\n",
		`gos3_request_duration_seconds_bucket{op="GetObject",le="0.01"} 0` + "\n",
		`gos3_request_duration_seconds_bucket{op="GetObject",le="0.025"} 1` + "\n",
		`gos3_request_duration_seconds_bucket{op="GetObject",le="2.5"} 2` + "\n",
		`gos3_request_duration_seconds_bucket{op="GetObject",le="+Inf"} 2` + "\n",
		`gos3_request_duration_seconds_sum{op="GetObject"} 2.02` + "\n",
		`gos3_request_duration_seconds_count{op="GetObject"} 2` + "\n",
		`gos3_bytes_in_total{op="GetObject"} 5` + "\n",
		`gos3_bytes_out_total{op="GetObject"} 100` + "\n",
		"gos3_inflight_requests 1\n",
		"gos3_uptime_seconds 12.02\n",
		"# TYPE gos3_buckets gauge\ngos3_buckets 3\n",
		"gos3_objects_total 12.5\n",
		"gos3_custom 1\n",
		"gos3_start_time_seconds 1.76732307e+09\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, " 99\n") || strings.Contains(out, " 9\n") {
		t.Errorf("colliding/empty extra gauge emitted:\n%s", out)
	}
	// Every non-comment line is "name{labels} value" or "name value".
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "gos3_") || strings.Count(line, " ") < 1 {
			t.Errorf("bad line %q", line)
		}
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestWritePrometheusError(t *testing.T) {
	r := NewRegistry()
	r.Begin()("Op", 200, 0, 0)
	if err := r.WritePrometheus(failWriter{}, nil); err == nil {
		t.Fatal("write error not reported")
	}
}

func TestMetricName(t *testing.T) {
	for in, want := range map[string]string{
		"buckets":     "gos3_buckets",
		"gos3_x":      "gos3_x",
		"a-b.c d":     "gos3_a_b_c_d",
		"ünï":         "gos3__n_",
		"":            "",
		" ns:sub_x9 ": "gos3_ns:sub_x9",
	} {
		if got := MetricName(in); got != want {
			t.Errorf("MetricName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConcurrent(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				done := r.Begin()
				done(fmt.Sprintf("op%d", i%7), 200+(i%4)*100, 1, 2)
				if i%100 == 0 {
					_ = r.Snapshot()
					_ = r.WritePrometheus(&bytes.Buffer{}, nil)
				}
			}
		}(g)
	}
	wg.Wait()
	s := r.Snapshot()
	if s.Requests != 16*500 || s.BytesIn != 16*500 || s.BytesOut != 2*16*500 || s.InFlight != 0 {
		t.Fatalf("%+v", s)
	}
	var sum int64
	for _, o := range s.Ops {
		sum += o.Requests
	}
	if sum != s.Requests {
		t.Fatalf("op sum %d != %d", sum, s.Requests)
	}
}

// TestClientAbortedNotAnError: status 499 (client closed request, recorded
// by the S3 layer when an upload is cut off by the client) is counted in its
// own "aborted" class, never as a 4xx error, in the snapshot totals, per op,
// per minute and in the Prometheus output.
func TestClientAbortedNotAnError(t *testing.T) {
	c := newClock()
	r := newRegistry(c.now)
	r.Begin()("PutObject", StatusClientClosedRequest, 10, 0)
	r.Begin()("PutObject", StatusClientClosedRequest, 20, 0)
	r.Begin()("PutObject", 400, 0, 0)
	r.Begin()("GetObject", 200, 0, 5)

	s := r.Snapshot()
	if s.Requests != 4 || s.Errors4xx != 1 || s.Errors5xx != 0 || s.Aborted != 2 || s.BytesIn != 30 {
		t.Fatalf("totals: %+v", s)
	}
	if len(s.Ops) != 2 {
		t.Fatalf("ops: %+v", s.Ops)
	}
	if g := s.Ops[0]; g.Op != "GetObject" || g.Aborted != 0 || g.Errors4xx != 0 {
		t.Fatalf("GetObject: %+v", g)
	}
	if p := s.Ops[1]; p.Op != "PutObject" || p.Requests != 3 || p.Errors4xx != 1 || p.Aborted != 2 {
		t.Fatalf("PutObject: %+v", p)
	}
	if last := s.Recent[59]; last.Requests != 4 || last.Errors != 1 || last.Aborted != 2 {
		t.Fatalf("minute: %+v", last)
	}

	var b bytes.Buffer
	if err := r.WritePrometheus(&b, nil); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`gos3_requests_total{op="PutObject",code_class="aborted"} 2` + "\n",
		`gos3_requests_total{op="PutObject",code_class="4xx"} 1` + "\n",
		`gos3_requests_total{op="GetObject",code_class="2xx"} 1` + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `code_class="4xx"} 3`) || strings.Contains(out, `code_class="4xx"} 2`) {
		t.Errorf("aborted requests counted as 4xx:\n%s", out)
	}

	// The new fields are additive: the existing JSON keys are unchanged and
	// "aborted" appears alongside them.
	js, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"errors4xx":1`, `"errors5xx":0`, `"aborted":2`, `"requests":4`} {
		if !strings.Contains(string(js), key) {
			t.Errorf("snapshot JSON missing %s: %s", key, js)
		}
	}
}
