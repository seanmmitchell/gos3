package restic

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

func TestMain(m *testing.M) { restictest.Main(m) }

func TestLongProgress(t *testing.T) {
	var out snapshotOutput
	progress := bytes.Repeat([]byte("{\"message_type\":\"status\",\"bytes_done\":12}\n"), 40000)
	snapshot := strings.Repeat("a", 64)
	progress = append(progress, []byte("{\"message_type\":\"summary\",\"snapshot_id\":\""+snapshot+"\"}")...)
	// Feed chunk boundaries through the middle of JSON records.
	for len(progress) > 0 {
		n := min(317, len(progress))
		if _, err := out.Write(progress[:n]); err != nil {
			t.Fatal(err)
		}
		progress = progress[n:]
	}
	if err := out.consume(); err != nil || out.snapshot != snapshot {
		t.Fatalf("lost backup summary: %q %v", out.snapshot, err)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][3]int{
		"restic 0.17.1 compiled with go1.22.5 on linux/amd64": {0, 17, 1},
		"restic 0.18.0-dev (compiled manually) compiled with": {0, 18, 0},
	} {
		if v, ok := parseVersion(in); !ok || v != want {
			t.Fatalf("parse %q = %v %v", in, v, ok)
		}
	}
	if _, ok := parseVersion("rustic 1.0"); ok {
		t.Fatal("parsed a foreign version")
	}
}

// Restic's diagnostics are bounded even when os/exec copies them.
func TestDiagnosticsBounded(t *testing.T) {
	restictest.Install(t, "current")
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Fatal(err)
	}
	var diag stderrTail
	if _, ok := any(&diag).(io.ReaderFrom); ok {
		t.Fatal("stderrTail must not implement io.ReaderFrom")
	}
	cmd := exec.Command(bin, "noise")
	cmd.Stderr = &diag
	_ = cmd.Run()
	if len(diag.buf) == 0 || len(diag.buf) > stderrTailSize {
		t.Fatalf("kept %d bytes of diagnostics", len(diag.buf))
	}
	if s := diag.redacted(); len(s) > 1100 || strings.ContainsAny(s, "\n\r") {
		t.Fatalf("redacted diagnostics not bounded to one line: %d bytes", len(s))
	}
	var small stderrTail
	_, _ = small.Write([]byte("a secret\nb\tc"))
	if got := small.redacted("secret"); got != "a [redacted] b c" {
		t.Fatalf("redacted = %q", got)
	}
}

func TestCommandErrorUnwraps(t *testing.T) {
	err := &cmdError{op: "x", err: errors.New("exit status 1"), detail: "Fatal: boom"}
	if !errors.Is(err, storage.ErrEncryptionUnavailable) || !strings.Contains(err.Error(), "Fatal: boom") {
		t.Fatalf("cmdError: %v", err)
	}
}

func TestValidSnapshot(t *testing.T) {
	if !ValidSnapshot(strings.Repeat("0a", 32)) {
		t.Fatal("valid ID refused")
	}
	for _, id := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if ValidSnapshot(id) {
			t.Fatalf("accepted %q", id)
		}
	}
}

func TestPasswordFromKey(t *testing.T) {
	if got := PasswordFromKey([]byte{0, 0xab, 0x10}); got != "00ab10" {
		t.Fatalf("password = %q", got)
	}
}

func TestS3RepositoryLocation(t *testing.T) {
	loc := S3Repository{
		Endpoint: "https://s3.example.test/", Bucket: "data", Path: "/gos3/restic/abc/",
		Region: "eu-west-1", AccessKey: "AK", SecretKey: "SK", SessionToken: "TOK", PathStyle: true,
	}.Location("pw", "/cache")
	if loc.Repository != "s3:https://s3.example.test/data/gos3/restic/abc" {
		t.Fatalf("repository = %q", loc.Repository)
	}
	if !slices.Equal(loc.Options, []string{"s3.bucket-lookup=path", "s3.region=eu-west-1"}) {
		t.Fatalf("options = %q", loc.Options)
	}
	if !slices.Contains(loc.Env, "AWS_ACCESS_KEY_ID=AK") || !slices.Contains(loc.Env, "AWS_SECRET_ACCESS_KEY=SK") ||
		!slices.Contains(loc.Env, "AWS_SESSION_TOKEN=TOK") || !slices.Contains(loc.Env, "AWS_DEFAULT_REGION=eu-west-1") {
		t.Fatalf("env = %q", loc.Env)
	}
	if loc.Password != "pw" || loc.CacheDir != "/cache" {
		t.Fatalf("location = %+v", loc)
	}
	secrets := loc.secrets()
	for _, s := range []string{"pw", "AK", "SK", "TOK"} {
		if !slices.Contains(secrets, s) {
			t.Fatalf("secret %q not redacted: %q", s, secrets)
		}
	}
	if slices.Contains(secrets, "eu-west-1") {
		t.Fatal("region treated as a secret")
	}
	dns := S3Repository{Endpoint: "https://s3.example.test", Bucket: "data"}.Location("pw", "")
	if dns.Repository != "s3:https://s3.example.test/data" || !slices.Equal(dns.Options, []string{"s3.bucket-lookup=dns"}) {
		t.Fatalf("dns location = %+v", dns)
	}
}
