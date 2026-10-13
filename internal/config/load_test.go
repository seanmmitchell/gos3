package config

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/discovery"
)

// env returns a lookup function over a fixed map (the real environment is
// never consulted by these tests).
func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefault(t *testing.T) {
	c := Default()
	checks := []struct {
		name string
		ok   bool
	}{
		{"address", c.Server.Address == ":9000"},
		{"region", c.Server.Region == "us-east-1"},
		{"max object size", c.Server.MaxObjectSize == 5<<30},
		{"public info", c.Server.PublicInfo},
		{"shutdown", c.Server.ShutdownTimeout == 30*time.Second},
		{"trust proxy off", !c.Server.TrustProxyHeaders},
		{"backend", c.Storage.Backend == BackendFS},
		{"path", c.Storage.Path == "./data"},
		{"fsync", c.Storage.FsyncData},
		{"ui", c.UI.Enabled && c.UI.RedirectRoot},
		{"discovery", c.Discovery.Enabled && c.Discovery.BeaconGroup == discovery.DefaultBeaconGroup},
		{"logging", c.Logging == LoggingConfig{Level: "info", Format: "text", AccessLog: true}},
		{"lifecycle", c.Lifecycle == LifecycleConfig{Enabled: true, Interval: time.Hour}},
		{"metrics", c.Metrics == MetricsConfig{Enabled: true}},
		{"cors origins", slices.Equal(c.CORS.AllowedOrigins, []string{"*"})},
		{"cors max age", c.CORS.MaxAgeSeconds == 3600},
		{"no root creds", c.Auth.RootAccessKey == "" && c.Auth.RootSecretKey == ""},
	}
	for _, ck := range checks {
		if !ck.ok {
			t.Errorf("default %s wrong: %+v", ck.name, c)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v", err)
	}
	// Default returns independent copies.
	c.CORS.ExposeHeaders[0] = "mutated"
	if Default().CORS.ExposeHeaders[0] == "mutated" || DefaultExposeHeaders[0] == "mutated" {
		t.Error("Default shares the expose headers slice")
	}
}

func TestDefaultExposeHeaders(t *testing.T) {
	// The web UI reads these cross-origin: "*" covers x-amz-meta-* for
	// requests without credentials, the explicit names cover credentialed ones.
	for _, h := range []string{"*", "ETag", "x-amz-version-id", "x-amz-delete-marker", "x-amz-request-id",
		"x-amz-checksum-sha256", "Content-Range", "Last-Modified"} {
		if !slices.Contains(DefaultExposeHeaders, h) {
			t.Errorf("DefaultExposeHeaders lacks %q", h)
		}
	}
	s := SettingsFrom(Default())
	if err := ValidateSettings(s); err != nil {
		t.Fatalf("default settings invalid: %v", err)
	}
}

func TestLoadDefaultsOnly(t *testing.T) {
	c, err := load("", env(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Storage.Path != DefaultDataPath || c.Server.Address != DefaultAddress {
		t.Errorf("got %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPrecedence(t *testing.T) {
	file := writeFile(t, "gos3.yaml", `
server:
  address: ":8000"
  name: from-file
  region: eu-west-1
  domains: [s3.file.example]
storage:
  path: /srv/file
logging:
  level: debug
discovery:
  beacon_interval: 30s
  seeds: ["http://seed.file:9000"]
lifecycle:
  interval: 2h
`)
	vars := map[string]string{
		"GOS3_ADDRESS":             ":7000",
		"GOS3_REGION":              "ap-south-1",
		"GOS3_DOMAINS":             " a.example , b.example,, ",
		"GOS3_FSYNC":               "false",
		"GOS3_CORS_ORIGINS":        "https://ui.example.com",
		"GOS3_LOG_FORMAT":          "JSON",
		"GOS3_PUBLIC_INFO":         "off",
		"GOS3_MAX_OBJECT_SIZE":     "1GiB",
		"GOS3_TRUST_PROXY_HEADERS": "yes",
		"GOS3_TRUSTED_PROXIES":     "10.0.0.5",
	}
	c, err := load(file, env(vars), []Override{{"server.address", ":6000"}, {"GOS3_NAME", "from-flag"}})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"address (flag > env > file)", c.Server.Address, ":6000"},
		{"name (flag > file)", c.Server.Name, "from-flag"},
		{"region (env > file)", c.Server.Region, "ap-south-1"},
		{"domains (env list)", strings.Join(c.Server.Domains, ","), "a.example,b.example"},
		{"path (file)", c.Storage.Path, "/srv/file"},
		{"fsync (env)", c.Storage.FsyncData, false},
		{"level (file)", c.Logging.Level, "debug"},
		{"format (env, normalized)", c.Logging.Format, "json"},
		{"access log (default)", c.Logging.AccessLog, true},
		{"beacon interval (file)", c.Discovery.BeaconInterval, 30 * time.Second},
		{"beacon group (default kept)", c.Discovery.BeaconGroup, discovery.DefaultBeaconGroup},
		{"seeds (file)", strings.Join(c.Discovery.Seeds, ","), "http://seed.file:9000"},
		{"lifecycle (file)", c.Lifecycle.Interval, 2 * time.Hour},
		{"cors (env)", strings.Join(c.CORS.AllowedOrigins, ","), "https://ui.example.com"},
		{"public info (env)", c.Server.PublicInfo, false},
		{"max object size (env)", c.Server.MaxObjectSize, int64(1 << 30)},
		{"trust proxy (env)", c.Server.TrustProxyHeaders, true},
		{"ui (default)", c.UI.Enabled, true},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s = %v, want %v", ck.name, ck.got, ck.want)
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	file := writeFile(t, "c.yaml", "server:\n  name: via-env\n")
	c, err := load("", env(map[string]string{EnvConfig: file}), nil)
	if err != nil || c.Server.Name != "via-env" {
		t.Fatalf("GOS3_CONFIG: %+v %v", c.Server, err)
	}
	// An explicit path wins over GOS3_CONFIG.
	other := writeFile(t, "d.yaml", "server:\n  name: explicit\n")
	c, err = load(other, env(map[string]string{EnvConfig: file}), nil)
	if err != nil || c.Server.Name != "explicit" {
		t.Fatalf("explicit: %+v %v", c.Server, err)
	}
}

func TestLoadEnvClearsList(t *testing.T) {
	c, err := load("", env(map[string]string{"GOS3_CORS_ORIGINS": ""}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.CORS.AllowedOrigins == nil || len(c.CORS.AllowedOrigins) != 0 {
		t.Errorf("origins = %#v, want empty", c.CORS.AllowedOrigins)
	}
}

func TestLoadStoragePathDefaults(t *testing.T) {
	tests := []struct {
		name  string
		file  string
		vars  map[string]string
		flags []Override
		want  string
	}{
		{"fs default", "", nil, nil, DefaultDataPath},
		{"memory is ephemeral", "", map[string]string{"GOS3_STORAGE_BACKEND": "memory"}, nil, ""},
		{"memory via flag", "", nil, []Override{{"storage.backend", "memory"}}, ""},
		{"memory with explicit path", "", map[string]string{"GOS3_STORAGE_BACKEND": "Memory", "GOS3_DATA": "/tmp/x"}, nil, "/tmp/x"},
		{"memory from file with path", "storage:\n  backend: memory\n  path: /p\n", nil, nil, "/p"},
		{"fs explicit empty path", "storage:\n  path: \"\"\n", nil, nil, DefaultDataPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.file != "" {
				path = writeFile(t, "c.yaml", tt.file)
			}
			c, err := load(path, env(tt.vars), tt.flags)
			if err != nil {
				t.Fatal(err)
			}
			if c.Storage.Path != tt.want {
				t.Errorf("path = %q, want %q", c.Storage.Path, tt.want)
			}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		vars  map[string]string
		flags []Override
		want  []string
	}{
		{"unknown field", "server:\n  address: \":1\"\n  adress: x\n", nil, nil, []string{"line 3", `unknown key "adress" in section "server"`}},
		{"unknown section", "storage:\n  path: /x\nbogus: 1\n", nil, nil, []string{"line 3", `unknown top-level key "bogus"`}},
		{"type error", "server:\n  max_object_size: big\n", nil, nil, []string{"line 2"}},
		{"bad duration", "lifecycle:\n  interval: often\n", nil, nil, []string{"line 2"}},
		{"syntax", "server: [\n", nil, nil, []string{"c.yaml"}},
		{"two documents", "server:\n  name: a\n---\nserver:\n  name: b\n", nil, nil, []string{"one YAML document"}},
		{"bad env bool", "", map[string]string{"GOS3_FSYNC": "maybe"}, nil, []string{"GOS3_FSYNC", "maybe"}},
		{"bad env size", "", map[string]string{"GOS3_MAX_OBJECT_SIZE": "5 parsecs"}, nil, []string{"GOS3_MAX_OBJECT_SIZE"}},
		{"bad env duration", "", map[string]string{"GOS3_LIFECYCLE_INTERVAL": "3"}, nil, []string{"GOS3_LIFECYCLE_INTERVAL", "duration"}},
		{"bad flag", "", nil, []Override{{"server.public_info", "nah"}}, []string{"server.public_info"}},
		{"unknown flag key", "", nil, []Override{{"server.nope", "1"}}, []string{"server.nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.yaml != "" {
				path = writeFile(t, "c.yaml", tt.yaml)
			}
			_, err := load(path, env(tt.vars), tt.flags)
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		_, err := load(filepath.Join(t.TempDir(), "nope.yaml"), env(nil), nil)
		if err == nil || !os.IsNotExist(errorsUnwrapAll(err)) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("empty file", func(t *testing.T) {
		c, err := load(writeFile(t, "c.yaml", "# nothing\n"), env(nil), nil)
		if err != nil || c.Server.Address != DefaultAddress {
			t.Errorf("empty file: %v", err)
		}
	})
}

func errorsUnwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"address", func(c *Config) { c.Server.Address = "9000" }, "server.address"},
		{"address port", func(c *Config) { c.Server.Address = ":http" }, "server.address"},
		{"address port range", func(c *Config) { c.Server.Address = ":70000" }, "server.address"},
		{"tls pair", func(c *Config) { c.Server.TLSCert = "c.pem" }, "server.tls_cert and server.tls_key"},
		{"max object size", func(c *Config) { c.Server.MaxObjectSize = -1 }, "server.max_object_size"},
		{"max object size huge", func(c *Config) { c.Server.MaxObjectSize = 6 << 40 }, "server.max_object_size"},
		{"shutdown", func(c *Config) { c.Server.ShutdownTimeout = -time.Second }, "server.shutdown_timeout"},
		{"backend", func(c *Config) { c.Storage.Backend = "s3" }, "storage.backend"},
		{"fs path", func(c *Config) { c.Storage.Path = "" }, "storage.path"},
		{"root pair", func(c *Config) { c.Auth.RootAccessKey = "admin" }, "must be set together"},
		{"root access key", func(c *Config) { c.Auth.RootAccessKey, c.Auth.RootSecretKey = "a b", "secret123" }, "auth.root_access_key"},
		{"root secret short", func(c *Config) { c.Auth.RootAccessKey, c.Auth.RootSecretKey = "admin", "short" }, "auth.root_secret_key"},
		{"lifecycle interval", func(c *Config) { c.Lifecycle.Interval = 30 * time.Second }, "lifecycle.interval"},
		{"lifecycle fraction", func(c *Config) { c.Lifecycle.Interval = 90*time.Second + time.Millisecond }, "lifecycle.interval"},
		// Settings-level errors are reported with YAML keys.
		{"region", func(c *Config) { c.Server.Region = "EU" }, "server.region"},
		{"name", func(c *Config) { c.Server.Name = strings.Repeat("n", 129) }, "server.name"},
		{"public url", func(c *Config) { c.Server.PublicURL = "ftp://x" }, "server.public_url"},
		{"domain", func(c *Config) { c.Server.Domains = []string{"ok.example", "bad/x"} }, "server.domains[1]"},
		{"cors origin", func(c *Config) { c.CORS.AllowedOrigins = []string{"*", "a.com"} }, "cors.allowed_origins[1]"},
		{"cors header", func(c *Config) { c.CORS.AllowedHeaders = []string{"bad header"} }, "cors.allowed_headers[0]"},
		{"cors expose", func(c *Config) { c.CORS.ExposeHeaders = []string{"x-amz-*"} }, "cors.expose_headers[0]"},
		{"cors max age", func(c *Config) { c.CORS.MaxAgeSeconds = -1 }, "cors.max_age_seconds"},
		{"beacon group", func(c *Config) { c.Discovery.BeaconGroup = "10.0.0.1:1" }, "discovery.beacon_group"},
		{"beacon interval", func(c *Config) { c.Discovery.BeaconInterval = 2 * time.Hour }, "discovery.beacon_interval"},
		{"peer ttl", func(c *Config) { c.Discovery.PeerTTL = 8 * 24 * time.Hour }, "discovery.peer_ttl"},
		{"seeds", func(c *Config) { c.Discovery.Seeds = []string{"http://a:9000/x"} }, "discovery.seeds[0]"},
		{"dns srv", func(c *Config) { c.Discovery.DNSSRV = []string{"-x"} }, "discovery.dns_srv[0]"},
		{"max scan hosts", func(c *Config) { c.Discovery.MaxScanHosts = 70000 }, "discovery.max_scan_hosts"},
		{"log level", func(c *Config) { c.Logging.Level = "loud" }, "logging.level"},
		{"log format", func(c *Config) { c.Logging.Format = "xml" }, "logging.format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Default()
			tt.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.HasPrefix(err.Error(), tt.want) && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
	t.Run("zero discovery values mean defaults", func(t *testing.T) {
		c := Default()
		c.Discovery = discovery.Config{Enabled: true}
		c.Lifecycle.Interval = 0
		c.Server.MaxObjectSize = 0
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid variants", func(t *testing.T) {
		c := Default()
		c.Server.Address = "127.0.0.1:0"
		c.Server.TLSCert, c.Server.TLSKey = "c.pem", "k.pem"
		c.Storage = StorageConfig{Backend: BackendMemory}
		c.Auth.RootAccessKey, c.Auth.RootSecretKey = "admin", "password1"
		c.Discovery.Seeds = []string{"nas.local:9000", "https://s3.example.com"}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestYAMLRoundTrip(t *testing.T) {
	c := Default()
	c.Server.Domains = []string{"s3.example.com"}
	c.Server.TrustProxyHeaders = true
	data, err := c.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("trust_proxy_headers: true")) {
		t.Errorf("yaml lacks trust_proxy_headers:\n%s", data)
	}
	back, err := load(writeFile(t, "c.yaml", string(data)), env(nil), nil)
	if err != nil {
		t.Fatalf("reloading printed config: %v\n%s", err, data)
	}
	if back.Server.Domains[0] != "s3.example.com" || !back.Server.TrustProxyHeaders ||
		back.Discovery.BeaconInterval != c.Discovery.BeaconInterval || back.Lifecycle.Interval != time.Hour {
		t.Errorf("round trip mismatch: %+v", back)
	}
}

func TestRedacted(t *testing.T) {
	c := Default()
	c.Auth.RootAccessKey, c.Auth.RootSecretKey = "admin", "supersecret"
	c.Server.Domains = []string{"a.example"}
	r := c.Redacted()
	if r.Auth.RootSecretKey != RedactedValue || r.Auth.RootAccessKey != "admin" {
		t.Errorf("redacted auth = %+v", r.Auth)
	}
	if c.Auth.RootSecretKey != "supersecret" {
		t.Error("Redacted modified the original")
	}
	r.Server.Domains[0] = "changed"
	if c.Server.Domains[0] != "a.example" {
		t.Error("Redacted shares slices with the original")
	}
	data, err := r.YAML()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("supersecret")) {
		t.Errorf("secret in redacted YAML:\n%s", data)
	}
	// Nothing to redact: stays empty (shows that no secret is configured).
	if Default().Redacted().Auth.RootSecretKey != "" {
		t.Error("empty secret redacted")
	}
}

func TestSystemPaths(t *testing.T) {
	c := Default()
	c.Storage.Path = "/data"
	if c.SystemDir() != filepath.Join("/data", ".gos3") || c.SettingsPath() != filepath.Join("/data", ".gos3", "settings.json") {
		t.Errorf("paths %q %q", c.SystemDir(), c.SettingsPath())
	}
	c.Storage = StorageConfig{Backend: BackendMemory}
	if c.SystemDir() != "" || c.SettingsPath() != "" {
		t.Error("ephemeral config has a system dir")
	}
}

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"0": 0, "1024": 1024, "1K": 1 << 10, "1kb": 1 << 10, "5GiB": 5 << 30, " 2 MiB ": 2 << 20,
		"1.5G": 3 << 29, "1T": 1 << 40, "10B": 10,
	}
	for in, want := range good {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "G", "-1", "1X", "1.2.3G", "99999999999T", "1e3"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}
}

func TestParseLogLevel(t *testing.T) {
	for _, l := range []string{"debug", "info", "warn", "error"} {
		if _, err := ParseLogLevel(l); err != nil {
			t.Error(err)
		}
	}
	if _, err := ParseLogLevel("INFO"); err == nil {
		t.Error("upper case accepted (Normalize lower-cases)")
	}
}
