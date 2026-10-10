package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/server"
)

// clearEnv unsets every GOS3_* variable for the test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GOS3_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
}

func runCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestConfigFlags(t *testing.T) {
	clearEnv(t)
	fs := newFlagSet("serve", "serve", &bytes.Buffer{})
	cf := addConfigFlags(fs)
	err := parseFlags(fs, []string{
		"--address", "127.0.0.1:9100", "--data", "/srv/gos3", "--backend", "memory",
		"--name", "nas", "--region", "eu-west-1", "--public-url", "https://s3.example.com",
		"--domain", "s3.example.com", "--domain", "s3.lan,s3.local",
		"--tls-cert", "c.pem", "--tls-key", "k.pem",
		"--cors-origin", "http://localhost:3000", "--seed", "http://a:9000", "--seed", "b:9000",
		"--no-ui", "--no-discovery", "--log-level", "debug", "--log-format", "json", "--fsync=false",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range cf.overrides() {
		got[o.Key] = o.Value
	}
	want := map[string]string{
		"server.address":       "127.0.0.1:9100",
		"storage.path":         "/srv/gos3",
		"storage.backend":      "memory",
		"server.name":          "nas",
		"server.region":        "eu-west-1",
		"server.public_url":    "https://s3.example.com",
		"server.domains":       "s3.example.com,s3.lan,s3.local",
		"server.tls_cert":      "c.pem",
		"server.tls_key":       "k.pem",
		"cors.allowed_origins": "http://localhost:3000",
		"discovery.seeds":      "http://a:9000,b:9000",
		"ui.enabled":           "false",
		"discovery.enabled":    "false",
		"logging.level":        "debug",
		"logging.format":       "json",
		"storage.fsync_data":   "false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overrides:\n got %v\nwant %v", got, want)
	}
	cfg, err := config.Load("", cf.overrides()...)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UI.Enabled || cfg.Discovery.Enabled || cfg.Storage.FsyncData || len(cfg.Server.Domains) != 3 ||
		cfg.Server.Address != "127.0.0.1:9100" || cfg.Storage.Path != "/srv/gos3" {
		t.Fatalf("cfg = %+v", cfg)
	}

	// Flags not given are not overrides: env/file values survive.
	fs = newFlagSet("serve", "serve", &bytes.Buffer{})
	cf = addConfigFlags(fs)
	if err := parseFlags(fs, []string{"--no-ui=false"}); err != nil {
		t.Fatal(err)
	}
	if o := cf.overrides(); len(o) != 1 || o[0] != (config.Override{Key: "ui.enabled", Value: "true"}) {
		t.Fatalf("overrides = %v", o)
	}

	// Positional arguments and unknown flags are usage errors.
	if code, _, _ := runCmd(t, "serve", "extra"); code != 2 {
		t.Fatalf("positional arg: exit %d", code)
	}
	if code, _, _ := runCmd(t, "serve", "--bogus"); code != 2 {
		t.Fatalf("unknown flag: exit %d", code)
	}
	if code, _, _ := runCmd(t, "frobnicate"); code != 2 {
		t.Fatalf("unknown command: exit %d", code)
	}
	if code, _, stderr := runCmd(t, "serve", "--backend", "tape"); code != 1 || !strings.Contains(stderr, "storage.backend") {
		t.Fatalf("invalid config: exit %d %s", code, stderr)
	}
}

func TestConfigPrintRedactsSecrets(t *testing.T) {
	clearEnv(t)
	t.Setenv("GOS3_ROOT_ACCESS_KEY", "printaccesskey")
	t.Setenv("GOS3_ROOT_SECRET_KEY", "super-secret-value-123")
	code, out, stderr := runCmd(t, "config", "print", "--name", "flagname", "--domain", "s3.test")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(out, "super-secret-value-123") {
		t.Fatal("secret printed")
	}
	for _, want := range []string{"root_secret_key: " + config.RedactedValue, "root_access_key: printaccesskey", "name: flagname", "- s3.test"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// An invalid configuration is printed and reported.
	code, out, stderr = runCmd(t, "config", "print", "--log-level", "loud")
	if code != 1 || !strings.Contains(out, "level: loud") || !strings.Contains(stderr, "logging.level") {
		t.Fatalf("invalid: exit %d\n%s\n%s", code, out, stderr)
	}
}

func TestSampleConfigMatchesDefaults(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "gos3.yaml")
	if code, _, stderr := runCmd(t, "config", "init", "-o", file); code != 0 {
		t.Fatalf("config init: %d %s", code, stderr)
	}
	if code, _, _ := runCmd(t, "config", "init", "-o", file); code != 1 {
		t.Fatal("config init overwrote an existing file")
	}
	got, err := config.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	// Compare the YAML forms: the sample's empty lists ([]) decode as empty
	// rather than nil slices.
	gotY, _ := got.YAML()
	wantY, _ := want.YAML()
	if string(gotY) != string(wantY) {
		t.Fatalf("sample config differs from the defaults:\n got %s\nwant %s", gotY, wantY)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEnvKeygenVersion(t *testing.T) {
	code, out, _ := runCmd(t, "config", "env")
	if code != 0 || !strings.Contains(out, "GOS3_ROOT_SECRET_KEY") || !strings.Contains(out, "server.address") {
		t.Fatalf("config env: %d\n%s", code, out)
	}
	code, out, _ = runCmd(t, "keygen")
	if code != 0 || !strings.HasPrefix(out, "GOS3_ROOT_ACCESS_KEY=") || !strings.Contains(out, "\nGOS3_ROOT_SECRET_KEY=") {
		t.Fatalf("keygen: %d %q", code, out)
	}
	code, out, _ = runCmd(t, "keys", "gen", "--json")
	var keys map[string]string
	if code != 0 || json.Unmarshal([]byte(out), &keys) != nil || len(keys["accessKey"]) != 20 || len(keys["secretKey"]) != 40 {
		t.Fatalf("keys gen --json: %d %q", code, out)
	}
	code, out, _ = runCmd(t, "version")
	if code != 0 || !strings.HasPrefix(out, "gos3 ") {
		t.Fatalf("version: %d %q", code, out)
	}
}

func TestServeAndHealthcheck(t *testing.T) {
	clearEnv(t)
	t.Setenv("GOS3_ROOT_ACCESS_KEY", "serveaccesskey")
	t.Setenv("GOS3_ROOT_SECRET_KEY", "serve-secret-key-123")
	dir := t.TempDir()

	ready := make(chan *server.Server, 1)
	onServerReady = func(s *server.Server) { ready <- s }
	defer func() { onServerReady = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--address", "127.0.0.1:0", "--data", dir, "--no-discovery", "--name", "cli-test"},
			&bytes.Buffer{}, &stderr)
	}()
	var srv *server.Server
	select {
	case srv = <-ready:
	case code := <-done:
		t.Fatalf("serve exited with %d: %s", code, stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("serve not ready: %s", stderr.String())
	}
	if srv.Info().Name != "cli-test" || srv.BackendName() != "fs" {
		t.Fatalf("info = %+v", srv.Info())
	}
	banner := stderr.String()
	for _, want := range []string{"S3 endpoint", srv.URL(), "Access key   serveaccesskey", "from the configuration", "fs at "} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner lacks %q:\n%s", want, banner)
		}
	}
	if strings.Contains(banner, "serve-secret-key-123") {
		t.Error("configured secret printed")
	}

	if code, out, errOut := runCmd(t, "healthcheck", "--url", srv.URL()); code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("healthcheck: %d %q %q", code, out, errOut)
	}
	if code, _, _ := runCmd(t, "healthcheck", "--ready", "--url", srv.URL()); code != 0 {
		t.Fatalf("healthcheck --ready: %d", code)
	}
	code, out, errOut := runCmd(t, "discover", "--no-mdns", "--no-beacon", "--seed", srv.URL(), "--wait", "500ms", "--json")
	var peers []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &peers) != nil || len(peers) != 1 || peers[0].ID != srv.ServerID() {
		t.Fatalf("discover --json: %d %q %q", code, out, errOut)
	}
	code, out, _ = runCmd(t, "discover", "--no-mdns", "--no-beacon", "--seed", srv.URL(), "--wait", "500ms")
	if code != 0 || !strings.Contains(out, "cli-test") || !strings.Contains(out, srv.ServerID()) {
		t.Fatalf("discover table: %d %q", code, out)
	}
	t.Setenv("GOS3_ADDRESS", srv.Addr())
	if code, _, errOut := runCmd(t, "healthcheck"); code != 0 {
		t.Fatalf("healthcheck from env: %d %s", code, errOut)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit %d: %s", code, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not stop")
	}
	if !strings.Contains(stderr.String(), "shutdown complete") {
		t.Errorf("no shutdown log:\n%s", stderr.String())
	}
	if code, _, _ := runCmd(t, "healthcheck", "--url", srv.URL(), "--timeout", "2s"); code != 1 {
		t.Fatalf("healthcheck against a stopped server: exit %d", code)
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestConfigPrintAppliesSettingsFile checks that `config print` shows the
// runtime settings saved in <data>/.gos3/settings.json, which the server
// lays over flags and variables, and names the configured values they
// override. Previously it printed the flag values as the effective
// configuration.
func TestConfigPrintAppliesSettingsFile(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Storage.Path = dir
	store := config.NewStore(config.SettingsFrom(cfg), cfg.SettingsPath())
	if _, err := store.Update(func(s *config.Settings) error {
		s.Name = "renamed-in-ui"
		s.Discovery.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCmd(t, "config", "print", "--data", dir, "--no-discovery", "--name", "flagname")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"name: renamed-in-ui",
		"settings.json",
		"#   server.name: flagname -> renamed-in-ui",
		"#   discovery.enabled: false -> true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "name: flagname\n") {
		t.Errorf("flag value printed as effective:\n%s", out)
	}
}
