package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestExpectContinueEarlyError sends requests with "Expect: 100-continue"
// whose body is never sent and which are rejected before the body is read
// (unknown bucket; unauthenticated admin call). The error response must
// arrive immediately: botocore and the AWS CLI wait for it, or for
// "100 Continue", before uploading. Previously the S3 handler and the admin
// metrics wrapper replaced the server's own Request.Body, which made
// net/http try to drain the unread body first, so clients only saw the
// error after their 100-continue timeout.
func TestExpectContinueEarlyError(t *testing.T) {
	srv := start(t, testConfig())
	addr := srv.Addr()
	// An Authorization header with an unknown access key: rejected by the
	// verifier before anything reads (or closes) the body.
	badAuth := "Authorization: AWS4-HMAC-SHA256 Credential=NOSUCHKEY/20260101/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-date, Signature=" + strings.Repeat("0", 64) + "\r\n" +
		"X-Amz-Date: " + time.Now().UTC().Format("20060102T150405Z") + "\r\n" +
		"X-Amz-Content-Sha256: UNSIGNED-PAYLOAD\r\n"
	for _, tc := range []struct{ name, method, path, hdr string }{
		{"s3", "PUT", "/no-such-bucket/key", ""},
		{"s3 bad auth", "PUT", "/no-such-bucket/key", badAuth},
		{"admin", "PUT", "/_gos3/admin/v1/config", ""},
		{"admin bad auth", "PUT", "/_gos3/admin/v1/config", badAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			req := tc.method + " " + tc.path + " HTTP/1.1\r\nHost: " + addr + "\r\n" +
				tc.hdr +
				"Content-Type: application/json\r\nContent-Length: 1048576\r\nExpect: 100-continue\r\n\r\n"
			if _, err := io.WriteString(conn, req); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			began := time.Now()
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("no response without the body: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Fatalf("status = %d, want an error", resp.StatusCode)
			}
			if d := time.Since(began); d > 500*time.Millisecond {
				t.Fatalf("error response took %v; the server waited for the body", d)
			}
			if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "<Error>") && !strings.Contains(string(b), `"error"`) {
				t.Errorf("unexpected body %q", b)
			}
		})
	}
}
