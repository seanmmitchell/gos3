package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/seanmmitchell/gos3/internal/iam"
)

// LoadStore returns a Store persisting to path, starting from initial with a
// previously persisted settings.json (if any) laid over it: fields present in
// the file win, fields missing from it (e.g. added by a newer version) keep
// their initial value. An unreadable or invalid file is an error (the server
// should not silently drop an operator's settings). path "" = memory only.
func LoadStore(initial Settings, path string) (*Store, error) {
	s := cloneSettings(initial)
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("config: reading settings: %w", err)
		default:
			dec := json.NewDecoder(bytes.NewReader(data))
			if err := dec.Decode(&s); err != nil {
				return nil, fmt.Errorf("config: %s: %w", path, err)
			}
			s = normalizeSettings(s)
			if err := ValidateSettings(s); err != nil {
				return nil, fmt.Errorf("config: %s: %w (fix or delete the file)", path, err)
			}
		}
	}
	return NewStore(normalizeSettings(s), path), nil
}

// writeSettingsFile atomically writes s as JSON to path (mode 0600).
func writeSettingsFile(path string, s Settings) error {
	data, err := json.MarshalIndent(normalizeSettings(s), "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding settings: %w", err)
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// WriteFileAtomic writes data to path via a temporary file in the same
// directory: write, fsync, chmod perm, rename, then fsync of the directory.
// The directory is created (0700) when missing. Readers never observe a
// partially written file, and a failure leaves any previous file intact.
// Errors wrap the underlying *fs.PathError.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename is durable (best effort; not
// supported on Windows).
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// RootCredentialsFileName is the file (in SystemDir) holding generated root
// credentials.
const RootCredentialsFileName = "root-credentials.json"

// Sources of the root credentials reported by ResolveRootCredentials.
const (
	// CredentialsConfigured: set in the config file or environment.
	CredentialsConfigured = "configured"
	// CredentialsStored: generated on an earlier start and read back from
	// <data>/.gos3/root-credentials.json.
	CredentialsStored = "stored"
	// CredentialsGenerated: generated now and stored for later starts.
	CredentialsGenerated = "generated"
	// CredentialsEphemeral: generated now and not stored (no data path).
	CredentialsEphemeral = "ephemeral"
)

type rootCredentialsFile struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// ResolveRootCredentials makes sure c has root credentials. Configured keys
// are kept. Otherwise keys stored in <SystemDir>/root-credentials.json by an
// earlier start are reused, or new random ones are generated and stored
// there (0600) — or, without a data path, generated for this process only.
// It returns the source (Credentials* constants) and the file path ("" when
// not stored).
func ResolveRootCredentials(c *Config) (source, file string, err error) {
	if c.Auth.RootAccessKey != "" || c.Auth.RootSecretKey != "" {
		if err := validateRootCredentials(c.Auth.RootAccessKey, c.Auth.RootSecretKey); err != nil {
			return "", "", err
		}
		return CredentialsConfigured, "", nil
	}
	dir := c.SystemDir()
	if dir == "" {
		ak, sk, err := generateRootCredentials()
		if err != nil {
			return "", "", err
		}
		c.Auth.RootAccessKey, c.Auth.RootSecretKey = ak, sk
		return CredentialsEphemeral, "", nil
	}
	file = filepath.Join(dir, RootCredentialsFileName)
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		var rc rootCredentialsFile
		if err := json.Unmarshal(data, &rc); err != nil {
			return "", "", fmt.Errorf("config: %s: %w", file, err)
		}
		if err := validateRootCredentials(rc.AccessKey, rc.SecretKey); err != nil || rc.AccessKey == "" {
			return "", "", fmt.Errorf("config: %s holds invalid credentials (delete it to generate new ones)", file)
		}
		c.Auth.RootAccessKey, c.Auth.RootSecretKey = rc.AccessKey, rc.SecretKey
		return CredentialsStored, file, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", "", fmt.Errorf("config: reading root credentials: %w", err)
	}
	ak, sk, err := generateRootCredentials()
	if err != nil {
		return "", "", err
	}
	rc := rootCredentialsFile{AccessKey: ak, SecretKey: sk}
	data, err = json.MarshalIndent(rc, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := WriteFileAtomic(file, append(data, '\n'), 0o600); err != nil {
		return "", "", fmt.Errorf("storing generated root credentials: %w", err)
	}
	c.Auth.RootAccessKey, c.Auth.RootSecretKey = rc.AccessKey, rc.SecretKey
	return CredentialsGenerated, file, nil
}

// generateRootCredentials returns a random access/secret key pair (the same
// generators as IAM user keys).
func generateRootCredentials() (ak, sk string, err error) {
	if ak, err = iam.GenerateAccessKey(); err != nil {
		return "", "", err
	}
	if sk, err = iam.GenerateSecretKey(); err != nil {
		return "", "", err
	}
	return ak, sk, nil
}
