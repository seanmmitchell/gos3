package metrics

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// classLabels maps statusClass results to code_class label values.
var classLabels = [6]string{"other", "1xx", "2xx", "3xx", "4xx", "5xx"}

// opSnapshot is a consistent copy of one operation's counters.
type opSnapshot struct {
	name     string
	classes  [6]int64
	requests int64
	bytesIn  int64
	bytesOut int64
	latSum   float64
	hist     [len(durationBuckets) + 1]int64
}

func (r *Registry) writePrometheus(w io.Writer, extra map[string]float64) error {
	bw := bufio.NewWriter(w)
	var ops []opSnapshot
	inflight, uptime, started := int64(0), 0.0, 0.0
	if r != nil {
		for _, e := range r.sortedOps() {
			e.st.mu.Lock()
			ops = append(ops, opSnapshot{
				name: e.name, classes: e.st.classes, requests: e.st.requests,
				bytesIn: e.st.bytesIn, bytesOut: e.st.bytesOut,
				latSum: e.st.latSum, hist: e.st.hist,
			})
			e.st.mu.Unlock()
		}
		inflight = r.inFlight.Load()
		uptime = math.Max(0, r.now().Sub(r.started).Seconds())
		started = float64(r.started.UnixNano()) / 1e9
	}

	header(bw, "gos3_requests_total", "counter", "Total API requests by operation and HTTP status class.")
	for _, o := range ops {
		for c, n := range o.classes {
			if n == 0 {
				continue
			}
			sample(bw, "gos3_requests_total", []string{"op", o.name, "code_class", classLabels[c]}, float64(n))
		}
	}

	header(bw, "gos3_request_duration_seconds", "histogram", "API request latency in seconds by operation.")
	for _, o := range ops {
		var cum int64
		for i, le := range durationBuckets {
			cum += o.hist[i]
			sample(bw, "gos3_request_duration_seconds_bucket", []string{"op", o.name, "le", formatFloat(le)}, float64(cum))
		}
		cum += o.hist[len(durationBuckets)]
		sample(bw, "gos3_request_duration_seconds_bucket", []string{"op", o.name, "le", "+Inf"}, float64(cum))
		sample(bw, "gos3_request_duration_seconds_sum", []string{"op", o.name}, o.latSum)
		sample(bw, "gos3_request_duration_seconds_count", []string{"op", o.name}, float64(o.requests))
	}

	header(bw, "gos3_bytes_in_total", "counter", "Request body bytes received by operation.")
	for _, o := range ops {
		sample(bw, "gos3_bytes_in_total", []string{"op", o.name}, float64(o.bytesIn))
	}
	header(bw, "gos3_bytes_out_total", "counter", "Response body bytes sent by operation.")
	for _, o := range ops {
		sample(bw, "gos3_bytes_out_total", []string{"op", o.name}, float64(o.bytesOut))
	}

	header(bw, "gos3_inflight_requests", "gauge", "API requests currently being served.")
	sample(bw, "gos3_inflight_requests", nil, float64(inflight))
	header(bw, "gos3_uptime_seconds", "gauge", "Seconds since the metrics registry was created.")
	sample(bw, "gos3_uptime_seconds", nil, uptime)
	header(bw, "gos3_start_time_seconds", "gauge", "Start time of the server in unix seconds.")
	sample(bw, "gos3_start_time_seconds", nil, started)

	// Extra gauges, sorted for stable output; names that collide after
	// sanitizing (or with built-in metrics) are dropped.
	seen := map[string]bool{
		"gos3_requests_total": true, "gos3_request_duration_seconds": true,
		"gos3_bytes_in_total": true, "gos3_bytes_out_total": true,
		"gos3_inflight_requests": true, "gos3_uptime_seconds": true,
		"gos3_start_time_seconds": true,
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := MetricName(k)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		header(bw, name, "gauge", "")
		sample(bw, name, nil, extra[k])
	}
	return bw.Flush()
}

// MetricName turns an arbitrary gauge name into a valid Prometheus metric
// name with the "gos3_" prefix: characters outside [a-zA-Z0-9_:] become '_'.
// It returns "" for names that are empty after trimming.
func MetricName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + 5)
	if !strings.HasPrefix(s, "gos3_") {
		b.WriteString("gos3_")
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == ':':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func header(w *bufio.Writer, name, typ, help string) {
	if help != "" {
		w.WriteString("# HELP " + name + " " + help + "\n")
	}
	w.WriteString("# TYPE " + name + " " + typ + "\n")
}

// sample writes one sample line; labels are name/value pairs.
func sample(w *bufio.Writer, name string, labels []string, v float64) {
	w.WriteString(name)
	if len(labels) > 0 {
		w.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.WriteByte(',')
			}
			w.WriteString(labels[i])
			w.WriteString(`="`)
			w.WriteString(escapeLabel(labels[i+1]))
			w.WriteByte('"')
		}
		w.WriteByte('}')
	}
	w.WriteByte(' ')
	w.WriteString(formatFloat(v))
	w.WriteByte('\n')
}

// escapeLabel escapes a label value per the text exposition format.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
