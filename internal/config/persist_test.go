package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func fileMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestStoreUpdatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".gos3", SettingsFileName)
	st := NewStore(validSettings(), path)
	if st.Path() != path {
		t.Fatalf("Path() = %q", st.Path())
	}
	var calls []string
	st.Subscribe(func(old, new Settings) { calls = append(calls, old.Name+">"+new.Name) })

	out, err := st.Update(func(s *Settings) error { s.Name = "renamed"; return nil })
	if err != nil || out.Name != "renamed" || st.Get().Name != "renamed" {
		t.Fatalf("update: %+v %v", out, err)
	}
	if strings.Join(calls, ",") != "gos3-test>renamed" {
		t.Errorf("subscriber calls %v", calls)
	}
	if runtime.GOOS != "windows" {
		if m := fileMode(t, path); m != 0o600 {
			t.Errorf("settings.json mode %v, want 0600", m)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Settings
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk.Name != "renamed" {
		t.Fatalf("on disk: %s (%v)", data, err)
	}
	// No temporary files are left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries", len(entries))
	}

	// A fresh store over the same file sees the persisted settings.
	st2, err := LoadStore(validSettings(), path)
	if err != nil || st2.Get().Name != "renamed" {
		t.Fatalf("LoadStore: %v %+v", err, st2)
	}
}

func TestStoreUpdateRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), SettingsFileName)
	st := NewStore(validSettings(), path)
	notified := false
	st.Subscribe(func(old, new Settings) { notified = true })

	// fn errors are returned as is.
	sentinel := errors.New("nope")
	if _, err := st.Update(func(s *Settings) error { s.Name = "x"; return sentinel }); err != sentinel {
		t.Fatalf("fn error: %v", err)
	}
	// Invalid documents are rejected with a FieldError.
	cur, err := st.Update(func(s *Settings) error { s.Region = "Bad Region"; return nil })
	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "region" {
		t.Fatalf("validation: %v", err)
	}
	if cur.Region != "us-east-1" || st.Get().Region != "us-east-1" {
		t.Error("invalid settings applied")
	}
	if notified {
		t.Error("subscriber notified of a failed update")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file written for failed updates: %v", err)
	}
}

func TestStoreUpdateRollsBackOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	// The settings directory is a regular file: MkdirAll fails (ENOTDIR),
	// even when running as root.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := NewStore(validSettings(), filepath.Join(blocker, SettingsFileName))
	notified := false
	st.Subscribe(func(old, new Settings) { notified = true })
	cur, err := st.Update(func(s *Settings) error {
		s.Name = "lost"
		s.Domains[0] = "mutated.example" // in-place edits of the copy must not leak
		return nil
	})
	var pe *fs.PathError
	if err == nil || !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *fs.PathError", err)
	}
	if cur.Name != "gos3-test" || st.Get().Name != "gos3-test" || st.Get().Domains[0] != "s3.local" {
		t.Errorf("state changed after failed write: %+v", st.Get())
	}
	if notified {
		t.Error("subscriber notified of a failed write")
	}
}

func TestStoreGetReturnsCopies(t *testing.T) {
	st := NewStore(validSettings(), "")
	s := st.Get()
	s.Domains[0] = "x"
	s.CORS.AllowedOrigins[0] = "x"
	s.Discovery.Seeds = append(s.Discovery.Seeds, "x")
	g := st.Get()
	if g.Domains[0] != "s3.local" || g.CORS.AllowedOrigins[0] != "https://ui.example.com" || len(g.Discovery.Seeds) != 0 {
		t.Errorf("Get aliases internal state: %+v", g)
	}
}

func TestStoreConcurrentUpdates(t *testing.T) {
	st := NewStore(validSettings(), filepath.Join(t.TempDir(), SettingsFileName))
	var mu sync.Mutex
	last := int64(validSettings().CORS.MaxAgeSeconds)
	ordered := true
	st.Subscribe(func(old, new Settings) {
		mu.Lock()
		defer mu.Unlock()
		if old.CORS.MaxAgeSeconds != int(last) {
			ordered = false
		}
		last = int64(new.CORS.MaxAgeSeconds)
	})
	st.Update(func(s *Settings) error { s.CORS.MaxAgeSeconds = 0; return nil })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st.Update(func(s *Settings) error { s.CORS.MaxAgeSeconds++; return nil })
			_ = st.Get()
		}()
	}
	wg.Wait()
	if got := st.Get().CORS.MaxAgeSeconds; got != 20 || !ordered {
		t.Errorf("max age %d, ordered %v", got, ordered)
	}
}

