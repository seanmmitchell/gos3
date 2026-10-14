package fs

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake restic for unit tests: the test binary itself, copied to a
// directory on PATH as "restic" and run with GOS3_FAKE_RESTIC=1. It keeps
// a "repository" as a directory (config file + snapshots/<id> holding the
// archived bytes) and logs each invocation's subcommand to FAKERESTIC_LOG.
// FAKERESTIC_FAIL names a subcommand that fails with diagnostics quoting
// the password; FAKERESTIC_VERSION overrides `restic version`;
// FAKERESTIC_DUMP_DELAY delays dumps. The real restic is exercised by the
// integration tests (skipped without restic on PATH).

func TestMain(m *testing.M) {
	if os.Getenv("GOS3_FAKE_RESTIC") == "1" {
		os.Exit(fakeRestic(os.Args[1:]))
	}
	code := m.Run()
	if fakeResticDir != "" {
		_ = os.RemoveAll(fakeResticDir)
	}
	os.Exit(code)
}

var (
	fakeResticOnce sync.Once
	fakeResticDir  string // parent of the fake executables' directories
	fakeResticErr  error
)

// installFakeRestic puts a fake restic (in a directory of its own named
// variant, so that version checks are not shared between variants) first
// on PATH for the test and returns the path of its invocation log.
func installFakeRestic(t *testing.T, variant string) string {
	t.Helper()
	fakeResticOnce.Do(func() { fakeResticDir, fakeResticErr = os.MkdirTemp("", "gos3-fake-restic-") })
	if fakeResticErr != nil {
		t.Fatal(fakeResticErr)
	}
	dir := filepath.Join(fakeResticDir, variant)
	name := "restic"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "restic.log")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOS3_FAKE_RESTIC", "1")
	t.Setenv("FAKERESTIC_LOG", log)
	t.Setenv("FAKERESTIC_FAIL", "")
	t.Setenv("FAKERESTIC_VERSION", "")
	t.Setenv("FAKERESTIC_DUMP_DELAY", "")
	return log
}

// resticCalls returns how often the fake ran the subcommand cmd (e.g.
// "backup", "cat config").
func resticCalls(t *testing.T, log, cmd string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if line == cmd {
			n++
		}
	}
	return n
}

// fakeRestic is the fake's main function.
func fakeRestic(args []string) int {
	var repo, sub string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--repo", "--retry-lock", "--http-user-agent", "--cache-dir":
			if a == "--repo" {
				repo = args[i+1]
			}
			i++
		case "--no-cache", "--no-lock", "--json":
		default:
			if sub == "" {
				sub = a
			} else {
				rest = append(rest, a)
			}
		}
	}
	if sub == "cat" && len(rest) > 0 {
		sub, rest = "cat "+rest[0], rest[1:]
	}
	if log := os.Getenv("FAKERESTIC_LOG"); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, sub)
			_ = f.Close()
		}
	}
	if sub == "version" {
		v := os.Getenv("FAKERESTIC_VERSION")
		if v == "" {
			v = "0.17.3"
		}
		fmt.Printf("restic %s compiled with go1.23 on %s/%s\n", v, runtime.GOOS, runtime.GOARCH)
		return 0
	}
	if sub == "noise" {
		// Plenty of diagnostics, to check that they are bounded.
		line := bytes.Repeat([]byte("x"), 1023)
		line = append(line, '\n')
		for range 8 << 10 {
			_, _ = os.Stderr.Write(line)
		}
		return 1
	}
	if os.Getenv("FAKERESTIC_FAIL") == sub {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Fprintf(os.Stderr, "progress...\nFatal: unable to save snapshot: password %s rejected\n", os.Getenv("RESTIC_PASSWORD"))
		return 1
	}
	if os.Getenv("RESTIC_PASSWORD") == "" {
		fmt.Fprintln(os.Stderr, "Fatal: no password")
		return 1
	}
	config := filepath.Join(repo, "config")
	if sub != "init" {
		if _, err := os.Stat(config); err != nil {
			fmt.Fprintln(os.Stderr, "Fatal: repository does not exist: unable to open config file")
			return 10
		}
	}
	snapshots := filepath.Join(repo, "snapshots")
	switch sub {
	case "cat config":
		raw, _ := os.ReadFile(config)
		_, _ = os.Stdout.Write(raw)
	case "init":
		if _, err := os.Stat(config); err == nil {
			fmt.Fprintln(os.Stderr, "Fatal: config file already exists")
			return 1
		}
		if err := os.MkdirAll(snapshots, 0o700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(config, []byte(`{"version":2}`), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "backup":
		// The snapshot exists only once the input ended, as with restic.
		tmp, err := os.CreateTemp(repo, "incoming-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.Remove(tmp.Name())
		if _, err := io.Copy(tmp, os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_ = tmp.Close()
		var id [32]byte
		_, _ = rand.Read(id[:])
		sid := hex.EncodeToString(id[:])
		if err := os.Rename(tmp.Name(), filepath.Join(snapshots, sid)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		enc := json.NewEncoder(os.Stdout)
		_ = enc.Encode(map[string]any{"message_type": "status", "percent_done": 1})
		_ = enc.Encode(map[string]any{"message_type": "summary", "snapshot_id": sid})
	case "dump":
		if d, err := time.ParseDuration(os.Getenv("FAKERESTIC_DUMP_DELAY")); err == nil {
			time.Sleep(d)
		}
		if len(rest) != 2 || rest[1] != "/object" {
			fmt.Fprintln(os.Stderr, "Fatal: bad dump arguments")
			return 1
		}
		f, err := os.Open(filepath.Join(snapshots, rest[0]))
		if err != nil {
			fmt.Fprintln(os.Stderr, "Fatal: no matching ID found")
			return 1
		}
		defer f.Close()
		_, _ = io.Copy(os.Stdout, f)
	case "snapshots":
		entries, _ := os.ReadDir(snapshots)
		var ids []string
		for _, e := range entries {
			ids = append(ids, e.Name())
		}
		_ = json.NewEncoder(os.Stdout).Encode(ids)
	default:
		fmt.Fprintf(os.Stderr, "Fatal: unknown command %q\n", sub)
		return 1
	}
	return 0
}

// snapshotCount returns how many snapshots the fake repository of the
// bucket bucket on b holds.
func snapshotCount(t *testing.T, b *Backend, bucket string) int {
	t.Helper()
	id, err := b.encryptionBucketID(ctx, bucket)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(b.resticRepository(id, nil), "snapshots"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(entries)
}
