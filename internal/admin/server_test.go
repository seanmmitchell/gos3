package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestCORS(t *testing.T) {
	e := newEnv(t)
	preflight := func(origin, method, headers string) response {
		req, _ := http.NewRequest("OPTIONS", e.srv.URL+Prefix+"/users", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if method != "" {
			req.Header.Set("Access-Control-Request-Method", method)
		}
		if headers != "" {
			req.Header.Set("Access-Control-Request-Headers", headers)
		}
		return e.send(req)
	}
	t.Run("allowed preflight without auth", func(t *testing.T) {
		r := preflight("https://ui.example.com", "POST", "Authorization, X-Amz-Date, x-amz-content-sha256, content-type")
		r.expectStatus(t, 204)
		h := r.header
		if h.Get("Access-Control-Allow-Origin") != "https://ui.example.com" {
			t.Errorf("Allow-Origin = %q", h.Get("Access-Control-Allow-Origin"))
		}
		if h.Get("Access-Control-Allow-Methods") != corsMethods {
			t.Errorf("Allow-Methods = %q", h.Get("Access-Control-Allow-Methods"))
		}
		if got := h.Get("Access-Control-Allow-Headers"); got != "authorization, x-amz-date, x-amz-content-sha256, content-type" {
			t.Errorf("Allow-Headers = %q", got)
		}
		if h.Get("Access-Control-Max-Age") != "600" {
			t.Errorf("Max-Age = %q", h.Get("Access-Control-Max-Age"))
		}
		if !strings.Contains(strings.Join(h.Values("Vary"), ","), "Origin") {
			t.Errorf("Vary = %q", h.Values("Vary"))
		}
	})
	t.Run("wildcard subdomain", func(t *testing.T) {
		preflight("https://a.b.apps.example.com", "GET", "").expectStatus(t, 204)
		preflight("https://apps.example.com", "GET", "").expectError(t, 403, codeAccessDenied)
		preflight("http://x.apps.example.com", "GET", "").expectError(t, 403, codeAccessDenied)
	})
	t.Run("disallowed origin", func(t *testing.T) {
		r := preflight("https://evil.example.org", "GET", "")
		r.expectError(t, 403, codeAccessDenied)
		if r.header.Get("Access-Control-Allow-Origin") != "" {
			t.Error("Allow-Origin set for a disallowed origin")
		}
	})
	t.Run("disallowed method", func(t *testing.T) {
		preflight("https://ui.example.com", "TRACE", "").expectError(t, 403, codeAccessDenied)
	})
	t.Run("restricted headers", func(t *testing.T) {
		if _, err := e.settings.Update(func(s *config.Settings) error {
			s.CORS.AllowedHeaders = []string{"authorization", "x-amz-*"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		defer e.settings.Update(func(s *config.Settings) error { s.CORS.AllowedHeaders = []string{"*"}; return nil })
		preflight("https://ui.example.com", "GET", "Authorization, X-Amz-Date").expectStatus(t, 204)
		preflight("https://ui.example.com", "GET", "Authorization, X-Custom").expectError(t, 403, codeAccessDenied)
	})
	t.Run("star origin", func(t *testing.T) {
		if _, err := e.settings.Update(func(s *config.Settings) error {
			s.CORS.AllowedOrigins = []string{"*"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		defer e.settings.Update(func(s *config.Settings) error {
			s.CORS.AllowedOrigins = defaultSettings().CORS.AllowedOrigins
			return nil
		})
		r := preflight("http://localhost:3000", "PUT", "")
		r.expectStatus(t, 204)
		if r.header.Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
			t.Errorf("Allow-Origin = %q", r.header.Get("Access-Control-Allow-Origin"))
		}
	})
	t.Run("plain OPTIONS", func(t *testing.T) {
		r := preflight("", "", "")
		r.expectStatus(t, 204)
		if r.header.Get("Allow") == "" {
			t.Error("no Allow header")
		}
	})
	t.Run("actual responses", func(t *testing.T) {
		r := e.root("GET", "/info", nil, withHeader("Origin", "https://ui.example.com"))
		r.expectStatus(t, 200)
		if r.header.Get("Access-Control-Allow-Origin") != "https://ui.example.com" {
			t.Errorf("Allow-Origin = %q", r.header.Get("Access-Control-Allow-Origin"))
		}
		if r.header.Get("Access-Control-Expose-Headers") != "ETag, x-amz-request-id" {
			t.Errorf("Expose-Headers = %q", r.header.Get("Access-Control-Expose-Headers"))
		}
		// Errors carry CORS headers too, so browsers can read them.
		r = e.do(nil, "GET", Prefix+"/info", nil, withHeader("Origin", "https://ui.example.com"))
		r.expectError(t, 401, codeUnauthenticated)
		if r.header.Get("Access-Control-Allow-Origin") != "https://ui.example.com" {
			t.Error("no CORS headers on error response")
		}
		r = e.root("GET", "/info", nil, withHeader("Origin", "https://evil.example.org"))
		if r.header.Get("Access-Control-Allow-Origin") != "" {
			t.Error("Allow-Origin set for disallowed origin")
		}
	})
}

func TestCORSOriginAllowed(t *testing.T) {
	tests := []struct {
		origin  string
		allowed []string
		want    bool
	}{
		{"https://a.com", []string{"*"}, true},
		{"https://a.com", []string{"https://a.com"}, true},
		{"HTTPS://A.COM", []string{"https://a.com"}, true},
		{"https://a.com:8443", []string{"https://a.com"}, false},
		{"https://a.com:8443", []string{"https://a.com:8443"}, true},
		{"https://x.a.com", []string{"https://*.a.com"}, true},
		{"https://y.x.a.com", []string{"https://*.a.com"}, true},
		{"https://a.com", []string{"https://*.a.com"}, false},
		{"https://xa.com", []string{"https://*.a.com"}, false},
		{"http://x.a.com", []string{"https://*.a.com"}, false},
		{"https://x.a.com:1", []string{"https://*.a.com"}, false},
		{"https://x.a.com:1", []string{"https://*.a.com:1"}, true},
		{"https://evil.com/.a.com", []string{"https://*.a.com"}, false},
		{"", []string{"*"}, false},
		{"https://a.com", nil, false},
	}
	for _, tt := range tests {
		if got := corsOriginAllowed(tt.origin, tt.allowed); got != tt.want {
			t.Errorf("corsOriginAllowed(%q, %v) = %v, want %v", tt.origin, tt.allowed, got, tt.want)
		}
	}
}

func TestWildcardMatch(t *testing.T) {
	tests := []struct {
		p, s string
		want bool
	}{
		{"*", "", true}, {"*", "abc", true}, {"x-amz-*", "x-amz-date", true}, {"x-amz-*", "x-am", false},
		{"a*b*c", "aXbYc", true}, {"a*b*c", "aXbY", false}, {"abc", "abc", true}, {"abc", "abd", false},
		{"**", "x", true}, {"*a", "bba", true}, {"*a", "bab", false},
	}
	for _, tt := range tests {
		if got := wildcardMatch(tt.p, tt.s); got != tt.want {
			t.Errorf("wildcardMatch(%q, %q) = %v", tt.p, tt.s, got)
		}
	}
}

func TestServerInfo(t *testing.T) {
	e := newEnv(t, withFSBackend(t.TempDir()))
	r := e.root("GET", "/info", nil)
	r.expectStatus(t, 200)
	var raw map[string]any
	r.json(t, &raw)
	for _, k := range []string{"service", "id", "name", "version", "apiVersion", "region", "endpoints", "features",
		"startedAt", "backend", "dataPath", "goVersion", "os", "arch", "numCPU", "uptimeSeconds"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing %q in %s", k, r.body)
		}
	}
	var si serverInfo
	r.json(t, &si)
	if si.ID != e.info.ID || si.Backend != "fs" || si.GoVersion != runtime.Version() || si.OS != runtime.GOOS ||
		si.NumCPU != runtime.NumCPU() {
		t.Errorf("info = %+v", si)
	}
	if si.UptimeSeconds < 59 {
		t.Errorf("uptimeSeconds = %d", si.UptimeSeconds)
	}
}

func TestStats(t *testing.T) {
	dir := t.TempDir()
	e := newEnv(t, withFSBackend(dir))
	e.createBucket("one", storage.CreateBucketOptions{})
	e.putObject("one", "a", "hello")

	get := func() statsResponse {
		t.Helper()
		r := e.root("GET", "/stats", nil)
		r.expectStatus(t, 200)
		var s statsResponse
		r.json(t, &s)
		return s
	}
	s := get()
	if s.Storage.Buckets != 1 || s.Storage.Objects != 1 || s.Storage.Bytes != 5 || s.Storage.ComputedAt.IsZero() {
		t.Errorf("storage = %+v", s.Storage)
	}
	if s.Runtime.Goroutines <= 0 || s.Runtime.SysBytes == 0 {
		t.Errorf("runtime = %+v", s.Runtime)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if s.Disk == nil || s.Disk.Path != dir || s.Disk.TotalBytes == 0 || s.Disk.TotalBytes < s.Disk.UsedBytes {
			t.Errorf("disk = %+v", s.Disk)
		}
	}

	// Cached: a new object is not visible until the TTL expires.
	e.createBucket("two", storage.CreateBucketOptions{})
	e.putObject("two", "b", "xyz")
	if s2 := get(); s2.Storage != s.Storage {
		t.Errorf("stats not cached: %+v vs %+v", s2.Storage, s.Storage)
	}
	e.clock.Advance(usageTTL + time.Second)
	s3 := get()
	if s3.Storage.Buckets != 2 || s3.Storage.Objects != 2 || s3.Storage.Bytes != 8 {
		t.Errorf("after TTL storage = %+v", s3.Storage)
	}

	// metrics present (and arrays never null)
	r := e.root("GET", "/stats", nil)
	var raw struct {
		Metrics map[string]json.RawMessage `json:"metrics"`
		Disk    json.RawMessage            `json:"disk"`
	}
	r.json(t, &raw)
	if string(raw.Metrics["ops"]) == "null" || string(raw.Metrics["recent"]) == "null" {
		t.Errorf("metrics arrays null: %s", r.body)
	}
}

// The storage totals add up cached per-bucket usage, so their computedAt is
// that of the oldest figure, not the moment they were summed.
func TestStatsComputedAtIsOldestBucketUsage(t *testing.T) {
	e := newEnv(t)
	e.createBucket("one", storage.CreateBucketOptions{})
	e.putObject("one", "a", "abc")
	t0 := e.clock.Now().UTC()
	e.root("GET", "/buckets?usage=true", nil).expectStatus(t, 200) // caches "one" at t0

	e.clock.Advance(usageTTL / 2)
	e.createBucket("two", storage.CreateBucketOptions{})
	var s statsResponse
	e.root("GET", "/stats", nil).json(t, &s)
	if s.Storage.Buckets != 2 || s.Storage.Objects != 1 {
		t.Fatalf("storage = %+v", s.Storage)
	}
	if !s.Storage.ComputedAt.Equal(t0) {
		t.Errorf("computedAt = %v, want the cached bucket usage time %v", s.Storage.ComputedAt, t0)
	}

	// Once every per-bucket entry expired, the totals are fresh again.
	e.clock.Advance(usageTTL + time.Second)
	e.root("GET", "/stats", nil).json(t, &s)
	if want := e.clock.Now().UTC(); !s.Storage.ComputedAt.Equal(want) {
		t.Errorf("computedAt after expiry = %v, want %v", s.Storage.ComputedAt, want)
	}
}

func TestStatsWithoutMetricsAndMemoryBackend(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Metrics = nil })
	r := e.root("GET", "/stats", nil)
	r.expectStatus(t, 200)
	var raw map[string]json.RawMessage
	r.json(t, &raw)
	if string(raw["disk"]) != "null" {
		t.Errorf("disk = %s, want null for memory backend", raw["disk"])
	}
	if string(raw["metrics"]) == "null" {
		t.Error("metrics null without registry")
	}
}

func TestTTLCacheSingleflight(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	c := newTTLCache[int](time.Minute, time.Minute, clock.Now)
	var calls atomic.Int32
	release := make(chan struct{})
	compute := func(ctx context.Context) (int, error) {
		calls.Add(1)
		<-release
		return 42, nil
	}
	var wg sync.WaitGroup
	results := make([]int, 20)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _, err := c.get(context.Background(), "k", compute)
			if err != nil {
				t.Error(err)
			}
			results[i] = v
		}()
	}
	// Let the goroutines pile up on the in-flight call, then release it.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("compute called %d times", n)
	}
	for _, v := range results {
		if v != 42 {
			t.Fatalf("results = %v", results)
		}
	}
	// cached
	if v, _, _ := c.get(context.Background(), "k", compute); v != 42 || calls.Load() != 1 {
		t.Error("value not cached")
	}
	// expired
	clock.Advance(2 * time.Minute)
	c.get(context.Background(), "k", compute)
	if calls.Load() != 2 {
		t.Error("expired value not recomputed")
	}
	// invalidate
	c.invalidate("k")
	c.get(context.Background(), "k", compute)
	if calls.Load() != 3 {
		t.Error("invalidated value not recomputed")
	}
}

