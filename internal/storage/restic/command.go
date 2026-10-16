package restic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// MinVersion is the oldest supported restic release.
var MinVersion = [3]int{0, 17, 1}

// verified caches the restic executables (by path) whose version was
// checked. Failures are not cached, so installing a newer restic at the same
// path takes effect without a restart.
var verified sync.Map

// stopDelay bounds how long restic may take to exit after it was
// interrupted (request ended, write aborted) before it is killed.
const stopDelay = 15 * time.Second

// cacheDirPerm is the mode of a cache directory created for restic.
const cacheDirPerm = 0o700

// errMissing returns the error for a missing or too old restic.
func errMissing(detail string) error {
	return fmt.Errorf("%w: restic %d.%d.%d or newer is required on the server PATH: %s",
		storage.ErrEncryptionUnavailable, MinVersion[0], MinVersion[1], MinVersion[2], detail)
}

// executable returns the path of the restic executable on PATH after
// checking (once per path) that it is at least MinVersion.
func (rt *Runtime) executable(ctx context.Context) (string, error) {
	bin, err := rt.lookPath()("restic")
	if err != nil {
		return "", errMissing("restic executable not found")
	}
	if _, ok := verified.Load(bin); ok {
		return bin, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	hideWindow(cmd)
	cmd.Env = baseEnv()
	var diag stderrTail
	cmd.Stderr = &diag
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("restic: version: %w", ctx.Err())
		}
		return "", errMissing(fmt.Sprintf("restic version: %v: %s", err, diag.redacted()))
	}
	v, ok := parseVersion(string(out))
	if !ok {
		return "", errMissing(fmt.Sprintf("unrecognized restic version output %q", clean(string(out), 120)))
	}
	if compareVersions(v, MinVersion) < 0 {
		return "", errMissing(fmt.Sprintf("found restic %d.%d.%d at %s", v[0], v[1], v[2], bin))
	}
	verified.Store(bin, struct{}{})
	return bin, nil
}

