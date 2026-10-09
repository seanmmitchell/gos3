package s3

// Keep-alive responses for long-running multipart operations
// (CompleteMultipartUpload, UploadPartCopy). Owned by the s3-multipart work
// item.
//
// Like AWS, when the work takes longer than a short delay the 200 status
// line, the XML declaration and then periodic whitespace are sent so that
// clients and proxies do not time the connection out. If the work then
// fails, the error document is embedded in the 200 response body; the AWS
// SDKs detect such a body (root element <Error>) and surface the error.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// newAtomicInt64 returns an atomic.Int64 holding v.
func newAtomicInt64(v int64) *atomic.Int64 {
	a := new(atomic.Int64)
	a.Store(v)
	return a
}

// Keep-alive timing. They are atomics so tests can lower them while
// servers are running.
var (
	// keepAliveDelay is how long a long-running operation may take before
	// the 200 header is sent early (nanoseconds).
	keepAliveDelay = newAtomicInt64(int64(5 * time.Second))
	// keepAliveInterval is the period of the whitespace written while the
	// operation is still running (nanoseconds).
	keepAliveInterval = newAtomicInt64(int64(5 * time.Second))
)

// longResult is the outcome of a long-running operation.
type longResult struct {
	// doc is the XML response document written on success.
	doc any
	// headers are response headers added on success. Once the response has
	// started only the names declared as trailers can still be sent (as
	// HTTP trailers); others are dropped.
	headers http.Header
	err     error
}

// respondWithKeepAlive runs work in its own goroutine and writes its
// result. When work finishes within keepAliveDelay the response is
// ordinary: an error is returned for the core to render, or early +
// result headers and the XML document are written with status 200.
// Otherwise the 200 header (with the early headers and the declared
// trailers) and the XML declaration are sent at once, whitespace is written
// every keepAliveInterval, and the result (or an <Error> document) follows
// when work completes. respondWithKeepAlive always waits for work to
// return, so work may use w-independent request state freely, but it must
// not write to w or touch rc.
func (s *Server) respondWithKeepAlive(w http.ResponseWriter, r *http.Request, rc *reqContext,
	early http.Header, trailers []string, work func() longResult) error {
	opName := rc.opName
	done := make(chan longResult, 1)
	go func() {
		var res longResult
		defer func() {
			if p := recover(); p != nil {
				s.log.LogAttrs(r.Context(), slog.LevelError, "s3: panic in long-running operation",
					slog.String("op", opName), slog.String("panic", fmt.Sprint(p)), slog.String("stack", string(debug.Stack())))
				res = longResult{err: fmt.Errorf("s3: panic in %s: %v", opName, p)}
			}
			done <- res
		}()
		res = work()
	}()

	timer := time.NewTimer(time.Duration(keepAliveDelay.Load()))
	defer timer.Stop()
	select {
	case res := <-done:
		if res.err != nil {
			return res.err
		}
		h := w.Header()
		copyHeaders(h, early)
		copyHeaders(h, res.headers)
		return writeXML(w, http.StatusOK, res.doc)
	case <-timer.C:
	}

	h := w.Header()
	copyHeaders(h, early)
	h.Set("Content-Type", "application/xml")
	h.Del("Content-Length")
	for _, t := range trailers {
		h.Add("Trailer", t)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xmlHeader)
	flush(w)
	rc.addLog(slog.Bool("keep_alive", true))

	ticker := time.NewTicker(time.Duration(max(keepAliveInterval.Load(), int64(time.Millisecond))))
	defer ticker.Stop()
	for {
		select {
		case res := <-done:
			s.finishKeepAlive(w, r, rc, trailers, res)
			return nil
		case <-ticker.C:
			// A failed write means the client is gone; keep waiting so the
			// operation's goroutine never outlives the request.
			_, _ = io.WriteString(w, " ")
			flush(w)
		}
	}
}

// finishKeepAlive writes the result of a long-running operation after the
// 200 header has been sent: the XML document (and declared trailers), or
// the error document embedded in the body.
func (s *Server) finishKeepAlive(w http.ResponseWriter, r *http.Request, rc *reqContext, trailers []string, res longResult) {
	var buf bytes.Buffer
	if res.err != nil {
		e := s.toS3Error(r.Context(), rc, res.err)
		rc.errCode = e.Code
		s.log.LogAttrs(r.Context(), slog.LevelWarn, "s3: operation failed after the response started",
			slog.String("request_id", rc.requestID), slog.String("op", rc.opName), slog.String("error", res.err.Error()))
		doc := errorDoc{
			Code:      e.Code,
			Message:   e.Message,
			Extra:     e.Extra,
			Resource:  errorResource(r),
			RequestID: rc.requestID,
			HostID:    s.hostID(rc.requestID),
		}
		if err := xml.NewEncoder(&buf).Encode(doc); err != nil {
			buf.Reset()
			buf.WriteString("<Error><Code>InternalError</Code></Error>")
		}
	} else {
		if err := xml.NewEncoder(&buf).Encode(res.doc); err != nil {
			s.log.LogAttrs(r.Context(), slog.LevelError, "s3: encode response",
				slog.String("request_id", rc.requestID), slog.String("error", err.Error()))
			rc.errCode = "InternalError"
			buf.Reset()
			buf.WriteString("<Error><Code>InternalError</Code><Message>We encountered an internal error. Please try again.</Message></Error>")
		} else {
			h := w.Header()
			for _, t := range trailers {
				if v := res.headers.Get(t); v != "" {
					h.Set(t, v)
				}
			}
		}
	}
	_, _ = w.Write(buf.Bytes())
	flush(w)
}

// copyHeaders sets every header of src on dst.
func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		dst[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
}

// flush flushes w when it supports it.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
