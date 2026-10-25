package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	assertHTTP2StalledStreamReleased(t, strings.TrimPrefix(srv.url, "https://"), http2MaxStreams)
}

// A body too large for net/http's response buffer must time out inside
// Write too, not only during the final flush after the handler returns.
func TestHTTP2StalledBodyWriteReleased(t *testing.T) {
	s := &Server{writeIdle: 300 * time.Millisecond}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w, finish := s.guardWrites(w)
		defer finish()
		_, _ = w.Write(make([]byte, 2*maxGuardedWrite))
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	assertHTTP2StalledStreamReleased(t, strings.TrimPrefix(ts.URL, "https://"), 0)
}

func assertHTTP2StalledStreamReleased(t *testing.T, addr string, wantMaxStreams uint32) {
	t.Helper()
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
			if wantMaxStreams != 0 && maxStreams != wantMaxStreams {
				t.Errorf("SETTINGS_MAX_CONCURRENT_STREAMS = %d, want %d", maxStreams, wantMaxStreams)
			}
			return
		}
	}
}

// TestMaxConnections: server.max_connections (opt-in) caps the open
// connections without ever blocking Accept: an idle keep-alive connection
// is closed to make room for a new one, then a connection that has waited
// longer than the grace period for its first request; a new connection is
// refused at once (not queued) only when every connection is busy with a
// request or still within its grace period, so a client holding
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
	setGrace := func(d time.Duration) {
		lim.mu.Lock()
		lim.grace = d
		lim.mu.Unlock()
	}
	waitConns := func(what string, open, idle, pending int) {
		t.Helper()
		var o, i, p int
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			o, i, p = lim.conns()
			if o == open && i == idle && p == pending {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d open, %d idle, %d pending connections, want %d, %d, %d", what, o, i, p, open, idle, pending)
			}
		}
	}
	refused := func(what string) {
		t.Helper()
		start := time.Now()
		if err := get(newClient()); err == nil {
			t.Errorf("connection served while %s", what)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("refusal took %v (accept must not block)", d)
		}
	}
	closed := func(what string, c net.Conn) {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Errorf("%s: read data, want the connection closed", what)
		} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Errorf("%s: still open", what)
		}
	}

	// A keep-alive connection left idle gives way to a new client.
	if err := get(newClient()); err != nil {
		t.Fatal(err)
	}
	waitConns("the first connection never became idle", 1, 1, 0)
	start := time.Now()
	if err := get(newClient()); err != nil {
		t.Fatalf("new client while a connection is idle: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("new client waited %v", d)
	}
	waitConns("the first connection was not closed, or the second one never became idle", 1, 1, 0)

	// A connection that has not sent a whole request (half-sent headers)
	// takes the idle one's place. Within the grace period it is kept and
	// the next connection is refused at once; after it, the held
	// connection gives way to a new client.
	const grace = time.Second
	setGrace(grace)
	hold, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	held := time.Now()
	if _, err := io.WriteString(hold, "GET /_gos3/health HTTP/1.1\r\n"); err != nil {
		t.Fatal(err)
	}
	waitConns("the held connection was refused, or the idle one not closed", 1, 0, 1)
	if time.Since(held) < grace {
		refused("the only connection is within its grace period")
	}
	waitFor(t, 5*time.Second, "the held connection to give way", func() bool { return get(newClient()) == nil })
	if d := time.Since(held); d < grace {
		t.Errorf("held connection closed after %v, before the %v grace period", d, grace)
	}
	closed("held connection past the grace period", hold)
	waitConns("the held connection was not closed, or the new one never became idle", 1, 1, 0)
}

// TestConnLimiterKeepsActiveConnections: a connection with a request in
// flight is never closed to make room, however old, and pending
// connections are evicted only past the grace period, oldest first. This
// drives the limiter directly: over HTTP, whether a request is still in
// flight depends on the handler and on timing.
func TestConnLimiterKeepsActiveConnections(t *testing.T) {
	pipe := func() net.Conn {
		c, peer := net.Pipe()
		t.Cleanup(func() { _ = c.Close(); _ = peer.Close() })
		return c
	}
	isClosed := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		ne, timeout := err.(net.Error)
		return err != nil && !(timeout && ne.Timeout())
	}

	l := newConnLimiter(2, func(string, ...any) {})
	l.grace = time.Hour
	active, older, newer := pipe(), pipe(), pipe()

	// An old connection with a request in flight is never a victim.
	l.connState(active, http.StateNew)
	l.connState(active, http.StateActive)
	l.connState(older, http.StateNew)
	if l.admit() {
		t.Fatal("admitted while the only other connection is pending within its grace period")
	}
	l.mu.Lock()
	l.pending[older] = time.Now().Add(-2 * time.Hour)
	l.mu.Unlock()
	if !l.admit() {
		t.Fatal("refused although a pending connection is past its grace period")
	}
	if !isClosed(older) || isClosed(active) {
		t.Fatal("evicted the wrong connection: want the old pending one, never the active one")
	}
	l.connState(older, http.StateClosed)
	l.connState(newer, http.StateNew)
	l.mu.Lock()
	l.pending[newer] = time.Now().Add(-2 * time.Hour)
	l.grace = 0
	l.mu.Unlock()
	if open, idle, pending := l.conns(); open != 2 || idle != 0 || pending != 1 {
		t.Fatalf("%d open, %d idle, %d pending, want 2, 0, 1", open, idle, pending)
	}
	l.connState(newer, http.StateActive)
	if l.admit() {
		t.Fatal("admitted by closing a connection with a request in flight")
	}
	if isClosed(active) || isClosed(newer) {
		t.Fatal("closed a connection with a request in flight")
	}
}