func TestTTLCacheCallerCancelAndErrors(t *testing.T) {
	c := newTTLCache[int](time.Minute, time.Minute, time.Now)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.get(ctx, "k", func(ctx context.Context) (int, error) {
			<-release
			return 7, ctx.Err() // the computation itself must not be canceled
		})
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	close(release)
	// A second caller waits for the same computation and gets its value.
	v, _, err := c.get(context.Background(), "k", func(context.Context) (int, error) { return 9, nil })
	if err != nil || (v != 7 && v != 9) {
		t.Fatalf("v = %d err = %v", v, err)
	}
	// Errors and panics are not cached.
	c2 := newTTLCache[int](time.Minute, time.Minute, time.Now)
	if _, _, err := c2.get(context.Background(), "e", func(context.Context) (int, error) { panic("boom") }); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Fatalf("panic err = %v", err)
	}
	if v, _, err := c2.get(context.Background(), "e", func(context.Context) (int, error) { return 1, nil }); err != nil || v != 1 {
		t.Fatalf("after panic: %d %v", v, err)
	}
}

func TestConfig(t *testing.T) {
	e := newEnv(t)
	var notified atomic.Int32
	e.settings.Subscribe(func(old, new config.Settings) { notified.Add(1) })

	r := e.root("GET", "/config", nil)
	r.expectStatus(t, 200)
	var s config.Settings
	r.json(t, &s)
	if s.Name != "gos3-test" || s.Discovery.BeaconGroup != "239.255.73.83:7373" {
		t.Errorf("settings = %+v", s)
	}

	s.Name = "renamed"
	s.CORS.AllowedOrigins = []string{"*", "https://*.example.com", "http://[::1]:3000", "http://10.0.0.1:8080", "app://localhost"}
	s.Lifecycle.IntervalSeconds = 60
	r = e.root("PUT", "/config", s)
	r.expectStatus(t, 200)
	var out config.Settings
	r.json(t, &out)
	if out.Name != "renamed" || e.settings.Get().Name != "renamed" || notified.Load() != 1 {
		t.Errorf("update not applied: %+v (notified %d)", out, notified.Load())
	}
}

