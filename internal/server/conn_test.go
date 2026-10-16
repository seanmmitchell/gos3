package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// TestTLSStateFromConnection: s3:TlsVersion and aws:SecureTransport come
// from the TLS connection, not from the client-chosen HTTP/2 ":scheme"
// (net/http leaves Request.TLS nil for ":scheme http" even over TLS). A
// TLS 1.2 client sending ":scheme http" must not dodge a policy that
// denies TLS versions below 1.3.
func TestTLSStateFromConnection(t *testing.T) {
	cfg := testConfig()
	cfg.Server.TLSCert, cfg.Server.TLSKey = writeSelfSigned(t)
	srv := start(t, cfg)
	base := srv.url
	tlsClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: nil, ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed test certificate
	}}
	signed := func(method, path, body string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		if err := signReq(req); err != nil {
			t.Fatal(err)
		}
		resp, err := tlsClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
		}
	}
	signed("PUT", "/tlsb", "")
	signed("PUT", "/tlsb/k", "hello")
	signed("PUT", "/tlsb?policy", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::tlsb/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::tlsb/*",
		 "Condition":{"NumericLessThan":{"s3:TlsVersion":"1.3"}}}]}`)

	get := func(maxVersion uint16) int {
		t.Helper()
		tr := &http2.Transport{
			AllowHTTP: true, // ":scheme http" ...
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				// ... over a real TLS connection.
				d := tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}, MaxVersion: maxVersion}}
				return d.DialContext(ctx, network, addr)
			},
		}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get("http://" + strings.TrimPrefix(base, "https://") + "/tlsb/k")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(tls.VersionTLS12); code != 403 {
		t.Errorf("TLS 1.2 with :scheme http: %d, want 403", code)
	}
	if code := get(tls.VersionTLS13); code != 200 {
		t.Errorf("TLS 1.3 with :scheme http: %d, want 200", code)
	}
	// aws:SecureTransport is true for such requests too.
	signed("PUT", "/tlsb?policy", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::tlsb/*",
		 "Condition":{"Bool":{"aws:SecureTransport":"true"}}}]}`)
	if code := get(tls.VersionTLS13); code != 200 {
		t.Errorf("aws:SecureTransport over TLS with :scheme http: %d, want 200", code)
	}
}

// TestHTTP2StalledStreamReleased: a client that never opens its HTTP/2
// flow control window (SETTINGS_INITIAL_WINDOW_SIZE 0) used to pin the
// handler of every stream (up to 250 per connection) forever, whatever the
// response. Now the stream is reset once a response write makes no
// progress for the write idle timeout, and the server announces a lower
// stream limit.
func TestHTTP2StalledStreamReleased(t *testing.T) {
	cfg := testConfig()
	cfg.Server.TLSCert, cfg.Server.TLSKey = writeSelfSigned(t)
	s, err := New(cfg, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	s.writeIdle = 300 * time.Millisecond
	srv := serve(t, s)
	addr := strings.TrimPrefix(srv.url, "https://")

	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0}); err != nil {
		t.Fatal(err)
	}
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "https"}, {":authority", addr}, {":path", "/_gos3/info"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	maxStreams := uint32(0)
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("no RST_STREAM for the stalled stream: %v", err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				if v, ok := f.Value(http2.SettingMaxConcurrentStreams); ok {
					maxStreams = v
				}
				_ = fr.WriteSettingsAck()
			}
		case *http2.DataFrame:
			if len(f.Data()) > 0 {
				t.Fatalf("DATA sent although the window is 0")
			}
		case *http2.RSTStreamFrame:
			if f.StreamID != 1 {
				continue
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("stream reset after %v", d)
			}
			if maxStreams != http2MaxStreams {
				t.Errorf("SETTINGS_MAX_CONCURRENT_STREAMS = %d, want %d", maxStreams, http2MaxStreams)
			}
			return
		}
	}
}

// TestMaxConnections: server.max_connections (opt-in) caps the open
// connections without ever blocking Accept: an idle keep-alive connection
// is closed to make room for a new one, and a new connection is refused at
// once (not queued) when every connection is busy, so a client holding
// connections cannot make health probes hang.
func TestMaxConnections(t *testing.T) {
	cfg := testConfig()
	cfg.Server.MaxConnections = 1
	srv := start(t, cfg)
	addr := strings.TrimPrefix(srv.url, "http://")
	get := func(c *http.Client) error {
		resp, err := c.Get(srv.url + "/_gos3/health")
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil
	}
	newClient := func() *http.Client {
		return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	}

	// net/http reports a connection idle only after the response is
	// finished, which the client may see first, and reports a connection
	// the limiter shed as closed only once its serving goroutine notices.
	// Each step below waits for the server's counts to settle: a client
	// that races them is (rightly) refused, or (rightly) served.
	lim := srv.limiter.Load()
	if lim == nil {
		t.Fatal("no connection limiter")
	}
	waitConns := func(what string, open, idle int) {
		t.Helper()
		var o, i int
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			o, i = lim.conns()
			if o == open && i == idle {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d open, %d idle connections, want %d, %d", what, o, i, open, idle)
			}
		}
	}

	// A keep-alive connection left idle gives way to a new client.
	if err := get(newClient()); err != nil {
		t.Fatal(err)
	}
	waitConns("the first connection never became idle", 1, 1)
	start := time.Now()
	if err := get(newClient()); err != nil {
		t.Fatalf("new client while a connection is idle: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("new client waited %v", d)
	}
	waitConns("the first connection was not closed, or the second one never became idle", 1, 1)

	// A busy connection (headers not yet sent) is not closed; the next one
	// is refused at once.
	hold, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(hold, "GET /_gos3/health HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	// The held connection took the idle one's place: the only open one is
	// busy.
	waitConns("the held connection was refused, or the idle one not closed", 1, 0)
	start = time.Now()
	if err := get(newClient()); err == nil {
		t.Error("connection served while the only one is busy")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("refusal took %v (accept must not block)", d)
	}
	if _, err := io.WriteString(hold, "Host: 127.0.0.1\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.ReadResponse(bufio.NewReader(hold), nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("held connection: %v", err)
	}
	hold.Close()
	waitFor(t, 5*time.Second, "a free connection slot", func() bool { return get(newClient()) == nil })
}

// TestReadyHidesBackendErrors: /_gos3/ready is anonymous: a failing
// backend's error text is logged, not returned, and checks are cached so
// polling cannot hammer the backend.
func TestReadyHidesBackendErrors(t *testing.T) {
	srv, err := New(testConfig(), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rec := newRecorder()
	srv.Handler().ServeHTTP(rec, httpRequest("GET", "/_gos3/ready"))
	if rec.Code != 200 {
		t.Fatalf("ready: %d %s", rec.Code, rec.Body)
	}
	// The cached result is reused until it expires.
	_ = srv.Backend().Close()
	rec = newRecorder()
	srv.Handler().ServeHTTP(rec, httpRequest("GET", "/_gos3/ready"))
	if rec.Code != 200 {
		t.Errorf("ready within the cache period: %d", rec.Code)
	}
	srv.readyMu.Lock()
	srv.readyAt = time.Now().Add(-readyCacheTTL)
	srv.readyMu.Unlock()
	rec = newRecorder()
	srv.Handler().ServeHTTP(rec, httpRequest("GET", "/_gos3/ready"))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"error":"storage unavailable"`) {
		t.Errorf("ready with a closed backend: %d %s", rec.Code, rec.Body)
	}
}

// signReq signs req with the root credentials.
func signReq(req *http.Request) error {
	return auth.SignRequest(req, rootCreds, "us-east-1", "s3", "", time.Now())
}
