package s3

import (
	"bufio"
	"bytes"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// rawRequest sends head and the first part of a body on a raw connection
// and returns the connection and a reader for the response.
func rawRequest(t *testing.T, e *testEnv, head string, body []byte) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(e.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Write(append([]byte(head), body...)); err != nil {
		t.Fatal(err)
	}
	return c, bufio.NewReader(c)
}

// readResponseWithin reads a response, failing when none arrives in d.
func readResponseWithin(t *testing.T, c net.Conn, br *bufio.Reader, method string, d time.Duration) *http.Response {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(d))
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		t.Fatalf("no response within %v: %v", d, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp
}

// TestEarlyErrorDoesNotWaitForBody: a request rejected before its body is
// read gets its error after at most the grace period with
// "Connection: close"; net/http does not wait without limit for (and
// discard) the rest of a slowly sent body. A body that is already there is
// drained, so the connection stays reusable.
func TestEarlyErrorDoesNotWaitForBody(t *testing.T) {
	e := newTestEnv(t, withServer(func(s *Server) { s.unreadBodyGrace = 300 * time.Millisecond }))
	e.createBucket("rev")
	c, br := rawRequest(t, e, "PUT /rev/x HTTP/1.1\r\nHost: h\r\nContent-Length: 100\r\n\r\n", []byte("0123456789"))
	resp := readResponseWithin(t, c, br, "PUT", 3*time.Second)
	if resp.StatusCode != 403 || !resp.Close { // resp.Close: "Connection: close"
		t.Fatalf("got %d close=%v", resp.StatusCode, resp.Close)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("connection not closed after the error: %v", err)
	}

	// A complete body is drained and the connection kept.
	c2, br2 := rawRequest(t, e, "PUT /rev/y HTTP/1.1\r\nHost: h\r\nContent-Length: 3\r\n\r\n", []byte("abc"))
	if resp := readResponseWithin(t, c2, br2, "PUT", 3*time.Second); resp.StatusCode != 403 || resp.Close {
		t.Fatalf("second: %d close=%v", resp.StatusCode, resp.Close)
	}
	if _, err := c2.Write([]byte("GET /rev/y HTTP/1.1\r\nHost: h\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if resp := readResponseWithin(t, c2, br2, "GET", 3*time.Second); resp.StatusCode != 403 {
		t.Fatalf("reused connection: %d", resp.StatusCode)
	}

	// More than net/http's 256 KiB drain limit: closed without waiting.
	c3, br3 := rawRequest(t, e, "PUT /rev/z HTTP/1.1\r\nHost: h\r\nContent-Length: 1000000\r\n\r\n", nil)
	go c3.Write(bytes.Repeat([]byte("z"), 300<<10))
	if resp := readResponseWithin(t, c3, br3, "PUT", 3*time.Second); resp.StatusCode != 403 || !resp.Close {
		t.Fatalf("large body: %d close=%v", resp.StatusCode, resp.Close)
	}
}

// TestStalledBodyTimesOut: a body that stops arriving fails with
// RequestTimeout after the idle timeout instead of holding the connection.
func TestStalledBodyTimesOut(t *testing.T) {
	e := newTestEnv(t, withServer(func(s *Server) { s.bodyIdleTimeout = 300 * time.Millisecond }))
	requireAuthz(t, e)
	e.createBucket("pubw")
	e.putBucketConfig("pubw", "policy", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::pubw/*"}]}`)
	c, br := rawRequest(t, e, "PUT /pubw/x HTTP/1.1\r\nHost: h\r\nContent-Length: 100\r\n\r\n", []byte("0123456789"))
	start := time.Now()
	resp := readResponseWithin(t, c, br, "PUT", 5*time.Second)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(body), "<Code>RequestTimeout</Code>") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
	if _, err := e.backend.GetObjectInfo(t.Context(), "pubw", "x", ""); err == nil {
		t.Fatal("partial object stored")
	}

	// A body that keeps arriving within the idle timeout is fine.
	c2, br2 := rawRequest(t, e, "PUT /pubw/y HTTP/1.1\r\nHost: h\r\nContent-Length: 6\r\n\r\n", []byte("ab"))
	for _, s := range []string{"cd", "ef"} {
		time.Sleep(150 * time.Millisecond)
		c2.Write([]byte(s))
	}
	if resp := readResponseWithin(t, c2, br2, "PUT", 5*time.Second); resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow but steady upload: %d %s", resp.StatusCode, b)
	}
}

// TestPostObjectPreambleDeadline: the unauthenticated form fields of a POST
// upload must arrive within a fixed time, even when trickled.
func TestPostObjectPreambleDeadline(t *testing.T) {
	e := newTestEnv(t, withServer(func(s *Server) { s.postPreambleTimeout = 500 * time.Millisecond }))
	e.createBucket("rev")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("key", strings.Repeat("k", 200))
	head := "POST /rev HTTP/1.1\r\nHost: h\r\nContent-Type: " + mw.FormDataContentType() + "\r\nContent-Length: 100000\r\n\r\n"
	c, br := rawRequest(t, e, head, nil)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		data := buf.Bytes()
		for i := 0; i < len(data); i++ {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := c.Write(data[i : i+1]); err != nil {
				return
			}
		}
	}()
	start := time.Now()
	resp := readResponseWithin(t, c, br, "POST", 5*time.Second)
	if resp.StatusCode != 400 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

// TestBodyDeadlinesOverHTTP2: the read deadlines also work on HTTP/2
// streams: normal uploads succeed (and the deadline set while reading does
// not fire later) and a stalled body fails with RequestTimeout.
func TestBodyDeadlinesOverHTTP2(t *testing.T) {
	e := newTestEnv(t, withHTTP2(), withServer(func(s *Server) { s.bodyIdleTimeout = 300 * time.Millisecond }))
	e.createBucket("b")
	resp := e.signed("PUT", "/b/k", bytes.Repeat([]byte("x"), 1<<20), nil)
	if resp.ProtoMajor != 2 || resp.StatusCode != 200 {
		t.Fatalf("PUT over %s: %d %s", resp.Proto, resp.StatusCode, resp.Data)
	}
	// A handler that keeps working after reading its body is not cut off.
	e.override("PutObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		if _, err := io.ReadAll(r.Body); err != nil {
			return err
		}
		time.Sleep(600 * time.Millisecond)
		if err := r.Context().Err(); err != nil {
			return err
		}
		writeEmpty(w, http.StatusOK)
		return nil
	})
	if resp := e.signed("PUT", "/b/k2", []byte("data"), nil); resp.StatusCode != 200 {
		t.Fatalf("slow handler: %d %s", resp.StatusCode, resp.Data)
	}
	e.override("PutObject", nil)

	pr, pw := io.Pipe()
	defer pw.Close()
	req := e.newRequest("PUT", "/b/stalled", nil, objHdr("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD"))
	req.Body, req.ContentLength = pr, 100
	if err := signRootUnsigned(req); err != nil {
		t.Fatal(err)
	}
	go pw.Write([]byte("0123456789"))
	start := time.Now()
	r := e.do(req)
	if r.StatusCode != 400 || r.code() != "RequestTimeout" || time.Since(start) > 3*time.Second {
		t.Fatalf("stalled body: %d %s after %v", r.StatusCode, r.Data, time.Since(start))
	}
}

// signRootUnsigned signs req with the root credentials and an unsigned
// payload.
func signRootUnsigned(req *http.Request) error {
	return auth.SignRequest(req, testRoot, "us-east-1", "s3", auth.PayloadUnsigned, time.Now())
}

// TestBodyDeadlineClearedAfterEOF (HTTP/1.1): once the body is read, the
// idle deadline is cleared, so net/http's background read cannot time out
// and cancel the request context of a handler that is still working.
func TestBodyDeadlineClearedAfterEOF(t *testing.T) {
	e := newTestEnv(t, withServer(func(s *Server) { s.bodyIdleTimeout = 300 * time.Millisecond }))
	e.createBucket("b")
	e.override("PutObject", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		if _, err := io.ReadAll(r.Body); err != nil {
			return err
		}
		time.Sleep(600 * time.Millisecond)
		if err := r.Context().Err(); err != nil {
			return err
		}
		writeEmpty(w, http.StatusOK)
		return nil
	})
	for _, body := range [][]byte{[]byte("data"), bytes.Repeat([]byte("y"), 1<<20)} {
		if resp := e.signed("PUT", "/b/k", body, nil); resp.StatusCode != 200 || resp.Close {
			t.Fatalf("slow handler: %d close=%v %s", resp.StatusCode, resp.Close, resp.Data)
		}
	}
}

// TestClientAbortRecordedAs499: an upload whose client disconnects is
// logged and counted with status 499, not as a 400 client error.
func TestClientAbortRecordedAs499(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("pubw")
	e.putBucketConfig("pubw", "policy", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::pubw/*"}]}`)
	c, _ := rawRequest(t, e, "PUT /pubw/x HTTP/1.1\r\nHost: h\r\nContent-Length: 1000\r\n\r\n", []byte("0123456789"))
	time.Sleep(50 * time.Millisecond)
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(e.logs.String(), `"client_aborted":true`) {
		if time.Now().After(deadline) {
			t.Fatalf("no client_aborted access log line:\n%s", e.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(e.logs.String(), `"status":499`) {
		t.Fatalf("status not 499:\n%s", e.logs.String())
	}
}
