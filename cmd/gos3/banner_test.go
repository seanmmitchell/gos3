package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/server"
)

// TestServeGeneratedSecretNotInOutput: a generated root secret key is never
// written to the log, and the banner shows it only on a terminal. Without a
// terminal (docker, journald, Kubernetes collect the output) an ephemeral
// server writes its temporary keys to a private file, named in the banner
// and removed on exit.
func TestServeGeneratedSecretNotInOutput(t *testing.T) {
	clearEnv(t)
	ready := make(chan *server.Server, 1)
	onServerReady = func(s *server.Server) { ready <- s }
	defer func() { onServerReady = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--address", "127.0.0.1:0", "--backend", "memory", "--no-discovery"}, &bytes.Buffer{}, &stderr)
	}()
	var srv *server.Server
	select {
	case srv = <-ready:
	case code := <-done:
		t.Fatalf("serve exited with %d: %s", code, stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("serve not ready: %s", stderr.String())
	}
	_, sk := srv.RootCredentials()
	out := stderr.String()
	if strings.Contains(out, sk) {
		t.Fatalf("generated secret key in the output:\n%s", out)
	}
	m := regexp.MustCompile(`temporary, in (\S+) until`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("banner does not name the credentials file:\n%s", out)
	}
	data, err := os.ReadFile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]string
	if err := json.Unmarshal(data, &keys); err != nil || keys["secretKey"] != sk {
		t.Fatalf("credentials file %s: %s %v", m[1], data, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop")
	}
	if _, err := os.Stat(m[1]); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("credentials file left behind: %v", err)
	}
}
