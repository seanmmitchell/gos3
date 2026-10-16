package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/server"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// TestMain lets the test binary act as the fake restic of restictest.
func TestMain(m *testing.M) { restictest.Main(m) }

const (
	resticTestAK = "resticcliaccess"
	resticTestSK = "restic-cli-secret-0123456789"
)

var resticTestCreds = auth.Credentials{AccessKey: resticTestAK, SecretKey: resticTestSK}

func TestShellQuoting(t *testing.T) {
	bash := map[string]string{
		"":                 "''",
		"plain":            "'plain'",
		"it's":             `'it'\''s'`,
		`$HOME "x" \n ;|&`: `'$HOME "x" \n ;|&'`,
		"a\nb":             "'a\nb'",
	}
	for in, want := range bash {
		if got := bashQuote(in); got != want {
			t.Errorf("bashQuote(%q) = %s, want %s", in, got, want)
		}
	}
	ps := map[string]string{
		"":               "''",
		"it's":           "'it''s'",
		"$env:X `n":      "'$env:X `n'",
		"a\u2019b\u2018": "'a\u2019\u2019b\u2018\u2018'",
	}
	for in, want := range ps {
		if got := powerShellQuote(in); got != want {
			t.Errorf("powerShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	if got := commentSafe("a\nb\x7fc"); got != "a?b?c" {
		t.Errorf("commentSafe = %q", got)
	}
}

// trickyRepo is a repository whose secrets need quoting.
func trickyRepo() resticRepo {
	return resticRepo{
		bucket: "photos", backing: "cold", purpose: purposeArchive,
		loc: restic.Location{
			Repository: "s3:http://127.0.0.1:9/b/p/restic/x",
			Password:   `pa'ss "w" $HOME ` + "`x` \u2019;\n2nd line",
			Env:        []string{"AWS_SECRET_ACCESS_KEY=s'ec$ret", "AWS_ACCESS_KEY_ID=AKID"},
			Options:    []string{"s3.bucket-lookup=path", "s3.region=eu-1"},
		},
	}
}

func TestWriteResticEnv(t *testing.T) {
	repo := trickyRepo()
	var b bytes.Buffer
	if err := writeResticEnv(&b, shellBash, repo); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`# gos3 restic: the per-version archive of bucket "photos" on backing "cold"`,
		"add -o s3.bucket-lookup=path -o s3.region=eu-1 to every restic command",
		"export RESTIC_REPOSITORY='s3:http://127.0.0.1:9/b/p/restic/x'\nexport RESTIC_PASSWORD=",
		"export AWS_ACCESS_KEY_ID='AKID'\nexport AWS_SECRET_ACCESS_KEY='s'\\''ec$ret'\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bash output lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	if err := writeResticEnv(&b, shellPowerShell, repo); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "$env:AWS_SECRET_ACCESS_KEY = 's''ec$ret'\n") {
		t.Errorf("powershell output:\n%s", out)
	}
	b.Reset()
	if err := writeResticEnv(&b, shellJSON, repo); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Bucket, Backing, Purpose string
		Env                      map[string]string
		Options                  []string
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil || doc.Env["RESTIC_PASSWORD"] != repo.loc.Password ||
		doc.Env["AWS_ACCESS_KEY_ID"] != "AKID" || len(doc.Options) != 2 || doc.Backing != "cold" || doc.Purpose != "archive" {
		t.Fatalf("json: %v %s", err, b.Bytes())
	}
	// Invalid names never reach a shell.
	repo.loc.Env = []string{"BAD NAME=x"}
	if err := writeResticEnv(io.Discard, shellBash, repo); err == nil {
		t.Error("invalid variable name accepted")
	}
}

// TestResticEnvShellRoundTrip evaluates the output of env in the real
// shells (when installed) and checks that every value comes back intact.
func TestResticEnvShellRoundTrip(t *testing.T) {
	repo := trickyRepo()
	shells := []struct {
		format, exe string
		args        func(script string) []string
		print       string
	}{
		{shellBash, "sh", func(s string) []string { return []string{"-c", s} },
			`printf '%s|%s' "$RESTIC_PASSWORD" "$AWS_SECRET_ACCESS_KEY"`},
		{shellPowerShell, "pwsh", func(s string) []string { return []string{"-NoProfile", "-NonInteractive", "-Command", s} },
			`[Console]::Out.Write($env:RESTIC_PASSWORD + '|' + $env:AWS_SECRET_ACCESS_KEY)`},
	}
	if runtime.GOOS == "windows" {
		shells = append(shells, shells[1])
		shells[2].exe = "powershell"
	}
	tested := 0
	for _, sh := range shells {
		exe, err := exec.LookPath(sh.exe)
		if err != nil {
			continue
		}
		var b bytes.Buffer
		if err := writeResticEnv(&b, sh.format, repo); err != nil {
			t.Fatal(err)
		}
		script := b.String() + sh.print
		if sh.format == shellPowerShell {
			// The password's line break is part of the value.
			script = "[Console]::OutputEncoding = [Text.Encoding]::UTF8\n" + script
		}
		out, err := exec.Command(exe, sh.args(script)...).Output()
		if err != nil {
			t.Errorf("%s: %v", sh.exe, err)
			continue
		}
		got := strings.ReplaceAll(string(out), "\r\n", "\n")
		if want := repo.loc.Password + "|s'ec$ret"; got != want {
			t.Errorf("%s: got %q, want %q", sh.exe, got, want)
		}
		tested++
	}
	if tested == 0 {
		t.Skip("no shell found")
	}
}

func TestResticUsageErrors(t *testing.T) {
	clearEnv(t)
	for _, args := range [][]string{
		{"restic"},
		{"restic", "frobnicate"},
		{"restic", "env", "--server", "http://127.0.0.1:1"},                                    // no bucket
		{"restic", "env", "--bucket", "b"},                                                     // no source
		{"restic", "env", "--bucket", "b", "--server", "http://x", "--config", "c.yaml"},       // both sources
		{"restic", "env", "--bucket", "b", "--offline", "--snapshots", "--backing", "x"},       // backing of snapshots
		{"restic", "env", "--bucket", "b", "--server", "http://x", "--bucket-id", "abc"},       // bucket-id online
		{"restic", "env", "--bucket", "b", "--offline", "--bucket-id", "../x"},                 // bad bucket-id
		{"restic", "env", "--bucket", "b", "--offline", "--shell", "fish"},                     // shell
		{"restic", "env", "--bucket", "b", "--offline", "extra"},                               // positional
		{"restic", "exec", "--bucket", "b", "--offline"},                                       // no restic arguments
		{"restic", "objects", "--bucket", "b", "--offline", "--key", "k", "--prefix", "p"},     // key and prefix
		{"restic", "recover", "--bucket", "b", "--offline"},                                    // no key
		{"restic", "recover", "--bucket", "b", "--offline", "--key", "k", "--at", "yesterday"}, // bad time
		{"restic", "objects", "--bucket", "b", "--offline", "--snapshots"},                     // archive only
	} {
		if code, _, stderr := runCmd(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2: %s", args, code, stderr)
		}
	}
	if code, out, _ := runCmd(t, "restic", "help"); code != 0 || !strings.Contains(out, "gos3 restic recover") {
		t.Errorf("help: %d %s", code, out)
	}
	// Online without keys.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if code, _, stderr := runCmd(t, "restic", "env", "--bucket", "b", "--server", "http://127.0.0.1:1"); code != 1 || !strings.Contains(stderr, "access key") {
		t.Errorf("no keys: %d %s", code, stderr)
	}
}

