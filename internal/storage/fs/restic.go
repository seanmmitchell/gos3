package fs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Restic owns immutable snapshots. Keeping snapshots after S3 deletes provides
// an archive and makes open readers safe across concurrent overwrites/deletes.
// Operators must not forget snapshots still referenced by gos3 metadata.
//
// Every restic failure is reported wrapping storage.ErrEncryptionUnavailable
// (or the request's context error when the request ended), never as a plain
// error: it concerns one bucket's archive, not the health of this backend.

// resticRepository is how gos3 reaches one bucket's restic repository. The
// JSON form is the recovery record keys/<bucket-id>.restic.json, which keeps
// the repository and its password after the bucket is deleted; S3
// credentials stay in the backing settings (restic-backing.json).
type resticRepository struct {
	Initialized  bool   `json:"initialized,omitempty"`
	Repository   string `json:"repository"`
	Password     string `json:"password"`
	AccessKey    string `json:"-"`
	SecretKey    string `json:"-"`
	SessionToken string `json:"-"`
	Region       string `json:"-"`
	// CacheDir is restic's local cache (--cache-dir); "" runs without one.
	CacheDir string `json:"-"`
}

// withS3 adds the S3 credentials of the backing's restic settings s.
func (cfg *resticRepository) withS3(s *storage.ResticS3) {
	if s != nil {
		cfg.AccessKey, cfg.SecretKey, cfg.SessionToken, cfg.Region = s.AccessKey, s.SecretKey, s.SessionToken, s.Region
	}
}

// secrets returns the values that must never appear in logs or errors.
func (cfg *resticRepository) secrets() []string {
	return []string{cfg.Password, cfg.SecretKey, cfg.SessionToken, cfg.AccessKey}
}

// resticSettings returns how to reach the repository of the bucket with ID
// id under the backing's current restic settings.
func (b *Backend) resticSettings(id string) (resticRepository, error) {
	key, err := b.bucketKey(id, false)
	if err != nil {
		return resticRepository{}, err
	}
	s3 := b.resticBacking()
	cfg := resticRepository{
		Repository: b.resticRepository(id, s3),
		Password:   resticPassword(key),
		CacheDir:   filepath.Join(b.sysDir, resticCacheDir),
	}
	cfg.withS3(s3)
	return cfg, nil
}

// resticCacheDir (below the system directory) is restic's persistent cache
// of repository indexes and metadata, shared by all of the backing's
// repositories. Without it every invocation would download every index
// file of the repository again. Its contents are encrypted by restic and
// can be deleted at any time (restic rebuilds them).
const resticCacheDir = "restic-cache"

// snapshotRefSize is the size of a restic version's local data file, which
// holds only its snapshot ID in hex.
const snapshotRefSize = 64

// validSnapshot reports whether id is a restic snapshot ID as stored in a
// data file.
func validSnapshot(id string) bool { return len(id) == snapshotRefSize && validHex(id) }

// --- runtime state ---

// resticState is a Backend's in-process restic state.
type resticState struct {
	// locationMu is held shared while a repository is prepared and
	// exclusively while the backing's restic location changes, so that no
	// repository is initialized at a location that is being replaced.
	locationMu sync.RWMutex

	// generation counts changes of the backing's restic settings; a
	// preparation made under an older generation is checked again.
	generation atomic.Uint64

	mu      sync.Mutex
	buckets map[string]*resticBucket // by bucket ID
	spools  map[string]*resticSpool  // by bucket ID + "/" + snapshot ID
}

// resticBucket is the preparation state of one bucket's repository.
type resticBucket struct {
	mu sync.Mutex // serializes preparing this bucket's repository
	// prepared is the repository this process found (or made) initialized
	// and recorded in its recovery file, under the settings generation
	// gen; it is not checked again.
	prepared string
	gen      uint64
}

// isPrepared reports whether repo was prepared under the current settings;
// rb.mu is held.
func (s *resticState) isPrepared(rb *resticBucket, repo string) bool {
	return rb.prepared == repo && rb.gen == s.generation.Load()
}

// bucket returns the preparation state of the bucket with ID id.
func (s *resticState) bucket(id string) *resticBucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets == nil {
		s.buckets = make(map[string]*resticBucket)
	}
	rb := s.buckets[id]
	if rb == nil {
		rb = &resticBucket{}
		s.buckets[id] = rb
	}
	return rb
}

// forgetPrepared makes the next write of every bucket check its repository
// again (after the backing's restic settings changed).
func (s *resticState) forgetPrepared() { s.generation.Add(1) }

// --- invoking restic ---

// resticMinVersion is the oldest supported restic release.
var resticMinVersion = [3]int{0, 17, 1}

// resticVerified caches the restic executables (by path) whose version was
// checked. Failures are not cached, so installing a newer restic at the same
// path takes effect without a restart.
var resticVerified sync.Map

// resticStopDelay bounds how long restic may take to exit after it was
// interrupted (request ended, write aborted) before it is killed.
const resticStopDelay = 15 * time.Second

