package server

import (
	"context"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
)

// TestTrustedProxiesWiring checks that server.trust_proxy_headers and
// server.trusted_proxies reach every API that evaluates aws:SourceIp: the
// S3 API, the admin API and the metrics endpoint. A client-controlled
// leftmost X-Forwarded-For entry (as forwarded by an appending proxy) must
// not satisfy an IP condition anywhere; previously the first entry was the
// client address on all three.
func TestTrustedProxiesWiring(t *testing.T) {
	setup := func(t *testing.T, proxies ...string) (*running, auth.Credentials) {
		cfg := testConfig()
		cfg.Server.TrustProxyHeaders = true
		cfg.Server.TrustedProxies = proxies
		srv := start(t, cfg)
		ctx := context.Background()
		doc, err := iam.ParsePolicy([]byte(`{"Version":"2012-10-17","Statement":[
			{"Effect":"Allow","Action":["s3:ListAllMyBuckets","admin:ServerInfo","admin:GetStats"],"Resource":"*"},
			{"Effect":"Deny","Action":["s3:*","admin:*"],"Resource":"*",
			 "Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.IAM().PutPolicy(ctx, "office-only", "", *doc); err != nil {
			t.Fatal(err)
		}
		if _, err := srv.IAM().CreateUser(ctx, iam.UserSpec{Name: "ops", Policies: []string{"office-only"}}); err != nil {
			t.Fatal(err)
		}
		k, err := srv.IAM().CreateAccessKey(ctx, "ops", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		return srv, auth.Credentials{AccessKey: k.AccessKey, SecretKey: k.SecretKey}
	}
	paths := []string{"/", "/_gos3/admin/v1/info", "/_gos3/metrics"}
	check := func(t *testing.T, srv *running, creds auth.Credentials, xff string, want int) {
		t.Helper()
		for _, p := range paths {
			if r := do(t, "GET", srv.url+p, nil, &creds, map[string]string{"X-Forwarded-For": xff}); r.status != want {
				t.Errorf("GET %s with X-Forwarded-For %q: %d, want %d (%s)", p, xff, r.status, want, r.body)
			}
		}
	}

	t.Run("trusted proxy", func(t *testing.T) {
		// The test client (127.0.0.1) plays the proxy in front of gos3.
		srv, ops := setup(t, "127.0.0.1")
		check(t, srv, ops, "10.1.2.3, 198.51.100.7", 403) // spoofed leftmost entry
		check(t, srv, ops, "198.51.100.7, 10.1.2.3", 200) // proxy appended the office address
	})
	t.Run("untrusted peer", func(t *testing.T) {
		srv, ops := setup(t, "192.0.2.0/24")
		check(t, srv, ops, "10.1.2.3", 403)
	})
}

// TestNewRejectsTrustWithoutProxies: trust_proxy_headers with an empty
// trusted_proxies would let every client choose its aws:SourceIp and
// aws:SecureTransport; the configuration is refused.
func TestNewRejectsTrustWithoutProxies(t *testing.T) {
	cfg := testConfig()
	cfg.Server.TrustProxyHeaders = true
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "server.trusted_proxies is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewRejectsInvalidTrustedProxies(t *testing.T) {
	cfg := testConfig()
	cfg.Server.TrustProxyHeaders = true
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8", "proxy.internal"}
	if _, err := New(cfg, nil); err == nil || !strings.Contains(err.Error(), "server.trusted_proxies[1]") {
		t.Fatalf("err = %v", err)
	}
}
