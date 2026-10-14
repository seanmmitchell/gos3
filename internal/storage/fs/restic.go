package fs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Restic owns immutable snapshots. Keeping snapshots after S3 deletes provides
// an archive and makes open readers safe across concurrent overwrites/deletes.
// Operators must not forget snapshots still referenced by gos3 metadata.
type resticRepository struct {
	Initialized  bool   `json:"initialized,omitempty"`
	Repository   string `json:"repository"`
	Password     string `json:"password"`
	AccessKey    string `json:"-"`
	SecretKey    string `json:"-"`
	SessionToken string `json:"-"`
	Region       string `json:"-"`
}

func (cfg *resticRepository) withS3(s *storage.ResticS3) {
	if s != nil {
		cfg.AccessKey, cfg.SecretKey, cfg.SessionToken, cfg.Region = s.AccessKey, s.SecretKey, s.SessionToken, s.Region
	}
}
func (b *Backend) resticSettings(id string) (resticRepository, error) {
	key, err := b.bucketKey(id, false)
	if err != nil {
		return resticRepository{}, err
	}
	s3 := b.resticBacking()
	cfg := resticRepository{Repository: b.resticRepository(id, s3), Password: resticPassword(key)}
	cfg.withS3(s3)
	return cfg, nil
}

// limitedOutput prevents subprocess diagnostics/progress from exhausting memory.
type limitedOutput struct{ bytes.Buffer }

// Validate without initializing: credentials are checked before replacing a
// working configuration. A new repository's password is persisted before init.
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
	return validateResticRepository(ctx, repo)
}
func validateResticRepository(ctx context.Context, cfg resticRepository) error {
	cmd, err := resticCommandFor(ctx, cfg, "cat", "config")
	if err != nil {
		return err
	}
	var out limitedOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 10 && !cfg.Initialized {
		return nil
	}
	return err
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if w.Len() < 1<<20 {
		_, _ = w.Buffer.Write(p[:min(n, (1<<20)-w.Len())])
	}
	return n, nil
}

func (b *Backend) resticCommand(ctx context.Context, id string, args ...string) (*exec.Cmd, error) {
	cfg, err := b.resticSettings(id)
	if err != nil {
		return nil, err
	}
	return resticCommandFor(ctx, cfg, args...)
}

func resticCommandFor(ctx context.Context, cfg resticRepository, args ...string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		return nil, errors.New("restic executable is unavailable; install restic on the server PATH")
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"--repo", cfg.Repository, "--no-cache", "--retry-lock", "30s", "--http-user-agent", storage.ResticUserAgent}, args...)...)
	hideResticWindow(cmd)
	// Do not inherit repository/password/command or AWS settings from a host
	// that happens to use restic for unrelated backups.
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		name = strings.ToUpper(name)
		if !strings.HasPrefix(name, "RESTIC_") && !strings.HasPrefix(name, "AWS_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "RESTIC_PASSWORD="+cfg.Password, "AWS_ACCESS_KEY_ID="+cfg.AccessKey, "AWS_SECRET_ACCESS_KEY="+cfg.SecretKey,
		"AWS_SESSION_TOKEN="+cfg.SessionToken, "AWS_DEFAULT_REGION="+cfg.Region)
	return cmd, nil
}

func (b *Backend) prepareRestic(ctx context.Context, id string) error {
	b.resticMu.Lock()
	defer b.resticMu.Unlock()
	cfg, err := b.resticSettings(id)
	if err != nil {
		return err
	}
	// Retain recovery credentials independently of the bucket's metadata.
	// Deleting a bucket must not make its retained archive unrecoverable.
	recoveryPath := filepath.Join(b.sysDir, "keys", id+".restic.json")
	if old, readErr := os.ReadFile(recoveryPath); readErr == nil {
		var saved resticRepository
		if err := json.Unmarshal(old, &saved); err != nil {
			return err
		}
		cfg.Initialized = saved.Initialized
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(recoveryPath, raw, filePerm); err != nil {
		return err
	}
	cmd, err := b.resticCommand(ctx, id, "cat", "config")
	if err != nil {
		return err
	}
	var output limitedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Run(); err == nil {
		cfg.Initialized = true
		raw, _ = json.Marshal(cfg)
		return config.WriteFileAtomic(recoveryPath, raw, filePerm)
	}
	// Only restic's documented repository-not-found exit code permits init.
	// Wrong passwords, network and access errors must never initialize anew.
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 10 {
		return fmt.Errorf("restic: cannot open repository: %w", err)
	}
	if cfg.Initialized {
		return errors.New("restic: initialized repository is missing; restore the repository from backup")
	}
	cmd, err = b.resticCommand(ctx, id, "init")
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restic: initialize repository: %w", err)
	}
	cfg.Initialized = true
	raw, _ = json.Marshal(cfg)
	return config.WriteFileAtomic(recoveryPath, raw, filePerm)
}