// errResticMissing is wrapped by the error for a missing or too old restic.
func errResticMissing(detail string) error {
	return fmt.Errorf("%w: restic %d.%d.%d or newer is required on the server PATH: %s",
		storage.ErrEncryptionUnavailable, resticMinVersion[0], resticMinVersion[1], resticMinVersion[2], detail)
}

// resticExecutable returns the path of the restic executable on PATH after
// checking (once per path) that it is at least resticMinVersion.
func resticExecutable(ctx context.Context) (string, error) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		return "", errResticMissing("restic executable not found")
	}
	if _, ok := resticVerified.Load(bin); ok {
		return bin, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	hideResticWindow(cmd)
	cmd.Env = resticEnv()
	var diag stderrTail
	cmd.Stderr = &diag
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("restic: version: %w", ctx.Err())
		}
		return "", errResticMissing(fmt.Sprintf("restic version: %v: %s", err, diag.redacted()))
	}
	v, ok := parseResticVersion(string(out))
	if !ok {
		return "", errResticMissing(fmt.Sprintf("unrecognized restic version output %q", diag.clean(string(out), 120)))
	}
	if compareVersions(v, resticMinVersion) < 0 {
		return "", errResticMissing(fmt.Sprintf("found restic %d.%d.%d at %s", v[0], v[1], v[2], bin))
	}
	resticVerified.Store(bin, struct{}{})
	return bin, nil
}

// parseResticVersion extracts the release from `restic version` output
// ("restic 0.17.1 compiled with go1.22.5 on linux/amd64"; development
// builds append "-dev" and the like to the release).
func parseResticVersion(out string) ([3]int, bool) {
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

// resticEnv returns the server's environment without restic and AWS
// settings: restic must not inherit a repository, password, command or
// credentials from a host that happens to use restic for unrelated backups.
func resticEnv() []string {
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

// resticCommand returns a restic command with args for the repository of
// the bucket with ID id.
func (b *Backend) resticCommand(ctx context.Context, id string, args ...string) (*exec.Cmd, error) {
	cfg, err := b.resticSettings(id)
	if err != nil {
		return nil, err
	}
	return resticCommandFor(ctx, cfg, args...)
}

// resticCommandFor returns a restic command with args for the repository
// cfg. When ctx ends, restic is interrupted (so that it removes its lock)
// and killed if it has not exited resticStopDelay later.
func resticCommandFor(ctx context.Context, cfg resticRepository, args ...string) (*exec.Cmd, error) {
	bin, err := resticExecutable(ctx)
	if err != nil {
		return nil, err
	}
	global := []string{"--repo", cfg.Repository, "--retry-lock", "30s", "--http-user-agent", storage.ResticUserAgent}
	if cfg.CacheDir != "" {
		if err := os.MkdirAll(cfg.CacheDir, dirPerm); err != nil {
			return nil, fmt.Errorf("fs: create restic cache: %w", err)
		}
		global = append(global, "--cache-dir", cfg.CacheDir)
	} else {
		global = append(global, "--no-cache")
	}
	cmd := exec.CommandContext(ctx, bin, append(global, args...)...)
	hideResticWindow(cmd)
	cmd.Cancel = func() error { return interruptRestic(cmd.Process) }
	cmd.WaitDelay = resticStopDelay
	cmd.Env = append(resticEnv(), "RESTIC_PASSWORD="+cfg.Password, "AWS_ACCESS_KEY_ID="+cfg.AccessKey, "AWS_SECRET_ACCESS_KEY="+cfg.SecretKey,
		"AWS_SESSION_TOKEN="+cfg.SessionToken, "AWS_DEFAULT_REGION="+cfg.Region)
	return cmd, nil
}

// resticError is a failed restic invocation. It wraps
// storage.ErrEncryptionUnavailable and the underlying error, and carries a
// bounded, redacted tail of restic's diagnostics.
type resticError struct {
	op     string
	err    error
	detail string
}

func (e *resticError) Error() string {
	s := "restic: " + e.op + ": " + e.err.Error()
	if e.detail != "" {
		s += ": " + e.detail
	}
	return s
}

func (e *resticError) Unwrap() []error { return []error{storage.ErrEncryptionUnavailable, e.err} }

// resticFailure converts the failure err of the restic operation op on the
// repository cfg: the request's context error when ctx ended (restic was
// interrupted because the client went away), otherwise a logged
// *resticError with the redacted tail of diag (which may be nil).
func (b *Backend) resticFailure(ctx context.Context, op string, err error, diag *stderrTail, cfg resticRepository) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("restic: %s: %w", op, ctxErr)
	}
	e := &resticError{op: op, err: err}
	if diag != nil {
		e.detail = diag.redacted(cfg.secrets()...)
	}
	b.log.Warn("fs: restic failed", "op", op, "repository", cfg.Repository, "err", e.Error())
	return e
}

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
	return t.clean(s, 1024)
}

// clean collapses whitespace and control characters of s and keeps its
// last max bytes (restic prints the fatal error last).
func (*stderrTail) clean(s string, max int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(s) > max {
		s = "..." + strings.ToValidUTF8(s[len(s)-max:], "")
	}
	return s
}