func TestLoadStore(t *testing.T) {
	t.Run("no file", func(t *testing.T) {
		st, err := LoadStore(validSettings(), filepath.Join(t.TempDir(), SettingsFileName))
		if err != nil || st.Get().Name != "gos3-test" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("memory only", func(t *testing.T) {
		st, err := LoadStore(validSettings(), "")
		if err != nil || st.Path() != "" {
			t.Fatal(err)
		}
	})
	t.Run("partial overlay", func(t *testing.T) {
		path := writeFile(t, SettingsFileName, `{"name":"persisted","discovery":{"seeds":["http://peer:9000"]},"futureField":1}`)
		st, err := LoadStore(validSettings(), path)
		if err != nil {
			t.Fatal(err)
		}
		s := st.Get()
		if s.Name != "persisted" || s.Region != "us-east-1" || len(s.Discovery.Seeds) != 1 ||
			s.Discovery.BeaconGroup == "" || s.CORS.MaxAgeSeconds != 600 || s.Discovery.DNSSRV == nil {
			t.Errorf("overlay = %+v", s)
		}
	})
	errs := []struct {
		name, content, want string
	}{
		{"corrupt", `{"name":`, "corrupt"},
		{"empty", ``, "corrupt"},
		{"trailing", `{"name":"a"} {"name":"b"}`, "corrupt"},
		{"wrong type", `{"name":5}`, "corrupt"},
		{"invalid", `{"region":"Not Valid"}`, "region"},
	}
	for _, tt := range errs {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, SettingsFileName, tt.content)
			_, err := LoadStore(validSettings(), path)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), path) {
				t.Errorf("err = %v, want mention of %q and the path", err, tt.want)
			}
		})
	}
	t.Run("invalid initial", func(t *testing.T) {
		bad := validSettings()
		bad.Logging.Level = "loud"
		if _, err := LoadStore(bad, ""); err == nil || !strings.Contains(err.Error(), "logging.level") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	for _, content := range []string{"one", "two"} {
		if err := WriteFileAtomic(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Errorf("content %q, want %q", got, content)
		}
	}
}

// logBuffer returns a JSON logger writing to a buffer.
func logBuffer() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestResolveRootCredentialsGeneratesAndReuses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".gos3")
	c := Default()
	log, buf := logBuffer()
	if err := ResolveRootCredentials(&c, dir, log); err != nil {
		t.Fatal(err)
	}
	ak, sk := c.Auth.RootAccessKey, c.Auth.RootSecretKey
	if len(ak) != 20 || len(sk) != 40 {
		t.Fatalf("generated %q / %q", ak, sk)
	}
	if err := validateRootCredentials(ak, sk); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, RootCredentialsFileName)
	if runtime.GOOS != "windows" {
		if m := fileMode(t, file); m != 0o600 {
			t.Errorf("credentials file mode %v, want 0600", m)
		}
	}
	// The secret key is never logged (logs are collected and kept
	// widely); the warning names the access key and the file.
	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, ak) || !strings.Contains(out, RootCredentialsFileName) {
		t.Errorf("generation not logged as a warning with the access key and file:\n%s", out)
	}
	if strings.Contains(out, sk) {
		t.Errorf("generated secret key logged:\n%s", out)
	}

	// Second start: same keys, secret not logged again.
	c2 := Default()
	log2, buf2 := logBuffer()
	if err := ResolveRootCredentials(&c2, dir, log2); err != nil {
		t.Fatal(err)
	}
	if c2.Auth.RootAccessKey != ak || c2.Auth.RootSecretKey != sk {
		t.Error("stored credentials not reused")
	}
	if strings.Contains(buf2.String(), sk) || !strings.Contains(buf2.String(), ak) {
		t.Errorf("reuse log:\n%s", buf2.String())
	}
	if src, _, _ := resolveRootCredentials(&Config{}, dir); src != CredentialsStored {
		t.Errorf("source %q", src)
	}
}

func TestResolveRootCredentialsConfigured(t *testing.T) {
	dir := t.TempDir()
	c := Default()
	c.Auth.RootAccessKey, c.Auth.RootSecretKey = "admin", "configured-secret"
	log, buf := logBuffer()
	if err := ResolveRootCredentials(&c, dir, log); err != nil {
		t.Fatal(err)
	}
	if c.Auth.RootSecretKey != "configured-secret" {
		t.Error("configured credentials replaced")
	}
	if strings.Contains(buf.String(), "configured-secret") {
		t.Errorf("configured secret logged:\n%s", buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, RootCredentialsFileName)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("credentials file written for configured credentials")
	}

	half := Default()
	half.Auth.RootAccessKey = "admin"
	if err := ResolveRootCredentials(&half, dir, log); err == nil || !strings.Contains(err.Error(), "together") {
		t.Errorf("half-configured: %v", err)
	}
}

func TestResolveRootCredentialsEphemeral(t *testing.T) {
	log, buf := logBuffer()
	c1, c2 := Default(), Default()
	if err := ResolveRootCredentials(&c1, "", log); err != nil {
		t.Fatal(err)
	}
	if err := ResolveRootCredentials(&c2, "", log); err != nil {
		t.Fatal(err)
	}
	if c1.Auth.RootAccessKey == "" || c1.Auth.RootAccessKey == c2.Auth.RootAccessKey {
		t.Error("ephemeral credentials not freshly generated")
	}
	if strings.Count(buf.String(), `"level":"WARN"`) != 2 || !strings.Contains(buf.String(), c2.Auth.RootAccessKey) {
		t.Errorf("ephemeral credentials not logged on every start:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), c1.Auth.RootSecretKey) || strings.Contains(buf.String(), c2.Auth.RootSecretKey) {
		t.Errorf("ephemeral secret key logged:\n%s", buf.String())
	}
	// A nil logger falls back to slog.Default.
	prev := slog.Default()
	defer slog.SetDefault(prev)
	slog.SetDefault(log)
	c3 := Default()
	if err := ResolveRootCredentials(&c3, "", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), c3.Auth.RootAccessKey) {
		t.Error("nil logger did not use slog.Default")
	}
}

func TestResolveRootCredentialsBadFile(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt": "{",
		"invalid": `{"accessKey":"a b","secretKey":"x"}`,
		"empty":   `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, RootCredentialsFileName)
			if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			c := Default()
			err := ResolveRootCredentials(&c, dir, slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), file) {
				t.Errorf("err = %v", err)
			}
			if c.Auth.RootAccessKey != "" {
				t.Error("credentials set despite error")
			}
		})
	}
}
