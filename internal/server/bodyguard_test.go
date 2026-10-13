package server

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// readResponse reads one response from conn within timeout.
func readResponse(t *testing.T, conn net.Conn, br *bufio.Reader, timeout time.Duration) *http.Response {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("no response within %v: %v", timeout, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// expectClosed checks that the server closes conn after the response.
func expectClosed(t *testing.T, conn net.Conn, br *bufio.Reader) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("connection not closed after the response: %v", err)
	}
}

// TestStalledBodyOnSystemEndpoints sends requests that announce a small body
// (below net/http's 256 KiB post-handler drain limit) and never send it.
// The response must arrive at once, with "Connection: close". Previously
// net/http tried to discard the unread body after the handler returned,
// without any deadline, so the response was never sent and the connection
// and its goroutine were held forever – without any credentials.
func TestStalledBodyOnSystemEndpoints(t *testing.T) {
	cfg := testConfig()
	cfg.UI.Enabled = true
	srv := start(t, cfg, WithUIFS(fakeUI()))
	addr := srv.Addr()
	for _, tc := range []struct {
		name, method, path string
		status             int
	}{
		{"health", "GET", HealthPath, http.StatusOK},
		{"info", "GET", InfoPath, http.StatusOK},
		{"ui", "GET", "/_gos3/ui/", http.StatusOK},
		{"unknown", "POST", "/_gos3/nope", http.StatusNotFound},
		{"admin anonymous", "PUT", "/_gos3/admin/v1/config", http.StatusUnauthorized},
		{"admin chunked", "POST", "/_gos3/admin/v1/users", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			framing := "Content-Length: 10\r\n"
			if strings.Contains(tc.name, "chunked") {
				framing = "Transfer-Encoding: chunked\r\n"
			}
			req := tc.method + " " + tc.path + " HTTP/1.1\r\nHost: " + addr + "\r\n" + framing + "\r\n"
			if _, err := io.WriteString(conn, req); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(conn)
			resp := readResponse(t, conn, br, 3*time.Second)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if !resp.Close {
				t.Errorf("response without Connection: close (header %v)", resp.Header)
			}
			expectClosed(t, conn, br)
		})
	}
}

// TestSystemBodyKeepAlive checks that requests whose body was read keep
// their connection.
func TestSystemBodyKeepAlive(t *testing.T) {
	srv := start(t, testConfig())
	for i := range 2 {
		body := []byte(`{"name":"keepalive` + string(rune('a'+i)) + `"}`)
		req, err := http.NewRequest("POST", srv.url+"/_gos3/admin/v1/users", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", "", time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if resp.Close {
			t.Fatal("a fully read request body must not close the connection")
		}
	}
}

// TestStalledBodyIdleTimeout sends an authenticated admin request whose body
// stops after a few bytes: the body read must fail after the idle timeout
// and the server must answer and close the connection.
func TestStalledBodyIdleTimeout(t *testing.T) {
	s, err := New(testConfig(), testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	s.bodyIdle = 300 * time.Millisecond
	srv := serve(t, s)

	body := []byte(`{"name":"` + strings.Repeat("x", 90) + `"}`)
	req, err := http.NewRequest("PUT", srv.url+"/_gos3/admin/v1/config", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// "" signs the body's SHA-256: the admin API refuses unsigned bodies
	// before reading them, which would skip the stalled read under test.
	if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		t.Fatal(err)
	}
	wire := buf.Bytes()[:buf.Len()-len(body)+5] // headers and 5 body bytes

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	began := time.Now()
	resp := readResponse(t, conn, br, 5*time.Second)
	if resp.StatusCode < 400 {
		t.Errorf("status = %d, want an error", resp.StatusCode)
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("answered after %v", d)
	}
	expectClosed(t, conn, br)
}
