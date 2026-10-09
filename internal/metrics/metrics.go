// Package metrics collects request metrics for the S3 and admin APIs and
// renders them as a JSON snapshot (admin API / UI dashboard) and in the
// Prometheus text exposition format (GET /_gos3/metrics).
//
// CONTRACT: exported names/signatures are fixed. Implementation is owned by
// the s3-core work item (replace the stub bodies, add files freely).
package metrics

import (
	"io"
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

// Registry is safe for concurrent use.
type Registry struct {
	started time.Time
}

// NewRegistry returns an empty registry. CONTRACT STUB.
func NewRegistry() *Registry { return &Registry{started: time.Now()} }

// Begin marks a request in flight; call the returned func exactly once when
// it finishes. CONTRACT STUB.
func (r *Registry) Begin() (done func(op string, status int, bytesIn, bytesOut int64)) {
	return func(string, int, int64, int64) {}
}

// Snapshot returns current metrics. CONTRACT STUB.
func (r *Registry) Snapshot() Snapshot { return Snapshot{StartedAt: r.started} }

// WritePrometheus writes metrics in Prometheus text format (metric prefix
// "gos3_"). Extra gauges (e.g. bucket counts) may be supplied by the caller.
// CONTRACT STUB.
func (r *Registry) WritePrometheus(w io.Writer, extra map[string]float64) error { return nil }