// --- preparing repositories ---

// recoveryPath returns the path of the recovery record of the bucket with
// ID id.
func (b *Backend) recoveryPath(id string) string {
	return filepath.Join(b.sysDir, "keys", id+".restic.json")
}

// readRecovery returns the recovery record at path (zero when it does not
// exist) and its raw content.
func readRecovery(path string) (resticRepository, []byte, error) {
	var rec resticRepository
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rec, nil, nil
	}
	if err != nil {
		return rec, nil, err
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return rec, nil, fmt.Errorf("fs: invalid restic recovery record %s: %w", path, err)
	}
	return rec, raw, nil
}

// writeRecovery stores rec at path unless old already holds it.
func writeRecovery(path string, rec resticRepository, old []byte) ([]byte, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return old, err
	}
	if bytes.Equal(raw, old) {
		return old, nil
	}
	return raw, config.WriteFileAtomic(path, raw, filePerm)
}

// prepareRestic makes sure the repository of the bucket with ID id exists,
// initializing a new one, and that its recovery record is current. A
// repository found ready is not checked again by this process (unless the
// backing's settings change); buckets are prepared independently.
func (b *Backend) prepareRestic(ctx context.Context, id string) error {
	rb := b.restic.bucket(id)
	rb.mu.Lock()
	defer rb.mu.Unlock()
	b.restic.locationMu.RLock()
	defer b.restic.locationMu.RUnlock()
	gen := b.restic.generation.Load()
	cfg, err := b.resticSettings(id)
	if err != nil {
		return err
	}
	if b.restic.isPrepared(rb, cfg.Repository) {
		return nil
	}
	// Retain recovery credentials independently of the bucket's metadata:
	// deleting a bucket must not make its retained archive unrecoverable.
	// The record is written before the repository is created, so that an
	// initialized repository never lacks one. Only an initialized record
	// fixes the backing's location (checkResticLocation).
	path := b.recoveryPath(id)
	saved, raw, err := readRecovery(path)
	if err != nil {
		return err
	}
	cfg.Initialized = saved.Initialized
	if raw, err = writeRecovery(path, cfg, raw); err != nil {
		return err
	}
	cmd, err := resticCommandFor(ctx, cfg, "cat", "config")
	if err != nil {
		return err
	}
	var diag stderrTail
	cmd.Stderr = &diag
	err = cmd.Run()
	if err != nil {
		// Only restic's documented repository-not-found exit code permits
		// init. Wrong passwords, network and access errors must never
		// initialize anew.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 10 {
			return b.resticFailure(ctx, "open repository", err, &diag, cfg)
		}
		if cfg.Initialized {
			return b.resticFailure(ctx, "open repository", errors.New("initialized repository is missing; restore the repository from backup"), nil, cfg)
		}
		if cmd, err = resticCommandFor(ctx, cfg, "init"); err != nil {
			return err
		}
		diag = stderrTail{}
		cmd.Stderr = &diag
		if err := cmd.Run(); err != nil {
			return b.resticFailure(ctx, "initialize repository", err, &diag, cfg)
		}
	}
	cfg.Initialized = true
	if _, err := writeRecovery(path, cfg, raw); err != nil {
		return err
	}
	rb.prepared, rb.gen = cfg.Repository, gen
	return nil
}

// ValidateEncryptionSettings checks that the bucket can use cfg's provider.
// For restic it checks (without initializing anything) that the bucket's
// repository is either reachable with its password or does not exist yet;
// it creates the bucket's master key, from which that password is derived.
func (b *Backend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	if cfg.Provider != storage.EncryptionRestic {
		return b.PrepareEncryption(ctx, bucket, cfg.Provider)
	}
	id, err := b.encryptionBucketID(ctx, bucket)
	if err != nil {
		return err
	}
	if _, err = b.bucketKey(id, true); err != nil {
		return err
	}
	repo, err := b.resticSettings(id)
	if err != nil {
		return err
	}
	rb := b.restic.bucket(id)
	rb.mu.Lock()
	prepared := b.restic.isPrepared(rb, repo.Repository)
	rb.mu.Unlock()
	if prepared {
		return nil
	}
	saved, _, err := readRecovery(b.recoveryPath(id))
	if err != nil {
		return err
	}
	repo.Initialized = saved.Initialized
	return b.validateResticRepository(ctx, repo)
}

// validateResticRepository checks that the repository cfg can be opened, or
// does not exist yet when it was never initialized.
func (b *Backend) validateResticRepository(ctx context.Context, cfg resticRepository) error {
	cmd, err := resticCommandFor(ctx, cfg, "cat", "config")
	if err != nil {
		return err
	}
	var diag stderrTail
	cmd.Stderr = &diag
	err = cmd.Run()
	var exit *exec.ExitError
	if err == nil || (errors.As(err, &exit) && exit.ExitCode() == 10 && !cfg.Initialized) {
		return nil
	}
	return b.resticFailure(ctx, "open repository", err, &diag, cfg)
}
