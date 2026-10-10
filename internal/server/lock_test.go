package server

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
)

// TestDataDirectoryLock checks that a data directory can be used by one
// server at a time with every backend. Previously only the fs backend's
// metadata database was locked: a memory-backend server with a data path
// started next to a running server on the same directory, and the two
// silently overwrote each other's iam.json and settings.json.
func TestDataDirectoryLock(t *testing.T) {
	for _, tc := range []struct{ first, second string }{
		{config.BackendMemory, config.BackendMemory},
		{config.BackendFS, config.BackendMemory},
		{config.BackendMemory, config.BackendFS},
	} {
		t.Run(tc.first+"+"+tc.second, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig()
			cfg.Storage.Path = dir
			cfg.Storage.Backend = tc.first
			a, err := New(cfg, nil)
			if err != nil {
				t.Fatalf("first server: %v", err)
			}
			cfg.Storage.Backend = tc.second
			if b, err := New(cfg, nil); err == nil {
				b.Close()
				a.Close()
				t.Fatal("a second server started on a data directory in use")
			} else if !strings.Contains(err.Error(), "another gos3") {
				t.Errorf("error = %v", err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			b, err := New(cfg, nil)
			if err != nil {
				t.Fatalf("server after the first one closed: %v", err)
			}
			b.Close()
		})
	}
}

// TestPersistedSettingsOverrideWarning checks that a server whose
// settings.json overrides explicitly configured runtime values (e.g.
// --no-discovery after the settings were once saved in the UI) logs a
// warning naming the key; previously the flag was ignored silently.
func TestPersistedSettingsOverrideWarning(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	cfg.Storage.Path = dir
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settings().Update(func(st *config.Settings) error {
		st.Name = "renamed"
		st.Discovery.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	var logs strings.Builder
	cfg.Discovery.Enabled = false
	cfg.CORS.AllowedOrigins = []string{"https://ui.example.com"}
	s, err = New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Settings().Get().Discovery.Enabled {
		t.Fatal("settings.json no longer wins")
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "key=discovery.enabled") || !strings.Contains(out, "configured=false") {
		t.Errorf("no warning for discovery.enabled:\n%s", out)
	}
	if !strings.Contains(out, "key=cors.allowed_origins") {
		t.Errorf("no warning for cors.allowed_origins:\n%s", out)
	}
	if strings.Contains(out, "key=logging.level") {
		t.Errorf("a key that is not overridden was reported:\n%s", out)
	}
}