func TestConfigValidation(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name   string
		mutate func(*config.Settings)
		want   string // substring of the message
	}{
		{"region", func(s *config.Settings) { s.Region = "US East" }, "region"},
		{"empty region", func(s *config.Settings) { s.Region = "" }, "region"},
		{"name control", func(s *config.Settings) { s.Name = "a\x00b" }, "name"},
		{"public url scheme", func(s *config.Settings) { s.PublicURL = "ftp://x" }, "publicUrl"},
		{"public url query", func(s *config.Settings) { s.PublicURL = "http://x?y=1" }, "publicUrl"},
		{"public url creds", func(s *config.Settings) { s.PublicURL = "http://u:p@x" }, "publicUrl"},
		{"domain", func(s *config.Settings) { s.Domains = []string{"s3.local", "bad domain"} }, "domains[1]"},
		{"domain port", func(s *config.Settings) { s.Domains = []string{"s3.local:9000"} }, "domains[0]"},
		{"cors origin path", func(s *config.Settings) { s.CORS.AllowedOrigins = []string{"https://a.com/"} }, "allowedOrigins[0]"},
		{"cors origin bare", func(s *config.Settings) { s.CORS.AllowedOrigins = []string{"a.com"} }, "allowedOrigins[0]"},
		{"cors origin port", func(s *config.Settings) { s.CORS.AllowedOrigins = []string{"https://a.com:99999"} }, "allowedOrigins[0]"},
		{"cors origin wildcard mid", func(s *config.Settings) { s.CORS.AllowedOrigins = []string{"https://a.*.com"} }, "allowedOrigins[0]"},
		{"cors header", func(s *config.Settings) { s.CORS.AllowedHeaders = []string{"bad header"} }, "allowedHeaders[0]"},
		{"cors expose pattern", func(s *config.Settings) { s.CORS.ExposeHeaders = []string{"ETag", "x-amz-*"} }, "exposeHeaders[1]"},
		{"cors max age", func(s *config.Settings) { s.CORS.MaxAgeSeconds = -1 }, "maxAgeSeconds"},
		{"beacon group not multicast", func(s *config.Settings) { s.Discovery.BeaconGroup = "10.0.0.1:7373" }, "beaconGroup"},
		{"beacon group no port", func(s *config.Settings) { s.Discovery.BeaconGroup = "239.255.73.83" }, "beaconGroup"},
		{"beacon group bad port", func(s *config.Settings) { s.Discovery.BeaconGroup = "239.255.73.83:0" }, "beaconGroup"},
		{"beacon group ipv6", func(s *config.Settings) { s.Discovery.BeaconGroup = "[ff02::1]:7373" }, "beaconGroup"},
		{"beacon interval", func(s *config.Settings) { s.Discovery.BeaconInterval = 0 }, "beaconInterval"},
		{"beacon interval high", func(s *config.Settings) { s.Discovery.BeaconInterval = 2 * time.Hour }, "beaconInterval"},
		{"peer ttl", func(s *config.Settings) { s.Discovery.PeerTTL = -time.Second }, "peerTtl"},
		{"seed", func(s *config.Settings) { s.Discovery.Seeds = []string{"not a url"} }, "seeds[0]"},
		{"seed path", func(s *config.Settings) { s.Discovery.Seeds = []string{"http://h:9000/x"} }, "seeds[0]"},
		{"dns srv", func(s *config.Settings) { s.Discovery.DNSSRV = []string{"-bad.example"} }, "dnsSrv[0]"},
		{"dns srv too many", func(s *config.Settings) { s.Discovery.DNSSRV = make([]string, 65) }, "dnsSrv"},
		{"max scan hosts low", func(s *config.Settings) { s.Discovery.MaxScanHosts = 0 }, "maxScanHosts"},
		{"max scan hosts high", func(s *config.Settings) { s.Discovery.MaxScanHosts = 65537 }, "maxScanHosts"},
		{"log level", func(s *config.Settings) { s.Logging.Level = "verbose" }, "logging.level"},
		{"log format", func(s *config.Settings) { s.Logging.Format = "xml" }, "logging.format"},
		{"lifecycle interval", func(s *config.Settings) { s.Lifecycle.IntervalSeconds = 59 }, "lifecycle.intervalSeconds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := defaultSettings()
			tt.mutate(&s)
			msg := e.root("PUT", "/config", s).expectError(t, 400, codeInvalidSettings)
			if !strings.Contains(msg, tt.want) {
				t.Errorf("message %q does not mention %q", msg, tt.want)
			}
		})
	}
	t.Run("unknown field", func(t *testing.T) {
		e.root("PUT", "/config", `{"name":"x","nope":true}`).expectError(t, 400, codeInvalidSettings)
	})
	t.Run("valid edge values", func(t *testing.T) {
		s := defaultSettings()
		s.PublicURL = ""
		s.Domains = nil
		s.Discovery.MaxScanHosts = 65536
		s.Discovery.Seeds = []string{"https://peer.example.com:9443", "nas.local:9000"}
		s.Discovery.DNSSRV = []string{"example.com."}
		s.Discovery.BeaconGroup = "239.1.2.3:7000"
		s.CORS.ExposeHeaders = []string{"*", "ETag", "x-amz-version-id"}
		e.root("PUT", "/config", s).expectStatus(t, 200)
	})
	t.Run("defaults round trip", func(t *testing.T) {
		// The settings a default server starts with must survive the UI's
		// GET-then-PUT round trip (same validator on both sides).
		s := config.SettingsFrom(config.Default())
		e.root("PUT", "/config", s).expectStatus(t, 200)
		var got config.Settings
		e.root("GET", "/config", nil).json(t, &got)
		e.root("PUT", "/config", got).expectStatus(t, 200)
	})
	t.Run("message names field and value", func(t *testing.T) {
		s := defaultSettings()
		s.Discovery.Seeds = []string{"http://ok:9000", "ftp://peer"}
		msg := e.root("PUT", "/config", s).expectError(t, 400, codeInvalidSettings)
		if !strings.HasPrefix(msg, "discovery.seeds[1]: ") || !strings.Contains(msg, "ftp://peer") {
			t.Errorf("message %q", msg)
		}
	})
	if got := e.settings.Get().Region; got != "us-east-1" {
		t.Errorf("invalid settings applied: region %q", got)
	}
}
