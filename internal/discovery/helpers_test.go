package discovery

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
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

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// testLogger logs to the test output at debug level.
func testLogger(t testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t testing.TB }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// gos3Server is an httptest server answering /_gos3/info.
type gos3Server struct {
	*httptest.Server
	mu   sync.Mutex
	info Info
	hits int
}

// newGos3Server starts a fake gos3 server with the given id and name; its
// endpoints point at the server's own URL. It is closed on test cleanup.
func newGos3Server(t testing.TB, id, name string) *gos3Server {
	t.Helper()
	g := &gos3Server{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != InfoPath {
			http.NotFound(w, r)
			return
		}
		g.mu.Lock()
		info := g.info
		g.hits++
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	}))
	g.info = Info{
		Service:    ServiceName,
		ID:         id,
		Name:       name,
		Version:    "0.1.0",
		APIVersion: "v1",
		Region:     "us-east-1",
		Endpoints:  Endpoints{S3: g.URL, Admin: g.URL + "/_gos3/admin/v1", All: []string{g.URL}},
		Features:   []string{"versioning"},
		StartedAt:  time.Now().UTC().Truncate(time.Second),
	}
	t.Cleanup(g.Close)
	return g
}

func (g *gos3Server) Info() Info {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.info
}

func (g *gos3Server) Hits() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

// hostPort returns "127.0.0.1:port" of the server.
func (g *gos3Server) hostPort() string { return strings.TrimPrefix(g.URL, "http://") }

func (g *gos3Server) port(t testing.TB) int {
	t.Helper()
	_, p, err := net.SplitHostPort(g.hostPort())
	if err != nil {
		t.Fatal(err)
	}
	n, err := parsePort(p)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// eventually polls cond until it is true or the timeout expires.
func eventually(t testing.TB, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// leakMarkers identify goroutines started by this package or its
// dependencies.
var leakMarkers = []string{
	"gos3/internal/discovery.(",
	"gos3/internal/discovery.mdns",
	"hashicorp/mdns",
	"net/http.(*persistConn)",
}

// checkLeaks returns a function that fails the test if goroutines started by
// discovery code are still running (after a grace period).
func checkLeaks(t *testing.T) func() {
	t.Helper()
	return func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			leaked := leakedGoroutines()
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("%d leaked goroutine(s):\n%s", len(leaked), strings.Join(leaked, "\n\n"))
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func leakedGoroutines() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var out []string
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "testing.tRunner") || strings.Contains(g, "testing.(*T)") {
			continue
		}
		for _, m := range leakMarkers {
			if strings.Contains(g, m) {
				out = append(out, g)
				break
			}
		}
	}
	return out
}

// fakeGroup simulates a multicast group over unicast loopback sockets: every
// beacon gets a "group" socket on 127.0.0.1 and group sends go to all of
// them.
type fakeGroup struct {
	mu    sync.Mutex
	addrs []net.Addr
}

func (g *fakeGroup) dests() []net.Addr {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]net.Addr(nil), g.addrs...)
}

// opener returns a Service.openBeacon replacement.
func (g *fakeGroup) opener(t testing.TB) func(*net.UDPAddr, beaconOptions) (*beacon, error) {
	return func(_ *net.UDPAddr, o beaconOptions) (*beacon, error) {
		uc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return nil, err
		}
		mc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			uc.Close()
			return nil, err
		}
		g.mu.Lock()
		g.addrs = append(g.addrs, mc.LocalAddr())
		g.mu.Unlock()
		return newBeacon(o, uc, mc, g.dests), nil
	}
}

// recordSink records sink calls.
type recordSink struct {
	mu    sync.Mutex
	cands []candidate
	byes  []string
}

func (r *recordSink) candidate(c candidate) {
	r.mu.Lock()
	r.cands = append(r.cands, c)
	r.mu.Unlock()
}

func (r *recordSink) bye(id string, _ netip.Addr) {
	r.mu.Lock()
	r.byes = append(r.byes, id)
	r.mu.Unlock()
}

func (r *recordSink) snapshot() ([]candidate, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]candidate(nil), r.cands...), append([]string(nil), r.byes...)
}

// quietConfig is a running config with every network mechanism disabled.
func quietConfig() Config {
	c := DefaultConfig()
	c.MDNS = false
	c.Beacon = false
	return c
}
