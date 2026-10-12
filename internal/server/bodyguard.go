package server

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// systemBodyIdleTimeout is how long a read of a request body sent to a gos3
// endpoint (admin API, UI, info, ...) may wait for the client to send more
// data. The S3 handler applies its own limits (see s3.Server.ServeHTTP).
const systemBodyIdleTimeout = 60 * time.Second

// guardBody protects the connection serving a request routed to one of
// gos3's own endpoints from a client that announces a request body and then
// does not send it:
//
//   - every Read of the body must make progress within s.bodyIdle (an idle
//     read deadline on the connection, re-armed as data arrives and cleared
//     once the body was read to its end);
//   - over HTTP/1.x, when the response starts while the body was not read
//     completely (typically an error answered before reading it, or an
//     endpoint that takes no body), the response says "Connection: close"
//     and the rest of the body is abandoned. Without this net/http reads and
//     discards up to 256 KiB of the unread body before sending the response,
//     with no deadline: a client sending "Content-Length: 10" and nothing
//     else would hold the goroutine and the connection forever and never get
//     an answer.
//
// It returns the writer and request to serve and a function that must be
// called (deferred) when the handler returns. The body is replaced on a
// shallow copy of r only: net/http must still see its own Request.Body.
func (s *Server) guardBody(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, func()) {
	if r.ContentLength == 0 || r.Body == nil || r.Body == http.NoBody {
		return w, r, func() {}
	}
	b := &guardedBody{rc: r.Body, ctl: http.NewResponseController(w), idle: s.bodyIdle}
	r = r.WithContext(r.Context())
	r.Body = b
	if r.ProtoMajor != 1 {
		// HTTP/2 does not drain unread request bodies.
		return w, r, func() {}
	}
	cl := r.ContentLength
	gw := &guardedWriter{ResponseWriter: w}
	gw.before = func() {
		if b.consumed(cl) {
			return
		}
		w.Header().Set("Connection", "close")
		b.abandon()
	}
	// A handler that writes nothing gets its response from net/http after
	// it returns, bypassing gw: run the check before that.
	return gw, r, gw.start
}

// guardedWriter runs before once, just before the response header is
// written.
type guardedWriter struct {
	http.ResponseWriter
	once   sync.Once
	before func()
}

func (w *guardedWriter) start() { w.once.Do(w.before) }

func (w *guardedWriter) WriteHeader(code int) {
	if code >= 200 || code == http.StatusSwitchingProtocols {
		w.start()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *guardedWriter) Write(p []byte) (int, error) {
	w.start()
	return w.ResponseWriter.Write(p)
}

// Flush implements http.Flusher when the underlying writer does.
func (w *guardedWriter) Flush() {
	w.start()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *guardedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// guardedBody is a request body whose reads are bounded by an idle read
// deadline on the connection.
type guardedBody struct {
	rc  io.ReadCloser
	n   atomic.Int64
	eof atomic.Bool

	mu      sync.Mutex
	ctl     *http.ResponseController // nil: deadlines unsupported
	idle    time.Duration            // 0: no idle deadline
	armedAt time.Time                // when the deadline was last set
}

func (b *guardedBody) Read(p []byte) (int, error) {
	b.arm()
	n, err := b.rc.Read(p)
	b.n.Add(int64(n))
	if err == io.EOF {
		b.eof.Store(true)
		b.clear()
	}
	// After any other error the deadline stays: an expired one keeps
	// net/http from waiting for the rest of a stalled body.
	return n, err
}

func (b *guardedBody) Close() error { return b.rc.Close() }

// consumed reports whether the whole body was read (EOF seen, or as many
// bytes as the declared Content-Length).
func (b *guardedBody) consumed(contentLength int64) bool {
	return b.eof.Load() || (contentLength > 0 && b.n.Load() >= contentLength)
}

// setDeadline sets the connection read deadline, disabling deadlines when
// the connection does not support them. b.mu must be held.
func (b *guardedBody) setDeadline(t time.Time) {
	if b.ctl == nil {
		return
	}
	if err := b.ctl.SetReadDeadline(t); err != nil {
		b.ctl = nil
	}
}

// arm sets the read deadline to now+idle before a Read; re-arming is
// skipped for a short while after the previous one.
func (b *guardedBody) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctl == nil || b.idle <= 0 {
		return
	}
	now := time.Now()
	if !b.armedAt.IsZero() && now.Sub(b.armedAt) < min(time.Second, b.idle/8) {
		return
	}
	b.setDeadline(now.Add(b.idle))
	b.armedAt = now
}

// clear removes the read deadline once the body was read to its end, so
// that net/http's background read of the idle connection is not cut short.
func (b *guardedBody) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.armedAt.IsZero() {
		b.setDeadline(time.Time{})
		b.armedAt = time.Time{}
	}
}

// abandon gives up on the rest of the body: the read deadline is set to now
// so that neither net/http's discard of the unread body nor a later Read
// waits for the client.
func (b *guardedBody) abandon() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.idle = 0
	b.armedAt = time.Time{}
	b.setDeadline(time.Now())
}
