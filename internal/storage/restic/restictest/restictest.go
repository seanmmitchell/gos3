// Package restictest provides a fake restic executable for unit tests of
// code that runs restic through package restic.
//
// The fake is the test binary itself: Install copies it to a directory put
// first on PATH as "restic" and sets GOS3_FAKE_RESTIC=1, and Main (called
// from the package's TestMain) runs the fake instead of the tests when that
// variable is set. It supports the subcommands gos3 uses (version, cat
// config, init, backup --stdin, dump, snapshots) on two kinds of
// repositories:
//
//   - a local directory: <repo>/config and <repo>/snapshots/<id> holding
//     the archived bytes;
//   - restic's s3 backend, "s3:[http(s)://]host[:port]/bucket[/path]":
//     the same files as objects below path in the bucket, reached through
//     internal/s3client with credentials from AWS_ACCESS_KEY_ID and
//     AWS_SECRET_ACCESS_KEY, path-style addressing from
//     `-o s3.bucket-lookup=path` and the region from `-o s3.region=...`
//     or AWS_DEFAULT_REGION. init creates a missing bucket.
//
// The fake is controlled by environment variables (Install resets them):
//
//   - EnvLog (FAKERESTIC_LOG): file receiving each invocation's subcommand
//     ("backup", "cat config", ...) on a line of its own (see Calls);
//   - EnvFail (FAKERESTIC_FAIL): a subcommand that fails, with diagnostics
//     quoting the password;
//   - EnvVersion (FAKERESTIC_VERSION): the release `restic version` reports
//     (default 0.17.3);
//   - EnvDumpDelay (FAKERESTIC_DUMP_DELAY): a duration each dump waits
//     first.
//
// The subcommand "noise" writes 8 MiB of diagnostics and fails. The real
// restic is exercised by integration tests, which skip without restic on
// PATH.
package restictest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// Environment variables controlling the fake (see the package
// documentation).
const (
	EnvFake      = "GOS3_FAKE_RESTIC"
	EnvLog       = "FAKERESTIC_LOG"
	EnvFail      = "FAKERESTIC_FAIL"
	EnvVersion   = "FAKERESTIC_VERSION"
	EnvDumpDelay = "FAKERESTIC_DUMP_DELAY"
)

// Main runs the fake restic when the test binary was started as one, and
// m's tests otherwise. Call it from TestMain:
//
//	func TestMain(m *testing.M) { restictest.Main(m) }
func Main(m *testing.M) {
	if os.Getenv(EnvFake) == "1" {
		os.Exit(run(os.Args[1:]))
	}
	code := m.Run()
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	os.Exit(code)
}

var (
	once   sync.Once
	dir    string // parent of the fake executables' directories
	dirErr error
)

// Install puts a fake restic (in a directory of its own named variant, so
// that version checks, which are cached per executable path, are not
// shared between variants) first on PATH for the test and returns the path
// of its invocation log. The package's TestMain must call Main.
func Install(t testing.TB, variant string) string {
	t.Helper()
	once.Do(func() { dir, dirErr = os.MkdirTemp("", "gos3-fake-restic-") })
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	vdir := filepath.Join(dir, variant)
	name := "restic"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if _, err := os.Stat(filepath.Join(vdir, name)); err != nil {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(vdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vdir, name), raw, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "restic.log")
	t.Setenv("PATH", vdir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvFake, "1")
	t.Setenv(EnvLog, log)
	t.Setenv(EnvFail, "")
	t.Setenv(EnvVersion, "")
	t.Setenv(EnvDumpDelay, "")
	return log
}

// Calls returns how often the fake logging to log ran the subcommand cmd
// (e.g. "backup", "cat config").
func Calls(t testing.TB, log, cmd string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSuffix(line, "\r") == cmd {
			n++
		}
	}
	return n
}

// errNotFound is returned by a store for a missing file.
var errNotFound = errors.New("not found")

