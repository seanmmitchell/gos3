package main

import (
	"strings"
	"testing"
)

// TestConfigTrustedProxies checks that server.trusted_proxies is listed by
// "config env", printed by "config print" and validated (an invalid CIDR is
// a configuration error).
func TestConfigTrustedProxies(t *testing.T) {
	clearEnv(t)
	code, out, stderr := runCmd(t, "config", "env")
	if code != 0 || !strings.Contains(out, "GOS3_SERVER_TRUSTED_PROXIES") || !strings.Contains(out, "server.trusted_proxies") {
		t.Fatalf("config env: exit %d\n%s\n%s", code, out, stderr)
	}

	t.Setenv("GOS3_TRUST_PROXY_HEADERS", "true")
	t.Setenv("GOS3_SERVER_TRUSTED_PROXIES", "10.0.0.0/8, 2001:db8::/32")
	code, out, stderr = runCmd(t, "config", "print", "--backend", "memory")
	if code != 0 {
		t.Fatalf("config print: exit %d: %s", code, stderr)
	}
	for _, want := range []string{"trust_proxy_headers: true", "trusted_proxies:", "- 10.0.0.0/8", "- 2001:db8::/32"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	t.Setenv("GOS3_SERVER_TRUSTED_PROXIES", "10.0.0.0/33")
	code, _, stderr = runCmd(t, "config", "print", "--backend", "memory")
	if code != 1 || !strings.Contains(stderr, "server.trusted_proxies[0]") {
		t.Fatalf("invalid CIDR: exit %d\n%s", code, stderr)
	}
	code, _, stderr = runCmd(t, "serve", "--backend", "memory", "--address", "127.0.0.1:0")
	if code == 0 || !strings.Contains(stderr, "server.trusted_proxies[0]") {
		t.Fatalf("serve with an invalid CIDR: exit %d\n%s", code, stderr)
	}
}
