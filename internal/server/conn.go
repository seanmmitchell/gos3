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
// without blocking Accept. When the cap is reached, a new connection makes
// room by closing, in this order:
//
//   - the longest-idle keep-alive connection;
//   - else the oldest connection that has not started its first request
//     (it sent nothing, only part of the request headers, or has not
//     finished its TLS handshake) and has been open for at least the grace
//     period (pendingGrace), which lets clients on slow links get their
//     first request in.
//
// When neither exists (every open connection has a request in flight, or
// is younger than the grace period) the new connection is closed at once.
// A connection with a request in flight is never closed. A plain semaphore
// around Accept (netutil.LimitListener), or a limiter that only evicts idle
// connections, would let one client that keeps idle keep-alive connections,
// silent sockets or half-sent headers open hold every slot (until
// ReadHeaderTimeout, reconnecting after it) and lock health probes and
// other clients out. Connections are counted through
// http.Server.ConnState: net/http reports a connection new until its first
// request's headers are read, and HTTP/2 connections report idle when they
// have no open streams.
type connLimiter struct {
	max int
	log func(msg string, args ...any)

	mu      sync.Mutex
	grace   time.Duration // pendingGrace, shorter in tests
	open    int
	idle    map[net.Conn]time.Time // idle connections, by when they became idle
	pending map[net.Conn]time.Time // StateNew connections, by when they were accepted
}

// pendingGrace is how long a connection that has not started its first
// request is safe from eviction by newer connections.
const pendingGrace = time.Second

func newConnLimiter(max int, log func(string, ...any)) *connLimiter {
	return &connLimiter{
		max: max, log: log, grace: pendingGrace,
		idle:    map[net.Conn]time.Time{},
		pending: map[net.Conn]time.Time{},
	}
}

// connState is the http.Server ConnState hook.
func (l *connLimiter) connState(c net.Conn, st http.ConnState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch st {
	case http.StateNew:
		l.open++
		l.pending[c] = time.Now()
	case http.StateIdle:
		delete(l.pending, c)
		l.idle[c] = time.Now()
	case http.StateActive:
		delete(l.pending, c)
		delete(l.idle, c)
	case http.StateClosed, http.StateHijacked:
		delete(l.pending, c)
		delete(l.idle, c)
		l.open--
	}
}

// conns returns the number of connections counted as open and, of those,
// as idle and as pending (not yet started their first request).
func (l *connLimiter) conns() (open, idle, pending int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.open, len(l.idle), len(l.pending)
}

// admit reports whether a newly accepted connection may be served. When
// the cap is reached it makes room by closing the longest-idle keep-alive
// connection or, when none is idle, the oldest pending connection past the
// grace period; it reports false when there is neither. The closed
// connection stays counted as open until net/http reports it closed.
func (l *connLimiter) admit() bool {
	l.mu.Lock()
	if l.open < l.max {
		l.mu.Unlock()
		return true
	}
	victim := oldestConn(l.idle, time.Time{})
	if victim != nil {
		delete(l.idle, victim)
	} else if victim = oldestConn(l.pending, time.Now().Add(-l.grace)); victim != nil {
		delete(l.pending, victim)
	}
	l.mu.Unlock()
	if victim == nil {
		return false
	}
	_ = victim.Close() // reported as StateClosed by net/http
	return true
}

// oldestConn returns the connection of m with the earliest time, ignoring
// those after notAfter (unless it is zero); nil when there is none.
func oldestConn(m map[net.Conn]time.Time, notAfter time.Time) net.Conn {
	var oldest net.Conn
	var since time.Time
	for c, t := range m {
		if !notAfter.IsZero() && t.After(notAfter) {
			continue
		}
		if oldest == nil || t.Before(since) {
			oldest, since = c, t
		}
	}
	return oldest
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
		ln.l.log("connection refused: server.max_connections reached and no connection is idle or waiting past the grace period for its first request",
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
