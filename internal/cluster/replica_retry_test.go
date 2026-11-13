package cluster

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReplicaRequestsReplayAfterReusedConnectionCloses(t *testing.T) {
	for _, path := range []string{"/stage", "/persist", "/release"} {
		t.Run(path, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/warm" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				if attempts.Add(1) == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport}
			res, err := client.Get(server.URL + "/warm")
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			replica := remoteReplica{peer: peerClient{name: "peer", origin: server.URL, client: client}}
			body := []byte("payload")
			digest := sha256.Sum256(body)
			if err := replica.request(context.Background(), path, "operation", body, &digest); err != nil {
				t.Fatalf("lost response on a reused connection aborted an idempotent replica request: %v", err)
			}
			if attempts.Load() != 2 {
				t.Fatalf("attempts = %d, want replay", attempts.Load())
			}
		})
	}
}