// parseVersion extracts the release from `restic version` output
// ("restic 0.17.1 compiled with go1.22.5 on linux/amd64"; development
// builds append "-dev" and the like to the release).
func parseVersion(out string) ([3]int, bool) {
	var v [3]int
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] != "restic" {
		return v, false
	}
	release, _, _ := strings.Cut(fields[1], "-")
	parts := strings.Split(strings.TrimPrefix(release, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// compareVersions compares two releases like strings.Compare.
func compareVersions(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// baseEnv returns the server's environment without restic and AWS
// settings: restic must not inherit a repository, password, command or
// credentials from a host that happens to use restic for unrelated backups.
func baseEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		name = strings.ToUpper(name)
		if !strings.HasPrefix(name, "RESTIC_") && !strings.HasPrefix(name, "AWS_") {
			env = append(env, e)
		}
	}
	return env
}

// Command returns a restic command running args against the repository
// loc: global options (--repo, --retry-lock, --http-user-agent
// storage.ResticUserAgent, the cache and loc.Options) come first. Its
// environment is the server's without RESTIC_* and AWS_* variables, plus
// RESTIC_PASSWORD and loc.Env. When ctx ends, restic is interrupted (so
// that it removes its lock) and killed if it has not exited 15 seconds
// later.
//
// Command fails (wrapping storage.ErrEncryptionUnavailable) when no
// restic of at least MinVersion is on PATH.
func (rt *Runtime) Command(ctx context.Context, loc Location, args ...string) (*exec.Cmd, error) {
	return rt.command(ctx, loc, cmdOptions{}, args...)
}

// defaultRetryLock is how long restic waits for a conflicting repository
// lock by default.
const defaultRetryLock = 30 * time.Second

// cmdOptions adjusts a restic command (see Runtime.command).
type cmdOptions struct {
	// retryLock is how long restic waits for a conflicting lock (0:
	// defaultRetryLock).
	retryLock time.Duration
	// env holds environment entries added after the location's.
	env []string
}

// command implements Command with the options o.
func (rt *Runtime) command(ctx context.Context, loc Location, o cmdOptions, args ...string) (*exec.Cmd, error) {
	bin, err := rt.executable(ctx)
	if err != nil {
		return nil, err
	}
	retryLock := o.retryLock
	if retryLock <= 0 {
		retryLock = defaultRetryLock
	}
	global := []string{"--repo", loc.Repository, "--retry-lock", retryLock.String(), "--http-user-agent", storage.ResticUserAgent}
	if loc.CacheDir != "" {
		if err := os.MkdirAll(loc.CacheDir, cacheDirPerm); err != nil {
			return nil, fmt.Errorf("restic: create cache directory: %w", err)
		}
		global = append(global, "--cache-dir", loc.CacheDir)
	} else {
		global = append(global, "--no-cache")
	}
	for _, o := range loc.Options {
		global = append(global, "-o", o)
	}
	cmd := exec.CommandContext(ctx, bin, append(global, args...)...)
	hideWindow(cmd)
	cmd.Cancel = func() error { return interrupt(cmd.Process) }
	cmd.WaitDelay = stopDelay
	env := append(baseEnv(), "RESTIC_PASSWORD="+loc.Password)
	env = append(env, loc.Env...)
	cmd.Env = append(env, o.env...)
	return cmd, nil
}

// cmdError is a failed restic invocation. It wraps
// storage.ErrEncryptionUnavailable and the underlying error, and carries a
// bounded, redacted tail of restic's diagnostics.
type cmdError struct {
	op     string
	err    error
	detail string
}

func (e *cmdError) Error() string {
	s := "restic: " + e.op + ": " + e.err.Error()
	if e.detail != "" {
		s += ": " + e.detail
	}
	return s
}

func (e *cmdError) Unwrap() []error { return []error{storage.ErrEncryptionUnavailable, e.err} }

// failure converts the failure err of the restic operation op on the
// repository loc: the request's context error when ctx ended (restic was
// interrupted because the client went away), otherwise a logged *cmdError
// with the redacted tail of diag (which may be nil).
func (rt *Runtime) failure(ctx context.Context, op string, err error, diag *stderrTail, loc Location) error {
	e := rt.fault(ctx, op, err, diag, loc)
	if _, ok := e.(*cmdError); ok {
		rt.log().Warn("restic failed", "op", op, "repository", loc.Repository, "err", e.Error())
	}
	return e
}

// fault is failure without logging.
func (rt *Runtime) fault(ctx context.Context, op string, err error, diag *stderrTail, loc Location) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("restic: %s: %w", op, ctxErr)
	}
	e := &cmdError{op: op, err: err}
	if diag != nil {
		e.detail = diag.redacted(loc.secrets()...)
	}
	return e
}

// exitCode returns restic's exit code when err reports that it exited
// unsuccessfully, and -1 otherwise.
func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// exitNoRepository is restic's documented exit code for a repository that
// does not exist.
const exitNoRepository = 10

// stderrTailSize bounds the restic diagnostics kept per invocation.
const stderrTailSize = 4 << 10

// stderrTail keeps the last stderrTailSize bytes written to it. It has no
// ReadFrom method on purpose: os/exec copies a pipe with io.Copy, which
// would use one and bypass the bound.
type stderrTail struct{ buf []byte }

func (t *stderrTail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > stderrTailSize {
		p = p[len(p)-stderrTailSize:]
	}
	if over := len(t.buf) + len(p) - stderrTailSize; over > 0 {
		t.buf = t.buf[:copy(t.buf, t.buf[over:])]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// redacted returns the end of the kept diagnostics on one line, at most
// 1 KiB, with every non-empty secret replaced.
func (t *stderrTail) redacted(secrets ...string) string {
	s := string(t.buf)
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return clean(s, 1024)
}

// clean collapses whitespace and control characters of s and keeps its
// last max bytes (restic prints the fatal error last).
func clean(s string, max int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(s) > max {
		s = "..." + strings.ToValidUTF8(s[len(s)-max:], "")
	}
	return s
}
