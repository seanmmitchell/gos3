package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
)

const (
	testAccessKey = "testrootaccess"
	testSecretKey = "testrootsecret-0123456789"
)

var rootCreds = auth.Credentials{AccessKey: testAccessKey, SecretKey: testSecretKey}

// testConfig returns an ephemeral memory-backed configuration listening on
// a random loopback port with discovery off and fixed root credentials.
func testConfig() config.Config {
	cfg := config.Default()
	cfg.Server.Address = "127.0.0.1:0"
	cfg.Server.Name = "test-server"
	cfg.Server.ShutdownTimeout = 5 * time.Second
	cfg.Storage.Backend = config.BackendMemory
	cfg.Storage.Path = ""
	cfg.Discovery.Enabled = false
	cfg.Auth.RootAccessKey = testAccessKey
	cfg.Auth.RootSecretKey = testSecretKey
	cfg.Logging.AccessLog = false
	return cfg
}

// fakeUI is an injected UI build.
func fakeUI() fstest.MapFS {
	return fstest.MapFS{
		"200.html":            {Data: []byte("<!doctype html><title>gos3 spa</title>")},
		"_nuxt/app.123abc.js": {Data: []byte("console.log(1)")},
	}
}

// testLogger returns a logger writing to t.Log.
func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(tWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type tWriter struct{ t *testing.T }

func (w tWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// running is a server served on a loopback listener.
type running struct {
	*Server
	url  string
	stop func() error // cancels Serve and returns its error
}

// start creates and serves a server; it is stopped at test cleanup.
func start(t *testing.T, cfg config.Config, opts ...Option) *running {
	t.Helper()
	srv, err := New(cfg, testLogger(t), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return serve(t, srv)
}

func serve(t *testing.T, srv *Server) *running {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ctx, ln) }()
	select {
	case <-srv.Ready():
	case err := <-errc:
		t.Fatalf("Serve: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not become ready")
	}
	var stopped bool
	var stopErr error
	r := &running{Server: srv, url: srv.URL()}
	r.stop = func() error {
		if !stopped {
			stopped = true
			cancel()
			select {
			case stopErr = <-errc:
			case <-time.After(20 * time.Second):
				t.Error("server did not stop")
			}
		}
		return stopErr
	}
	t.Cleanup(func() {
		if err := r.stop(); err != nil {
			t.Errorf("Serve returned %v", err)
		}
	})
	return r
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decoding %q: %v", r.body, err)
	}
}

// do sends a request; creds nil means anonymous. hdr may set "Host".
func do(t *testing.T, method, url string, body []byte, creds *auth.Credentials, hdr map[string]string) response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	if creds != nil {
		if err := auth.SignRequest(req, *creds, "us-east-1", "s3", "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

var noRedirectClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport:     &http.Transport{Proxy: nil},
}

// waitFor polls cond until it is true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type fsMap = fstest.MapFS

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func httpRequest(method, target string) *http.Request {
	return httptest.NewRequest(method, target, nil)
}
