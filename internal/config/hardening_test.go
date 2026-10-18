package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestDemoCredentialsRefused: the root secret keys published in the
// README, the docs and docker-compose.yml (and other servers' defaults)
// are refused unless auth.insecure_demo_credentials is set: any website a
// user opens can sign requests with them, and the default CORS "*" rule
// approves signed cross-origin requests.
func TestDemoCredentialsRefused(t *testing.T) {
	for _, sk := range []string{"change-me-now", "gos3admin-secret", "MinioAdmin", "CHANGE-ME-secret-key-at-least-8-chars", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"} {
		c := Default()
		c.Auth.RootAccessKey, c.Auth.RootSecretKey = "admin", sk
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "auth.root_secret_key") ||
			!strings.Contains(err.Error(), EnvInsecureDemoCredentials) {
			t.Errorf("secret %q: %v", sk, err)
		}
		c.Auth.InsecureDemoCredentials = true
		if err := c.Validate(); err != nil {
			t.Errorf("secret %q with insecure_demo_credentials: %v", sk, err)
		}
	}
	c, err := load("", env(map[string]string{
		"GOS3_ROOT_ACCESS_KEY": "admin", "GOS3_ROOT_SECRET_KEY": "change-me-now", EnvInsecureDemoCredentials: "true",
	}), nil)
	if err != nil || !c.Auth.InsecureDemoCredentials || c.Validate() != nil {
		t.Errorf("%s from the environment: %v %+v", EnvInsecureDemoCredentials, err, c.Auth)
	}
	c = Default()
	c.Auth.RootAccessKey, c.Auth.RootSecretKey = "admin", "a-private-secret-0123"
	if err := c.Validate(); err != nil {
		t.Errorf("own secret: %v", err)
	}
}

// TestShortOrDemoSecretWarned: a configured root secret shorter than
// MinRootSecretLen, or an allowed demo secret, is warned about at startup
// (never logging the secret itself).
func TestShortOrDemoSecretWarned(t *testing.T) {
	for _, tc := range []struct {
		secret string
		warn   string
	}{
		{"short-sec", "shorter than 16"},
		{"change-me-now", "published example"},
		{"a-long-enough-secret", ""},
	} {
		c := Default()
		c.Auth.RootAccessKey, c.Auth.RootSecretKey = "admin", tc.secret
		log, buf := logBuffer()
		if err := ResolveRootCredentials(&c, "", log); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		if tc.warn == "" && strings.Contains(out, `"level":"WARN"`) || tc.warn != "" && !strings.Contains(out, tc.warn) {
			t.Errorf("secret %q: log\n%s", tc.secret, out)
		}
		if strings.Contains(out, tc.secret) {
			t.Errorf("secret %q logged", tc.secret)
		}
	}
}

// TestPrivateFileACLBestEffort: a file system that cannot take an ACL
// (FAT, exFAT, SMB) must not make secret files unwritable: WriteFileAtomic
// warns and writes the file anyway.
func TestPrivateFileACLBestEffort(t *testing.T) {
	prev := restrictFile
	defer func() { restrictFile = prev }()
	restrictFile = func(string, bool) error { return errors.New("not supported") }
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := WriteFileAtomic(file, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "{}\n" {
		t.Fatalf("file: %q %v", data, err)
	}
}

// TestTrustProxyHeadersRequiresProxies: trust_proxy_headers without
// trusted_proxies would let every client choose aws:SourceIp and
// aws:SecureTransport; it is a configuration error.
func TestTrustProxyHeadersRequiresProxies(t *testing.T) {
	c := Default()
	c.Server.TrustProxyHeaders = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "server.trusted_proxies is empty") {
		t.Errorf("trust without proxies: %v", err)
	}
	c.Server.TrustedProxies = []string{"127.0.0.1"}
	if err := c.Validate(); err != nil {
		t.Errorf("trust with proxies: %v", err)
	}
}

func TestAllowedHostsAndMaxConnections(t *testing.T) {
	c, err := load("", env(map[string]string{"GOS3_ALLOWED_HOSTS": " S3.Example.com. , *.cdn.example,,[::1]", "GOS3_MAX_CONNECTIONS": "10"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"s3.example.com", "*.cdn.example", "[::1]"}; !slices.Equal(c.Server.AllowedHosts, want) {
		t.Errorf("allowed_hosts %q, want %q", c.Server.AllowedHosts, want)
	}
	if c.Server.MaxConnections != 10 || Default().Server.MaxConnections != 0 {
		t.Errorf("max_connections %d, default %d (want no limit)", c.Server.MaxConnections, Default().Server.MaxConnections)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		hosts []string
		want  string
	}{
		{[]string{"https://s3.example.com"}, "server.allowed_hosts[0]"},
		{[]string{"s3.example.com:9000"}, "server.allowed_hosts[0]"},
		{[]string{"ok.example", "*.*.example"}, "server.allowed_hosts[1]"},
		{[]string{"*", "ok.example"}, "must be the only entry"},
	} {
		c := Default()
		c.Server.AllowedHosts = tc.hosts
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.hosts, err, tc.want)
		}
	}
	c = Default()
	c.Server.AllowedHosts = []string{"*", "10.0.0.1"}[:1]
	if err := c.Validate(); err != nil {
		t.Errorf(`["*"]: %v`, err)
	}
	c.Server.MaxConnections = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "server.max_connections") {
		t.Errorf("negative max_connections: %v", err)
	}
	r := c.Redacted()
	r.Server.AllowedHosts[0] = "changed"
	if c.Server.AllowedHosts[0] != "*" {
		t.Error("Redacted shares allowed_hosts")
	}
}
