package admin

import (
	"net/netip"
	"testing"

	"github.com/seanmmitchell/gos3/internal/clientip"
)

func mustParsePrefixes(entries []string) []netip.Prefix {
	p, err := clientip.ParsePrefixes(entries)
	if err != nil {
		panic(err)
	}
	return p
}

// TestTrustedProxySourceIp checks that an admin-API aws:SourceIp condition
// cannot be satisfied by a client-supplied X-Forwarded-For entry forwarded
// by an appending proxy (nginx $proxy_add_x_forwarded_for, most load
// balancers): the proxy appends the real peer address, which is the client.
// Previously the first (client-controlled) entry was used, so an internet
// client sending "X-Forwarded-For: 10.1.2.3" passed an office-only Allow and
// slipped past a Deny-unless-office rule with a stolen key.
func TestTrustedProxySourceIp(t *testing.T) {
	setup := func(e *testEnv) {
		e.putPolicy("office-only", map[string]any{
			"Effect": "Allow", "Action": "admin:ServerInfo", "Resource": "*",
			"Condition": map[string]any{"IpAddress": map[string]any{"aws:SourceIp": "10.0.0.0/8"}},
		})
		e.putPolicy("stats-outside-office-denied", map[string]any{
			"Effect": "Allow", "Action": "admin:GetStats", "Resource": "*",
		}, map[string]any{
			"Effect": "Deny", "Action": "admin:*", "Resource": "*",
			"Condition": map[string]any{"NotIpAddress": map[string]any{"aws:SourceIp": "10.0.0.0/8"}},
		})
	}
	// The test client connects from loopback: that is "the proxy".
	for _, tt := range []struct {
		name    string
		proxies []string
	}{
		{"empty trusted_proxies", nil},
		{"proxy listed in trusted_proxies", []string{"127.0.0.0/8", "::1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, withTrustProxy(tt.proxies...))
			setup(e)
			ops := e.addUser("ops", "office-only", "stats-outside-office-denied")
			// Attacker at 198.51.100.7 forges the office address; the
			// appending proxy adds the real one.
			spoofed := withHeader("X-Forwarded-For", "10.1.2.3, 198.51.100.7")
			e.do(&ops, "GET", Prefix+"/info", nil, spoofed).expectError(t, 403, codeAccessDenied)
			e.do(&ops, "GET", Prefix+"/stats", nil, spoofed).expectError(t, 403, codeAccessDenied)
			// A genuine office client (the proxy appended 10.1.2.3).
			office := withHeader("X-Forwarded-For", "198.51.100.7, 10.1.2.3")
			e.do(&ops, "GET", Prefix+"/info", nil, office).expectStatus(t, 200)
			e.do(&ops, "GET", Prefix+"/stats", nil, office).expectStatus(t, 200)
		})
	}
	t.Run("multi-hop chain", func(t *testing.T) {
		// CDN (192.0.2.0/24) -> ingress (loopback) -> gos3.
		e := newEnv(t, withTrustProxy("127.0.0.1", "192.0.2.0/24"))
		setup(e)
		ops := e.addUser("ops", "office-only", "stats-outside-office-denied")
		e.do(&ops, "GET", Prefix+"/info", nil,
			withHeader("X-Forwarded-For", "10.1.2.3, 198.51.100.7, 192.0.2.5")).expectError(t, 403, codeAccessDenied)
		e.do(&ops, "GET", Prefix+"/info", nil,
			withHeader("X-Forwarded-For", "198.51.100.7, 10.1.2.3, 192.0.2.5")).expectStatus(t, 200)
	})
	t.Run("peer not a trusted proxy", func(t *testing.T) {
		// Requests that do not come from a listed proxy are taken at face
		// value: X-Forwarded-For and X-Forwarded-Proto are ignored.
		e := newEnv(t, withTrustProxy("192.0.2.0/24"))
		setup(e)
		e.putPolicy("tls-only", map[string]any{
			"Effect": "Allow", "Action": "admin:ListUsers", "Resource": "*",
			"Condition": map[string]any{"Bool": map[string]any{"aws:SecureTransport": "true"}},
		})
		ops := e.addUser("ops", "office-only", "tls-only")
		e.do(&ops, "GET", Prefix+"/info", nil, withHeader("X-Forwarded-For", "10.1.2.3")).expectError(t, 403, codeAccessDenied)
		e.do(&ops, "GET", Prefix+"/users", nil, withHeader("X-Forwarded-Proto", "https")).expectError(t, 403, codeAccessDenied)
	})
}
