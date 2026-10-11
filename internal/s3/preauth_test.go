package s3

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestPreAuthBodyDeadline: the operations that read their body before
// authorizing the caller (AuthzByHandler) give an anonymous client a fixed
// time to send it; trickling a byte now and then (well within the idle
// timeout) does not hold the request open.
func TestPreAuthBodyDeadline(t *testing.T) {
	const limit = 400 * time.Millisecond
	e := newTestEnv(t, withServer(func(s *Server) { s.preAuthBodyTimeout = limit }))
	e.createBucket("bkt")
	for _, tc := range []struct{ name, head string }{
		{"PutObjectTagging without bucket", "PUT /nobkt/k?tagging HTTP/1.1\r\nHost: h\r\nContent-Length: 65536\r\n\r\n"},
		{"DeleteObjects", "POST /bkt?delete HTTP/1.1\r\nHost: h\r\nContent-Length: 1000000\r\n\r\n"},
		{"PutObjectRetention", "PUT /bkt/k?retention HTTP/1.1\r\nHost: h\r\nContent-Length: 65536\r\n\r\n"},
		{"PutObjectLegalHold", "PUT /bkt/k?legal-hold HTTP/1.1\r\nHost: h\r\nContent-Length: 65536\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, br := rawRequest(t, e, tc.head, []byte("<"))
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				for {
					select {
					case <-stop:
						return
					case <-time.After(100 * time.Millisecond):
					}
					if _, err := c.Write([]byte(" ")); err != nil {
						return
					}
				}
			}()
			start := time.Now()
			resp := readResponseWithin(t, c, br, "PUT", 5*time.Second)
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 400 || !strings.Contains(string(body), "<Code>RequestTimeout</Code>") {
				t.Fatalf("got %d %s", resp.StatusCode, body)
			}
			if d := time.Since(start); d < limit-50*time.Millisecond || d > 3*time.Second {
				t.Fatalf("answered after %v (limit %v)", d, limit)
			}
		})
	}
}

// TestPreAuthBodyDeadlineCleared: the deadline only covers the body read
// before authorization; once the handler has authorized the caller
// (authorizeOpWith), the rest of the body may take its time.
func TestPreAuthBodyDeadlineCleared(t *testing.T) {
	const limit = 300 * time.Millisecond
	e := newTestEnv(t, withServer(func(s *Server) { s.preAuthBodyTimeout = limit }))
	e.createBucket("bkt")
	for _, authorize := range []bool{true, false} {
		e.override("PutObjectTagging", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
			rc.in.mu.Lock()
			limited := !rc.in.limit.IsZero()
			rc.in.mu.Unlock()
			if !limited || !rc.preAuthLimit {
				return errors.New("no pre-authorization deadline set")
			}
			if _, err := io.ReadFull(r.Body, make([]byte, 4)); err != nil {
				return mapStorageError(err)
			}
			if authorize {
				if err := s.authorizeOpWith(r.Context(), r, rc, nil); err != nil {
					return err
				}
				if rc.preAuthLimit {
					return errors.New("deadline not cleared")
				}
			}
			if _, err := io.ReadAll(r.Body); err != nil {
				return mapStorageError(err)
			}
			writeEmpty(w, http.StatusOK)
			return nil
		})
		pr, pw := io.Pipe()
		req := e.newRequest("PUT", "/bkt/k?tagging", nil, objHdr("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD"))
		req.Body, req.ContentLength = pr, 8
		if err := signRootUnsigned(req); err != nil {
			t.Fatal(err)
		}
		go func() {
			defer pw.Close()
			if _, err := pw.Write([]byte("abcd")); err != nil {
				return
			}
			time.Sleep(2 * limit)
			_, _ = pw.Write([]byte("efgh"))
		}()
		resp := e.do(req)
		switch {
		case authorize && resp.StatusCode != 200:
			t.Fatalf("authorized: %d %s", resp.StatusCode, resp.Data)
		case !authorize && (resp.StatusCode != 400 || resp.code() != "RequestTimeout"):
			t.Fatalf("not authorized: %d %s", resp.StatusCode, resp.Data)
		}
	}

	// Real handlers: a legitimate request is unaffected.
	e.override("PutObjectTagging", nil)
	e.putObject("bkt", "k", []byte("x"), storage.ObjectMeta{})
	resp := e.signed("PUT", "/bkt/k?tagging", []byte(`<Tagging><TagSet><Tag><Key>a</Key><Value>b</Value></Tag></TagSet></Tagging>`), nil)
	objExpect(t, resp, 200, "")
}