// testServer is a gos3 server for the CLI tests.
type testServer struct {
	srv  *server.Server
	url  string
	stop func()
}

// startServer serves cfg on a loopback port until stop or the end of the
// test.
func startServer(t *testing.T, cfg config.Config) *testServer {
	t.Helper()
	cfg.Server.Address = "127.0.0.1:0"
	cfg.Discovery.Enabled = false
	cfg.Logging.AccessLog = false
	cfg.Auth.RootAccessKey, cfg.Auth.RootSecretKey = resticTestAK, resticTestSK
	srv, err := server.New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	select {
	case <-srv.Ready():
	case err := <-done:
		t.Fatalf("serve: %v", err)
	}
	ts := &testServer{srv: srv, url: srv.URL()}
	stopped := false
	ts.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}
	t.Cleanup(ts.stop)
	return ts
}

// call sends a signed request to the server and fails the test on a status
// other than want.
func (ts *testServer) call(t *testing.T, method, path string, body []byte, hdr map[string]string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, ts.url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if err := auth.SignRequest(req, resticTestCreds, "us-east-1", "s3", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	return data
}

// online returns the flags selecting the server online.
func (ts *testServer) online() []string {
	return []string{"--server", ts.url, "--access-key", resticTestAK, "--secret-key", resticTestSK, "--no-cache"}
}

// envDoc is the JSON output of `gos3 restic env`.
type envDoc struct {
	Bucket, Backing, Purpose string
	Env                      map[string]string
	Options                  []string
}

func resticEnvJSON(t *testing.T, args ...string) envDoc {
	t.Helper()
	code, out, stderr := runCmd(t, append([]string{"restic", "env", "--shell", "json"}, args...)...)
	if code != 0 {
		t.Fatalf("restic env %v: exit %d: %s", args, code, stderr)
	}
	var doc envDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("env output: %v\n%s", err, out)
	}
	return doc
}

