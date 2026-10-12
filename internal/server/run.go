package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Run listens on the configured address and serves until ctx is done, then
// shuts down gracefully (see Serve).
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Server.Address)
	if err != nil {
		_ = s.Close()
		return fmt.Errorf("listen on %s: %w", s.cfg.Server.Address, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves HTTP (HTTPS when TLS is configured) on ln until ctx is done
// or serving fails. Once listening it starts discovery and the lifecycle
// worker and closes Ready. On ctx cancellation it stops accepting
// connections, marks /_gos3/ready unavailable, waits up to
// server.shutdown_timeout for in-flight requests (then closes the remaining
// connections), stops discovery and the lifecycle worker and closes the
// backend and IAM. It returns nil after a graceful shutdown. Serve closes
// ln and the server; it can be called only once.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if !s.serving.CompareAndSwap(false, true) {
		return errServing
	}
	defer s.Close()

	// Request contexts are not tied to ctx, so that cancelling ctx (SIGTERM)
	// lets in-flight requests finish within the shutdown timeout.
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	hs := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         s.tlsConfig,
		ErrorLog:          log.New(httpErrorWriter{s.log}, "", 0),
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ConnContext:       connContext,
		// HTTP/2 (offered with TLS): fewer concurrent streams per
		// connection than net/http's 250, dead connections detected by
		// pings, and a connection whose writes make no progress closed.
		// Stalled streams are aborted by writeGuard.
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: http2MaxStreams,
			SendPingTimeout:      http2PingAfter,
			PingTimeout:          http2PingTimeout,
			WriteByteTimeout:     responseWriteIdle,
		},
	}

	s.addr.Store(ln.Addr().String())
	if n := s.cfg.Server.MaxConnections; n > 0 {
		lim := newConnLimiter(n, func(msg string, args ...any) { s.log.Debug(msg, args...) })
		hs.ConnState = lim.connState
		ln = limitedListener{Listener: ln, l: lim}
	}
	if err := s.disc.Start(s.lifeCtx); err != nil {
		s.log.Error("discovery: start failed", "err", err)
	}
	s.lifecycle.Start(s.lifeCtx)

	errc := make(chan error, 1)
	go func() {
		if s.tlsConfig != nil {
			errc <- hs.ServeTLS(ln, "", "")
		} else {
			errc <- hs.Serve(ln)
		}
	}()
	close(s.ready)
	s.log.Info("gos3 listening", "addr", ln.Addr().String(), "url", s.URL(), "tls", s.tlsConfig != nil)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}

	s.draining.Store(true)
	timeout := s.cfg.Server.ShutdownTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	s.log.Info("shutting down: waiting for in-flight requests", "timeout", timeout)
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		s.log.Warn("graceful shutdown timed out; closing remaining connections", "err", err)
		_ = hs.Close()
	}
	cancelRequests()
	<-errc
	err := s.Close()
	if err != nil {
		s.log.Error("shutdown: closing resources failed", "err", err)
	} else {
		s.log.Info("shutdown complete")
	}
	return err
}

// httpErrorWriter routes net/http's internal error log to slog: TLS
// handshake noise (scanners, clients rejecting a self-signed certificate)
// at debug level, everything else as a warning.
type httpErrorWriter struct{ log *slog.Logger }

func (w httpErrorWriter) Write(p []byte) (int, error) {
	msg := string(bytes.TrimSpace(p))
	level := slog.LevelWarn
	if bytes.Contains(p, []byte("TLS handshake error")) {
		level = slog.LevelDebug
	}
	w.log.Log(context.Background(), level, "http server: "+msg)
	return len(p), nil
}