// addrConn is a net.Conn with a chosen remote address.
type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }

// TestConnLimiterEvictsBusiestAddressFirst: at the cap, a client that keeps
// opening connections evicts its own pending connections before an older
// one of a slow client elsewhere.
func TestConnLimiterEvictsBusiestAddressFirst(t *testing.T) {
	conn := func(ip string) net.Conn {
		c, peer := net.Pipe()
		t.Cleanup(func() { _ = c.Close(); _ = peer.Close() })
		return addrConn{Conn: c, remote: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
	}
	l := newConnLimiter(3, func(string, ...any) {})
	l.grace = 0
	// The flooding client uses two addresses of one IPv6 /64.
	slow, flood1, flood2 := conn("192.0.2.1"), conn("2001:db8:1:2::7"), conn("2001:db8:1:2::8")
	for i, c := range []net.Conn{slow, flood1, flood2} {
		l.connState(c, http.StateNew)
		l.pending[c] = time.Now().Add(time.Duration(i-10) * time.Second) // slow is the oldest
	}
	if !l.admit() {
		t.Fatal("refused although pending connections are past the grace period")
	}
	l.mu.Lock()
	_, slowKept := l.pending[slow]
	_, flood1Kept := l.pending[flood1]
	l.mu.Unlock()
	if !slowKept || flood1Kept {
		t.Fatal("want the flooding address's oldest pending connection evicted, not the slow client's")
	}
}

// TestMaxConnectionsSilentClients: connections that never send a request
// (or never finish their TLS handshake) count toward server.max_connections
// until ReadHeaderTimeout. They used to be impossible to evict, so a client
// holding max_connections silent sockets (reconnecting every
// ReadHeaderTimeout) locked every other client out, health probes
// included. Now the oldest one past the grace period gives way.
func TestMaxConnectionsSilentClients(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		name := "http"
		if useTLS {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			const maxConns = 4
			cfg := testConfig()
			cfg.Server.MaxConnections = maxConns
			if useTLS {
				cfg.Server.TLSCert, cfg.Server.TLSKey = writeSelfSigned(t)
			}
			srv := start(t, cfg)
			addr := srv.url[strings.Index(srv.url, "://")+3:]
			lim := srv.limiter.Load()
			if lim == nil {
				t.Fatal("no connection limiter")
			}
			const grace = 100 * time.Millisecond
			lim.mu.Lock()
			lim.grace = grace
			lim.mu.Unlock()

			silent := make([]net.Conn, maxConns)
			for i := range silent {
				c, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				silent[i] = c
			}
			waitFor(t, 5*time.Second, "the silent connections to be counted", func() bool {
				open, _, _ := lim.conns()
				return open == maxConns
			})
			time.Sleep(2 * grace)

			for i := range maxConns - 1 {
				c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
					Proxy:           nil,
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed test certificate
				}}
				resp, err := c.Get(srv.url + "/_gos3/health")
				if err != nil {
					t.Fatalf("health probe %d with %d silent connections open: %v", i, maxConns, err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("health probe %d: %d", i, resp.StatusCode)
				}
				c.CloseIdleConnections()
			}
			// A silent connection made room for the first probe (which
			// one is not checked: on Windows connections accepted within
			// one clock tick are equally old). They are read concurrently:
			// once a deadline has passed, Read reports a timeout even on a
			// closed connection.
			closedc := make(chan bool, len(silent))
			for _, c := range silent {
				go func() {
					_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
					_, err := c.Read(make([]byte, 1))
					ne, ok := err.(net.Error)
					closedc <- err != nil && (!ok || !ne.Timeout())
				}()
			}
			evicted := 0
			for range silent {
				if <-closedc {
					evicted++
				}
			}
			if evicted == 0 || evicted == maxConns {
				t.Errorf("%d of %d silent connections closed, want 1 to %d", evicted, maxConns, maxConns-1)
			}
		})
	}
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

// Later restic windows can take longer to prepare than the write-idle
// timeout. A healthy HTTP/2 client must be able to receive the next window
// after either a successful Write or an explicit Flush.
func TestHTTP2ResponsePreparationPause(t *testing.T) {
	for _, flush := range []bool{false, true} {
		t.Run(fmt.Sprintf("flush=%t", flush), func(t *testing.T) {
			const idle = 300 * time.Millisecond
			first := bytes.Repeat([]byte("a"), 128<<10)
			if flush {
				first = first[:64]
			}
			s := &Server{writeIdle: idle}
			handlerErr := make(chan error, 1)
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w, finish := s.guardWrites(w)
				defer finish()
				if _, err := w.Write(first); err != nil {
					handlerErr <- err
					return
				}
				if flush {
					if err := http.NewResponseController(w).Flush(); err != nil {
						handlerErr <- err
						return
					}
				}
				time.Sleep(3 * idle)
				_, err := io.WriteString(w, "next window")
				handlerErr <- err
			}))
			ts.EnableHTTP2 = true
			ts.StartTLS()
			defer ts.Close()
			client := ts.Client()
			client.Timeout = 5 * time.Second
			resp, err := client.Get(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.ProtoMajor != 2 {
				t.Fatalf("protocol = %s, want HTTP/2", resp.Proto)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("download interrupted while preparing the next window: %v", err)
			}
			want := append(bytes.Clone(first), []byte("next window")...)
			if !bytes.Equal(got, want) {
				t.Fatalf("downloaded %d bytes, want %d", len(got), len(want))
			}
			if err := <-handlerErr; err != nil {
				t.Fatalf("handler write: %v", err)
			}
		})
	}
}