func resticObjects(t *testing.T, args ...string) []archivedVersion {
	t.Helper()
	code, out, stderr := runCmd(t, append([]string{"restic", "objects", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("restic objects %v: exit %d: %s", args, code, stderr)
	}
	var list []archivedVersion
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("objects output: %v\n%s", err, out)
	}
	return list
}

// TestResticFS: env, objects, recover and exec, online with the server
// running and offline from its data directory once it stopped, for a
// bucket on an fs backing whose every write is archived (fake restic).
func TestResticFS(t *testing.T) {
	clearEnv(t)
	restictest.Install(t, "cli")
	data := t.TempDir()
	cfg := config.Default()
	cfg.Storage.Path = data
	ts := startServer(t, cfg)
	ts.call(t, "PUT", "/photos", nil, nil, 200)
	ts.call(t, "PUT", "/_gos3/admin/v1/buckets/photos/encryption-provider", []byte(`{"provider":"restic","scope":"all"}`), nil, 200)
	ts.call(t, "PUT", "/photos/cat.jpg", []byte("first cat"), nil, 200)
	time.Sleep(50 * time.Millisecond)
	ts.call(t, "PUT", "/photos/cat.jpg", []byte("second cat"), nil, 200)
	ts.call(t, "PUT", "/photos/dir/dog%20one.jpg", []byte("dog"), nil, 200)
	ts.call(t, "PUT", "/_gos3/admin/v1/buckets/photos/restic/snapshots/config", []byte(`{"enabled":false}`), nil, 200)

	online := resticEnvJSON(t, append(ts.online(), "--bucket", "photos")...)
	repo, pw := online.Env["RESTIC_REPOSITORY"], online.Env["RESTIC_PASSWORD"]
	if repo == "" || len(pw) != 64 || online.Backing != "default" || online.Purpose != "archive" {
		t.Fatalf("online env: %+v", online)
	}
	if !strings.HasPrefix(filepath.Clean(repo), filepath.Join(filepath.Clean(data), ".gos3", "restic")) {
		t.Errorf("repository %s is not in the data directory", repo)
	}

	list := resticObjects(t, append(ts.online(), "--bucket", "photos")...)
	if len(list) != 3 || list[0].Key != "cat.jpg" || list[1].Key != "cat.jpg" || list[2].Key != "dir/dog one.jpg" ||
		!list[0].Archived.Before(list[1].Archived) || list[2].Size != 3 {
		t.Fatalf("objects: %+v", list)
	}
	if only := resticObjects(t, append(ts.online(), "--bucket", "photos", "--prefix", "dir/")...); len(only) != 1 {
		t.Errorf("objects --prefix: %+v", only)
	}
	code, out, stderr := runCmd(t, append([]string{"restic", "objects", "--bucket", "photos"}, ts.online()...)...)
	if code != 0 || !strings.Contains(out, "ARCHIVED") || !strings.Contains(out, "dir/dog one.jpg") {
		t.Errorf("objects table: %d %s %s", code, out, stderr)
	}

	// recover: the newest version, or the newest at or before --at.
	code, out, stderr = runCmd(t, append([]string{"restic", "recover", "--bucket", "photos", "--key", "cat.jpg"}, ts.online()...)...)
	if code != 0 || out != "second cat" {
		t.Fatalf("recover: %d %q %s", code, out, stderr)
	}
	file := filepath.Join(t.TempDir(), "cat.jpg")
	at := list[0].Archived.Format(time.RFC3339Nano)
	code, _, stderr = runCmd(t, append([]string{"restic", "recover", "--bucket", "photos", "--key", "cat.jpg", "--at", at, "-o", file}, ts.online()...)...)
	if got, _ := os.ReadFile(file); code != 0 || string(got) != "first cat" {
		t.Fatalf("recover --at: %d %q %s", code, got, stderr)
	}
	before := list[0].Archived.Add(-time.Hour).Format(time.RFC3339)
	if code, _, stderr := runCmd(t, append([]string{"restic", "recover", "--bucket", "photos", "--key", "cat.jpg", "--at", before}, ts.online()...)...); code != 1 || !strings.Contains(stderr, "no version") {
		t.Errorf("recover before the first version: %d %s", code, stderr)
	}

	// Errors of the server are reported.
	code, _, stderr = runCmd(t, append([]string{"restic", "env", "--bucket", "nope"}, ts.online()...)...)
	if code != 1 || !strings.Contains(stderr, "HTTP 404") {
		t.Errorf("unknown bucket: %d %s", code, stderr)
	}
	code, _, stderr = runCmd(t, "restic", "env", "--bucket", "photos", "--server", ts.url, "--access-key", resticTestAK, "--secret-key", "wrong-secret-0123456789")
	if code != 1 || !strings.Contains(stderr, "HTTP 401") {
		t.Errorf("wrong secret: %d %s", code, stderr)
	}
	if strings.Contains(stderr, pw) {
		t.Error("password in an error")
	}

	// Offline while the server runs: its metadata database is locked.
	code, _, stderr = runCmd(t, "restic", "env", "--bucket", "photos", "--data", data)
	if code != 1 || !strings.Contains(stderr, "locked by a running gos3 server") {
		t.Errorf("offline with the server running: %d %s", code, stderr)
	}

	ts.stop()

	// Offline: the same repository and password.
	offline := resticEnvJSON(t, "--bucket", "photos", "--data", data, "--no-cache")
	if filepath.Clean(offline.Env["RESTIC_REPOSITORY"]) != filepath.Clean(repo) || offline.Env["RESTIC_PASSWORD"] != pw || offline.Backing != "default" {
		t.Fatalf("offline env: %+v, online %s", offline, repo)
	}
	// From a configuration file, too.
	cfgFile := filepath.Join(t.TempDir(), "gos3.yaml")
	if err := os.WriteFile(cfgFile, []byte("storage:\n  path: "+strings.ReplaceAll(data, `\`, `/`)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if doc := resticEnvJSON(t, "--bucket", "photos", "--config", cfgFile); doc.Env["RESTIC_PASSWORD"] != pw {
		t.Fatalf("offline env from a file: %+v", doc)
	}
	// The snapshot repository of a "bucket" target is the bucket's own.
	if doc := resticEnvJSON(t, "--bucket", "photos", "--data", data, "--snapshots"); doc.Env["RESTIC_PASSWORD"] != pw || doc.Purpose != "snapshots" {
		t.Fatalf("offline snapshot env: %+v", doc)
	}
	if list := resticObjects(t, "--bucket", "photos", "--data", data, "--no-cache"); len(list) != 3 {
		t.Fatalf("offline objects: %+v", list)
	}
	code, out, stderr = runCmd(t, "restic", "recover", "--bucket", "photos", "--data", data, "--no-cache", "--key", "dir/dog one.jpg")
	if code != 0 || out != "dog" {
		t.Fatalf("offline recover: %d %q %s", code, out, stderr)
	}
	code, out, stderr = runCmd(t, "restic", "exec", "--bucket", "photos", "--data", data, "--no-cache", "--", "snapshots", "--json")
	if code != 0 || strings.Count(out, `"gos3-object"`) != 3 {
		t.Fatalf("offline exec: %d %s %s", code, out, stderr)
	}
	// restic's exit code is passed on.
	if code, _, _ := runCmd(t, "restic", "exec", "--bucket", "photos", "--data", data, "--no-cache", "--", "frobnicate"); code != 1 {
		t.Errorf("failing restic: exit %d", code)
	}
	// Unknown buckets and backings.
	if code, _, stderr := runCmd(t, "restic", "env", "--bucket", "nope", "--data", data); code != 1 || !strings.Contains(stderr, `"nope"`) {
		t.Errorf("offline unknown bucket: %d %s", code, stderr)
	}
	if code, _, stderr := runCmd(t, "restic", "env", "--bucket", "photos", "--data", data, "--backing", "other"); code != 1 || !strings.Contains(stderr, `no backing "other"`) {
		t.Errorf("offline unknown backing: %d %s", code, stderr)
	}

	// A deleted bucket's archive stays, found by the name in its record.
	ts = startServer(t, cfg)
	for _, key := range []string{"cat.jpg", "dir/dog%20one.jpg"} {
		ts.call(t, "DELETE", "/photos/"+key, nil, nil, 204)
	}
	ts.call(t, "DELETE", "/photos", nil, nil, 204)
	ts.stop()
	code, out, stderr = runCmd(t, "restic", "env", "--bucket", "photos", "--data", data, "--shell", "json")
	if code != 0 || !strings.Contains(out, pw) || !strings.Contains(stderr, "no longer exists") {
		t.Fatalf("deleted bucket: %d %s %s", code, out, stderr)
	}
}

// TestResticOfflineS3: offline, the password of a wrapped bucket's archive
// on an s3 backing is unwrapped with the backing's encryption key, and
// restic reaches the repository with the backing's credentials.
func TestResticOfflineS3(t *testing.T) {
	clearEnv(t)
	restictest.Install(t, "cli-s3")
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "coldstore")
	kek := make([]byte, config.KEKSize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	yaml := `storage:
  path: ` + strings.ReplaceAll(data, `\`, `/`) + `
  backings:
    - name: cold
      backend: s3
      s3:
        endpoint: ` + remote.URL + `
        home_bucket: coldstore
        home_prefix: gos3/
        addressing: path
        access_key: ` + s3clienttest.AccessKey + `
`
	cfgFile := filepath.Join(t.TempDir(), "gos3.yaml")
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	// Secrets from the environment, as an operator would pass them.
	t.Setenv("GOS3_BACKING_COLD_SECRET_KEY", s3clienttest.SecretKey)
	t.Setenv("GOS3_BACKING_COLD_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(kek))
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	ts := startServer(t, cfg)
	ts.call(t, "PUT", "/vault", nil, map[string]string{"x-gos3-backing": "cold"}, 200)
	ts.call(t, "PUT", "/_gos3/admin/v1/buckets/vault/encryption-provider", []byte(`{"provider":"restic","scope":"all"}`), nil, 200)
	ts.call(t, "PUT", "/vault/doc.txt", []byte("cold document"), nil, 200)
	online := resticEnvJSON(t, append(ts.online(), "--bucket", "vault")...)
	if online.Backing != "cold" || !strings.HasPrefix(online.Env["RESTIC_REPOSITORY"], "s3:"+remote.URL+"/coldstore/gos3/restic/") ||
		online.Env["AWS_ACCESS_KEY_ID"] != s3clienttest.AccessKey || len(online.Options) == 0 {
		t.Fatalf("online env: %+v", online)
	}
	ts.stop()

	offline := resticEnvJSON(t, "--bucket", "vault", "--config", cfgFile, "--no-cache")
	for _, name := range []string{"RESTIC_REPOSITORY", "RESTIC_PASSWORD", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if offline.Env[name] != online.Env[name] || offline.Env[name] == "" {
			t.Errorf("offline %s = %q, online %q", name, offline.Env[name], online.Env[name])
		}
	}
	if offline.Backing != "cold" || strings.Join(offline.Options, " ") != strings.Join(online.Options, " ") {
		t.Errorf("offline: %+v, online %+v", offline, online)
	}
	code, out, stderr := runCmd(t, "restic", "recover", "--bucket", "vault", "--config", cfgFile, "--no-cache", "--key", "doc.txt")
	if code != 0 || out != "cold document" {
		t.Fatalf("offline recover: %d %q %s", code, out, stderr)
	}
	// Without the encryption key nothing can be read.
	t.Setenv("GOS3_BACKING_COLD_ENCRYPTION_KEY", "")
	if code, _, stderr := runCmd(t, "restic", "env", "--bucket", "vault", "--config", cfgFile, "--backing", "cold"); code != 1 || !strings.Contains(stderr, "encryption_key") {
		t.Errorf("without the encryption key: %d %s", code, stderr)
	}
}
