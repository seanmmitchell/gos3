package discovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseInfo(t *testing.T) {
	tests := []struct {
		name, body string
		ok         bool
	}{
		{"valid", `{"service":"gos3","id":"abc123","name":"n"}`, true},
		{"unknown fields ok", `{"service":"gos3","id":"abc123","extra":{"a":1}}`, true},
		{"wrong service", `{"service":"minio","id":"abc123"}`, false},
		{"missing id", `{"service":"gos3"}`, false},
		{"bad id", `{"service":"gos3","id":"a b"}`, false},
		{"not json", `<html>`, false},
		{"array", `[1,2]`, false},
		{"trailing", `{"service":"gos3","id":"a"} {}`, false},
		{"empty", ``, false},
		{"wrong types", `{"service":"gos3","id":5}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ParseInfo([]byte(tt.body))
			if tt.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrNotGos3) {
				t.Errorf("error %v does not wrap ErrNotGos3", err)
			}
			if tt.ok && info.ID != "abc123" {
				t.Errorf("id = %q", info.ID)
			}
		})
	}
}

func TestProberFetch(t *testing.T) {
	big := `{"service":"gos3","id":"big","name":"` + strings.Repeat("x", MaxInfoSize) + `"}`
	mux := http.NewServeMux()
	handle := func(prefix string, h http.HandlerFunc) {
		mux.HandleFunc(prefix+InfoPath, h)
	}
	handle("/valid", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"service":"gos3","id":"valid1","name":"ok"}`))
	})
	handle("/invalid", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"service":"other","id":"x"}`))
	})
	handle("/oversized", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(big))
	})
	handle("/chunked-oversized", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < len(big); i += 4096 {
			_, _ = w.Write([]byte(big[i:min(i+4096, len(big))]))
			w.(http.Flusher).Flush()
		}
	})
	handle("/status", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	handle("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/valid"+InfoPath, http.StatusFound)
	})
	slowDone := make(chan struct{})
	handle("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-slowDone:
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(slowDone)

	p := newProber()
	defer p.close()
	ctx := context.Background()

	info, err := p.fetch(ctx, srv.URL+"/valid")
	if err != nil || info.ID != "valid1" || info.Name != "ok" {
		t.Fatalf("valid: %+v %v", info, err)
	}
	for _, path := range []string{"/invalid", "/oversized", "/chunked-oversized", "/status", "/redirect"} {
		if _, err := p.fetch(ctx, srv.URL+path); !errors.Is(err, ErrNotGos3) {
			t.Errorf("%s: err = %v, want ErrNotGos3", path, err)
		}
	}
	tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.fetch(tctx, srv.URL+"/slow"); err == nil {
		t.Error("slow: no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("slow probe took %v", d)
	}
}

func TestServiceProbe(t *testing.T) {
	defer checkLeaks(t)()
	srv := newGos3Server(t, "peer000000000001", "alpha")
	self := newGos3Server(t, "self000000000001", "self")
	s := New(quietConfig(), func() Info { return self.Info() }, testLogger(t))
	clk := newFakeClock()
	s.now = clk.Now

	// Normalisation: host:port without scheme, trailing path.
	p, err := s.Probe(context.Background(), srv.hostPort()+"/ignored/path/")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "peer000000000001" || p.Name != "alpha" || !p.Verified || !p.Healthy {
		t.Fatalf("peer = %+v", p)
	}
	if !slices.Equal(p.Sources, []Source{SourceProbe}) || !slices.Equal(p.Addresses, []string{srv.URL}) {
		t.Errorf("sources/addresses = %v %v", p.Sources, p.Addresses)
	}
	if got := s.Peers(); len(got) != 1 || got[0].ID != p.ID {
		t.Fatalf("Peers() = %+v", got)
	}

	// Probing ourselves returns the view but does not add it.
	sp, err := s.Probe(context.Background(), self.URL)
	if err != nil || sp.ID != "self000000000001" {
		t.Fatalf("self probe: %+v %v", sp, err)
	}
	if got := s.Peers(); len(got) != 1 {
		t.Fatalf("self added: %+v", got)
	}

	// Failure marks the known peer unhealthy.
	srv.Close()
	if _, err := s.Probe(context.Background(), srv.URL); err == nil {
		t.Fatal("probe of closed server succeeded")
	}
	got := s.Peers()
	if len(got) != 1 || got[0].Healthy || got[0].Error == "" {
		t.Fatalf("after failure: %+v", got)
	}

	// Invalid URL.
	if _, err := s.Probe(context.Background(), "ftp://x"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("invalid url err = %v", err)
	}

	// TTL expiry through Peers() with the fake clock.
	clk.Advance(2 * time.Minute)
	if got := s.Peers(); len(got) != 0 {
		t.Fatalf("not expired: %+v", got)
	}
	s.Stop()
	self.Close()
}