func (b *Backend) finishRestic(ctx context.Context, t *tmpFile, id, bucket, key string) (string, string, error) {
	if err := b.prepareRestic(ctx, id); err != nil {
		t.discard()
		return "", "", err
	}
	if err := t.finish(false, false); err != nil {
		return "", "", err
	}
	defer t.remove()
	var input io.Reader = bytes.NewReader(nil)
	if t.size > 0 {
		f, err := os.Open(t.path)
		if err != nil {
			return "", "", err
		}
		defer f.Close()
		r, err := decryptFile(f, t.key, t.size, f.Close)
		if err != nil {
			return "", "", err
		}
		input = r
	}
	cmd, err := b.resticCommand(ctx, id, "backup", "--stdin", "--stdin-filename", "object", "--json", "--host", "gos3",
		"--tag", "gos3-object", "--tag", "gos3-bucket:"+base64.RawURLEncoding.EncodeToString([]byte(bucket)),
		"--tag", "gos3-key:"+base64.RawURLEncoding.EncodeToString([]byte(key)))
	if err != nil {
		return "", "", err
	}
	var out snapshotOutput
	var diagnostic limitedOutput
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, &out, &diagnostic
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("restic: archive object: %w", err)
	}
	if err := out.consume(); err != nil {
		return "", "", err
	}
	snapshot := out.snapshot
	if !validSnapshot(snapshot) {
		return "", "", errors.New("restic: backup returned no valid snapshot ID")
	}
	// The local data file is an immutable pointer. No plaintext object is kept.
	ref := b.newTemp()
	if _, err := ref.Write([]byte(snapshot)); err != nil {
		ref.discard()
		return "", "", err
	}
	return b.finishData(ref, id)
}

// Consume JSON progress incrementally: a long upload must not fill a bounded
// diagnostic buffer and lose the final summary containing its snapshot ID.
type snapshotOutput struct {
	pending  []byte
	snapshot string
}

func (w *snapshotOutput) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		line, rest, complete := bytes.Cut(p, []byte{'\n'})
		if len(w.pending)+len(line) > 1<<20 {
			return 0, errors.New("restic: oversized backup progress record")
		}
		w.pending = append(w.pending, line...)
		if !complete {
			break
		}
		if err := w.consume(); err != nil {
			return 0, err
		}
		p = rest
	}
	return n, nil
}

func (w *snapshotOutput) consume() error {
	if len(bytes.TrimSpace(w.pending)) == 0 {
		w.pending = w.pending[:0]
		return nil
	}
	var item struct {
		MessageType string `json:"message_type"`
		SnapshotID  string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(w.pending, &item); err != nil {
		return fmt.Errorf("restic: decode backup result: %w", err)
	}
	if item.MessageType == "summary" {
		w.snapshot = item.SnapshotID
	}
	w.pending = w.pending[:0]
	return nil
}

func validSnapshot(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 32 && id == strings.ToLower(id)
}

func (b *Backend) openRestic(ctx context.Context, f *os.File, id string, size int64) (io.ReadSeekCloser, error) {
	defer f.Close()
	ref, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return nil, err
	}
	if !validSnapshot(string(ref)) {
		return nil, errors.New("restic: invalid snapshot reference")
	}
	cmd, err := b.resticCommand(ctx, id, "dump", string(ref), "/object")
	if err != nil {
		return nil, err
	}
	// Spool authenticated output to an encrypted temporary file. This bounds
	// memory and supports S3 ranges/seeks without leaving plaintext on disk.
	t := b.newTemp()
	t.key = make([]byte, 32)
	_, _ = rand.Read(t.key)
	defer func() {
		if err != nil {
			t.discard()
		}
	}()
	var diagnostic limitedOutput
	cmd.Stdout, cmd.Stderr = &boundedObjectWriter{dst: t, remaining: size}, &diagnostic
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("restic: retrieve object: %w", err)
	}
	if t.size != size {
		err = errors.New("restic: restored object size mismatch")
		return nil, err
	}
	if err = t.finish(false, true); err != nil {
		return nil, err
	}
	cache, err := os.Open(t.path)
	if err != nil {
		return nil, err
	}
	r, err := decryptFile(cache, t.key, size, func() error { e := cache.Close(); t.remove(); return e })
	if err != nil {
		_ = cache.Close()
		return nil, err
	}
	return r, nil
}

type boundedObjectWriter struct {
	dst       io.Writer
	remaining int64
}

func (w *boundedObjectWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("restic: restored object exceeds expected size")
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}