// store holds the files of one fake repository.
type store interface {
	// create prepares an empty repository (directory or bucket).
	create() error
	// get copies the file name to w.
	get(name string, w io.Writer) error
	// exists reports whether the file name exists.
	exists(name string) (bool, error)
	// put stores the size bytes of r as the file name.
	put(name string, r io.ReaderAt, size int64) error
	// list returns the names of the files in the directory name.
	list(name string) ([]string, error)
}

// localStore is a repository in a local directory.
type localStore struct{ root string }

func (s localStore) create() error {
	return os.MkdirAll(filepath.Join(s.root, "snapshots"), 0o700)
}

func (s localStore) get(name string, w io.Writer) error {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

func (s localStore) exists(name string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s localStore) put(name string, r io.ReaderAt, size int64) error {
	dst := filepath.Join(s.root, filepath.FromSlash(name))
	tmp, err := os.CreateTemp(filepath.Dir(dst), "incoming-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.NewSectionReader(r, 0, size)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (s localStore) list(name string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, err
}

// s3Store is a repository in restic's s3 backend.
type s3Store struct {
	ctx    context.Context
	c      *s3client.Client
	bucket string
	prefix string // "" or ending in "/"
}

// newS3Store returns the store of the repository spec (the part after
// "s3:") with restic's extended options opts.
func newS3Store(spec string, opts map[string]string) (*s3Store, error) {
	scheme := "https://"
	for _, p := range []string{"http://", "https://"} {
		if strings.HasPrefix(spec, p) {
			scheme, spec = p, strings.TrimPrefix(spec, p)
		}
	}
	host, rest, _ := strings.Cut(spec, "/")
	bucket, prefix, _ := strings.Cut(rest, "/")
	if host == "" || bucket == "" {
		return nil, fmt.Errorf("invalid s3 repository %q", spec)
	}
	if prefix = strings.Trim(prefix, "/"); prefix != "" {
		prefix += "/"
	}
	region := opts["s3.region"]
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	var pathStyle bool
	switch lookup := opts["s3.bucket-lookup"]; lookup {
	case "path":
		pathStyle = true
	case "dns":
	case "", "auto":
		pathStyle = !strings.HasSuffix(strings.Split(host, ":")[0], "amazonaws.com")
	default:
		return nil, fmt.Errorf("bad bucket-lookup style %q", lookup)
	}
	c, err := s3client.New(s3client.Config{
		Endpoint:    scheme + host,
		Region:      region,
		AccessKey:   os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey:   os.Getenv("AWS_SECRET_ACCESS_KEY"),
		PathStyle:   pathStyle,
		MaxAttempts: 2,
	})
	if err != nil {
		return nil, err
	}
	return &s3Store{ctx: context.Background(), c: c, bucket: bucket, prefix: prefix}, nil
}

func (s *s3Store) create() error {
	err := s.c.HeadBucket(s.ctx, s.bucket)
	if s3client.IsNotFound(err) {
		return s.c.CreateBucket(s.ctx, s.bucket)
	}
	return err
}

func (s *s3Store) get(name string, w io.Writer) error {
	body, _, err := s.c.GetObject(s.ctx, s.bucket, s.prefix+name, s3client.GetOptions{})
	if s3client.IsNotFound(err) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = io.Copy(w, body)
	return err
}

func (s *s3Store) exists(name string) (bool, error) {
	_, err := s.c.HeadObject(s.ctx, s.bucket, s.prefix+name)
	if s3client.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *s3Store) put(name string, r io.ReaderAt, size int64) error {
	_, err := s.c.PutObject(s.ctx, s.bucket, s.prefix+name, r, size, s3client.PutOptions{})
	return err
}

func (s *s3Store) list(name string) ([]string, error) {
	var names []string
	dir := s.prefix + name + "/"
	err := s.c.Walk(s.ctx, s.bucket, dir, "", func(o s3client.ObjectAttrs) error {
		names = append(names, path.Base(o.Key))
		return nil
	})
	return names, err
}

// fatal reports a fatal error the way restic does and returns its exit
// code 1.
func fatal(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "Fatal: "+format+"\n", args...)
	return 1
}

// run is the fake's main function.
func run(args []string) int {
	var repo, sub string
	var rest []string
	opts := map[string]string{}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--repo", "--retry-lock", "--http-user-agent", "--cache-dir", "-o", "--option":
			if i+1 >= len(args) {
				return fatal("flag %s needs a value", a)
			}
			switch a {
			case "--repo":
				repo = args[i+1]
			case "-o", "--option":
				k, v, _ := strings.Cut(args[i+1], "=")
				opts[k] = v
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
	if log := os.Getenv(EnvLog); log != "" {
		if f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, sub)
			_ = f.Close()
		}
	}
	switch sub {
	case "version":
		v := os.Getenv(EnvVersion)
		if v == "" {
			v = "0.17.3"
		}
		fmt.Printf("restic %s compiled with go1.23 on %s/%s\n", v, runtime.GOOS, runtime.GOARCH)
		return 0
	case "noise":
		// Plenty of diagnostics, to check that they are bounded.
		line := append(bytes.Repeat([]byte("x"), 1023), '\n')
		for range 8 << 10 {
			_, _ = os.Stderr.Write(line)
		}
		return 1
	}
	if os.Getenv(EnvFail) == sub {
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Fprintf(os.Stderr, "progress...\nFatal: unable to save snapshot: password %s rejected\n", os.Getenv("RESTIC_PASSWORD"))
		return 1
	}
	if os.Getenv("RESTIC_PASSWORD") == "" {
		return fatal("no password")
	}
	if repo == "" {
		return fatal("Please specify repository location (-r or --repository-file)")
	}
	var st store = localStore{root: repo}
	if spec, ok := strings.CutPrefix(repo, "s3:"); ok {
		s, err := newS3Store(spec, opts)
		if err != nil {
			return fatal("%v", err)
		}
		st = s
	}
	if sub != "init" {
		ok, err := st.exists("config")
		if err != nil {
			return fatal("unable to open config file: %v", err)
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "Fatal: repository does not exist: unable to open config file")
			return 10
		}
	}
	switch sub {
	case "cat config":
		if err := st.get("config", os.Stdout); err != nil {
			return fatal("%v", err)
		}
	case "init":
		if ok, err := st.exists("config"); err != nil || ok {
			return fatal("create repository: config file already exists (%v)", err)
		}
		if err := st.create(); err != nil {
			return fatal("create repository: %v", err)
		}
		cfg := []byte(`{"version":2}`)
		if err := st.put("config", bytes.NewReader(cfg), int64(len(cfg))); err != nil {
			return fatal("create repository: %v", err)
		}
	case "backup":
		return backup(st)
	case "dump":
		if d, err := time.ParseDuration(os.Getenv(EnvDumpDelay)); err == nil {
			time.Sleep(d)
		}
		if len(rest) != 2 || rest[1] != "/object" {
			return fatal("bad dump arguments")
		}
		if err := st.get("snapshots/"+rest[0], os.Stdout); err != nil {
			return fatal("no matching ID found for prefix %q: %v", rest[0], err)
		}
	case "snapshots":
		ids, err := st.list("snapshots")
		if err != nil {
			return fatal("%v", err)
		}
		_ = json.NewEncoder(os.Stdout).Encode(ids)
	default:
		return fatal("unknown command %q", sub)
	}
	return 0
}

// backup archives stdin as a new snapshot. The snapshot exists only once
// the input ended, as with restic.
func backup(st store) int {
	tmp, err := os.CreateTemp("", "fakerestic-")
	if err != nil {
		return fatal("%v", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, os.Stdin)
	if err != nil {
		return fatal("%v", err)
	}
	var id [32]byte
	_, _ = rand.Read(id[:])
	sid := hex.EncodeToString(id[:])
	if err := st.put("snapshots/"+sid, tmp, size); err != nil {
		return fatal("unable to save snapshot: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{"message_type": "status", "percent_done": 1})
	_ = enc.Encode(map[string]any{"message_type": "summary", "snapshot_id": sid})
	return 0
}
