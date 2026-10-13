package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// Connection limits of the HTTP listener (see Serve). HTTP/2 multiplexes
// requests on one connection, so its per-connection stream limit bounds
// how many handlers one client connection can keep busy.
const (
	// responseWriteIdle is how long one write of a response body may make
	// no progress (the client does not read, or keeps an HTTP/2 flow
	// control window closed) before the request is aborted. Transfers
	// that make progress are never cut off.
	responseWriteIdle = 2 * time.Minute
	// maxGuardedWrite is the largest piece of a response written under
	// one write deadline: a transfer must move at least this much per
	// responseWriteIdle (about 550 bytes/s).
	maxGuardedWrite = 64 << 10
	// http2MaxStreams is the number of concurrent HTTP/2 streams per
	// connection (net/http's default is 250).
	http2MaxStreams = 100
	// http2PingAfter and http2PingTimeout detect dead HTTP/2 connections.
	http2PingAfter   = 30 * time.Second
	http2PingTimeout = 15 * time.Second
)

// writeGuard bounds the writes of a response: before each write (of at
// most maxGuardedWrite bytes) it makes sure the write deadline of the
// connection (HTTP/1) or stream (HTTP/2) is at least 3/4 of idle away,
// re-arming it to now+idle only when the last arming is more than idle/4
// old (on HTTP/2 every arming is a message to the connection's serve loop
// and a timer reset, so it is not done per write). A client that stops
// reading, or that never opens its HTTP/2 flow control window, then
// releases the handler goroutine, its buffers and open files within idle
// instead of holding them forever. The deadline stays armed between
// writes: a handler that pauses for most of idle after writing part of a
// response (none of gos3's do; slow operations write keep-alive bytes
// every few seconds) may be cut off on HTTP/2. Body reads are bounded
// separately (bodyguard.go, s3 countingBody).
type writeGuard struct {
	http.ResponseWriter
	ctl     *http.ResponseController // nil: deadlines unsupported
	idle    time.Duration
	armedAt time.Time // when the deadline was last set to armedAt+idle (zero: not armed)
}

// guardWrites wraps w in a writeGuard. The returned function must be
// called (deferred) when the handler returns: it arms the deadline for
// the rest of the response, which net/http writes after the handler
// returned (buffered data, the final chunk).
func (s *Server) guardWrites(w http.ResponseWriter) (http.ResponseWriter, func()) {
	if s.writeIdle <= 0 {
		return w, func() {}
	}
	g := &writeGuard{ResponseWriter: w, ctl: http.NewResponseController(w), idle: s.writeIdle}
	// An HTTP/1 connection keeps the deadline armed for the end of the
	// previous response on it.
	g.set(time.Time{})
	return g, func() { g.arm(true) }
}

// arm sets the deadline to now+idle unless that was done less than idle/4
// ago (force: always).
func (g *writeGuard) arm(force bool) {
	now := time.Now()
	if !force && !g.armedAt.IsZero() && now.Sub(g.armedAt) < g.idle/4 {
		return
	}
	g.set(now.Add(g.idle))
	g.armedAt = now
}

// set sets the write deadline, disabling the guard when the writer does
// not support deadlines.
func (g *writeGuard) set(t time.Time) {
	if g.ctl == nil {
		return
	}
	if err := g.ctl.SetWriteDeadline(t); err != nil {
		g.ctl = nil
	}
}

func (g *writeGuard) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return g.ResponseWriter.Write(p)
	}
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxGuardedWrite)]
		g.arm(false)
		m, err := g.ResponseWriter.Write(chunk)
		n += m
		if err != nil {
			// The deadline stays: an expired one keeps net/http from
			// waiting for the client when it finishes the response.
			return n, err
		}
		p = p[m:]
	}
	return n, nil
}

// Flush implements http.Flusher when the underlying writer does.
func (g *writeGuard) Flush() {
	f, ok := g.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	g.arm(false)
	f.Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (g *writeGuard) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// connLimiter caps the open client connections (server.max_connections)
// without blocking Accept: when the cap is reached, a new connection first
// makes room by closing an idle keep-alive connection; when every open
// connection is busy the new one is closed at once. A plain semaphore
// around Accept (netutil.LimitListener) would let one client that keeps
// idle keep-alive connections (or half-sent headers) open hold every slot
// and make health probes and other clients wait indefinitely. Connections
// are counted through http.Server.ConnState (HTTP/2 connections report
// idle when they have no open streams).
type connLimiter struct {
	max int
	log func(msg string, args ...any)

	mu   sync.Mutex
	open int
	idle map[net.Conn]time.Time // idle connections, by when they became idle
}

func newConnLimiter(max int, log func(string, ...any)) *connLimiter {
	return &connLimiter{max: max, log: log, idle: map[net.Conn]time.Time{}}
}

// connState is the http.Server ConnState hook.
func (l *connLimiter) connState(c net.Conn, st http.ConnState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch st {
	case http.StateNew:
		l.open++
	case http.StateIdle:
		l.idle[c] = time.Now()
	case http.StateActive:
		delete(l.idle, c)
	case http.StateClosed, http.StateHijacked:
		if _, ok := l.idle[c]; ok {
			delete(l.idle, c)
		}
		l.open--
	}
}

// admit reports whether a newly accepted connection may be served, closing
// the longest-idle keep-alive connection to make room when the cap is
// reached.
func (l *connLimiter) admit() bool {
	l.mu.Lock()
	if l.open < l.max {
		l.mu.Unlock()
		return true
	}
	var oldest net.Conn
	var since time.Time
	for c, t := range l.idle {
		if oldest == nil || t.Before(since) {
			oldest, since = c, t
		}
	}
	if oldest != nil {
		delete(l.idle, oldest)
	}
	l.mu.Unlock()
	if oldest == nil {
		return false
	}
	_ = oldest.Close() // reported as StateClosed by net/http
	return true
}

// limitedListener applies a connLimiter to Accept.
type limitedListener struct {
	net.Listener
	l *connLimiter
}

func (ln limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := ln.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ln.l.admit() {
			return c, nil
		}
		ln.l.log("connection refused: server.max_connections reached and no connection is idle",
			"max", ln.l.max, "remote", c.RemoteAddr().String())
		_ = c.Close()
	}
}

// connTLSKey is the context key of the *tls.Conn a request arrived on
// (set by connContext).
type connTLSKey struct{}

// connContext is the http.Server ConnContext: it remembers TLS connections
// in the connection's context.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		return context.WithValue(ctx, connTLSKey{}, tc)
	}
	return ctx
}

// withConnTLS returns r with TLS set from the connection it arrived on.
// net/http's HTTP/2 server leaves Request.TLS nil when the client sends the
// ":scheme http" pseudo-header, even on a TLS connection; the TLS state
// (aws:SecureTransport, s3:TlsVersion) must come from the connection, not
// from a value the client chooses. The request is copied (shallowly) only
// when TLS is filled in.
func withConnTLS(r *http.Request) *http.Request {
	if r.TLS != nil {
		return r
	}
	tc, ok := r.Context().Value(connTLSKey{}).(*tls.Conn)
	if !ok {
		return r
	}
	st := tc.ConnectionState()
	if !st.HandshakeComplete {
		return r
	}
	r = r.WithContext(r.Context())
	r.TLS = &st
	return r
}
