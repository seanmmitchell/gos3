package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/seanmmitchell/gos3/internal/iam"
)

// LoadStore returns a Store persisting to path, starting from initial with a
// previously persisted settings.json (if any) laid over it: fields present in
// the file win, fields missing from it (e.g. added by a newer version) keep
// their initial value. The result must pass ValidateSettings. An unreadable,
// corrupt or invalid file is an error naming the file (the server should not
// silently drop an operator's settings). path "" = memory only.
func LoadStore(initial Settings, path string) (*Store, error) {
	s := cloneSettings(initial)
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("config: reading settings: %w", err)
		default:
			if err := decodeSettings(data, &s); err != nil {
				return nil, fmt.Errorf("config: settings file %s is corrupt: %v (fix or delete it)", path, err)
			}
			if err := ValidateSettings(normalizeSettings(s)); err != nil {
				return nil, fmt.Errorf("config: settings file %s is invalid: %w (fix or delete it)", path, err)
			}
		}
	}
	s = normalizeSettings(s)
	if err := ValidateSettings(s); err != nil {
		return nil, fmt.Errorf("config: invalid settings: %w", err)
	}
	return NewStore(s, path), nil
}

// decodeSettings decodes one JSON object over s, rejecting trailing data.
// Unknown fields are ignored so that a file written by a newer version can
// still be read.
func decodeSettings(data []byte, s *Settings) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("file is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(s); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
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
// On Windows, where modes do not restrict access, a private perm (no group
// or other bits, e.g. 0600) gives the file a DACL that only grants the
// current user, SYSTEM and Administrators access (see RestrictDir).
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
	if perm&0o077 == 0 {
		// Best effort, like RestrictDir: file systems without ACLs (FAT,
		// exFAT, some SMB shares) or a missing privilege must not make
		// settings, IAM or credentials unwritable. The file then keeps the
		// directory's ACL.
		if aerr := restrictFile(tmp, false); aerr != nil {
			slog.Default().Warn("could not restrict access to a secret file to this user; "+
				"make sure other local users cannot read its directory", "file", path, "err", aerr)
		}
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

// restrictFile is restrictPath (a variable for tests).
var restrictFile = restrictPath

// RestrictDir makes dir private to the user running gos3. On Windows,
// where file modes do not restrict access, it replaces the directory's ACL
// with one granting only the current user, SYSTEM and Administrators
// access, inherited by everything inside (existing files included); the
// system directory holds root-credentials.json, iam.json (IAM secret keys)
// and settings.json. Elsewhere it does nothing (the directory is created
// with mode 0700 and secret files with 0600).
func RestrictDir(dir string) error { return restrictPath(dir, true) }

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

// RootCredentialsFileName is the file (in the system directory) holding
// generated root credentials.
const RootCredentialsFileName = "root-credentials.json"

// Sources of the root credentials (see ResolveRootCredentials).
const (
	// CredentialsConfigured: set in the config file, environment or flags.
	CredentialsConfigured = "configured"
	// CredentialsStored: generated on an earlier start and read back from
	// <systemDir>/root-credentials.json.
	CredentialsStored = "stored"
	// CredentialsGenerated: generated now and stored for later starts.
	CredentialsGenerated = "generated"
	// CredentialsEphemeral: generated now and not stored (no system dir).
	CredentialsEphemeral = "ephemeral"
)

type rootCredentialsFile struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// ResolveRootCredentials makes sure cfg has root credentials and logs where
// they come from. systemDir is the system state directory (normally
// cfg.SystemDir(); "" = nothing is persisted).
//
//   - Configured keys are validated and kept. A secret shorter than
//     MinRootSecretLen characters, or a published example secret allowed by
//     auth.insecure_demo_credentials, is warned about.
//   - Otherwise keys stored in <systemDir>/root-credentials.json by an
//     earlier start are reused (the access key is logged at info level).
//   - Otherwise random keys are generated (iam.GenerateAccessKey /
//     GenerateSecretKey) and stored there with mode 0600; a warning names
//     the access key and the file.
//   - With systemDir "" (memory backend without a data path) random keys are
//     generated on every start, with a warning naming the access key.
//
// The secret key is never logged: logs are collected and kept by systems
// (journald, docker, Kubernetes) that many more people can read than the
// credentials file. The CLI shows a generated secret in its startup banner
// when that is printed to a terminal. A nil logger means slog.Default().
func ResolveRootCredentials(cfg *Config, systemDir string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	source, file, err := resolveRootCredentials(cfg, systemDir)
	if err != nil {
		return err
	}
	ak := cfg.Auth.RootAccessKey
	switch source {
	case CredentialsConfigured:
		logger.Info("root credentials: using the configured access key", "accessKey", ak)
		sk := cfg.Auth.RootSecretKey
		switch {
		case IsDemoSecret(sk):
			logger.Warn("root credentials: the root secret key is a published example (auth.insecure_demo_credentials is on); " +
				"anyone who can reach this server, including websites opened in a browser on this machine, can act as root. " +
				"Use this only for a throwaway local demo")
		case len(sk) < MinRootSecretLen:
			logger.Warn(fmt.Sprintf("root credentials: the root secret key is shorter than %d characters; a short secret can be "+
				"guessed offline from a single captured signed request (use \"gos3 keygen\" for a random one)", MinRootSecretLen))
		}
	case CredentialsStored:
		logger.Info("root credentials: using the stored generated credentials", "accessKey", ak, "file", file)
	case CredentialsGenerated:
		logger.Warn("root credentials: none configured, generated new ones and stored them for later starts; "+
			"the secret key is not logged: read it from the file (set GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY to choose your own)",
			"accessKey", ak, "file", file)
	case CredentialsEphemeral:
		logger.Warn("root credentials: none configured and no data directory, generated temporary ones "+
			"valid until this process exits; the secret key is not logged "+
			"(set GOS3_ROOT_ACCESS_KEY / GOS3_ROOT_SECRET_KEY to choose your own)",
			"accessKey", ak)
	}
	return nil
}

// resolveRootCredentials implements ResolveRootCredentials without logging.
// It returns the source (Credentials* constants) and the credentials file
// ("" when not stored).
func resolveRootCredentials(c *Config, systemDir string) (source, file string, err error) {
	if c.Auth.RootAccessKey != "" || c.Auth.RootSecretKey != "" {
		if err := validateRootCredentials(c.Auth.RootAccessKey, c.Auth.RootSecretKey); err != nil {
			return "", "", err
		}
		return CredentialsConfigured, "", nil
	}
	if systemDir == "" {
		ak, sk, err := generateRootCredentials()
		if err != nil {
			return "", "", err
		}
		c.Auth.RootAccessKey, c.Auth.RootSecretKey = ak, sk
		return CredentialsEphemeral, "", nil
	}
	file = filepath.Join(systemDir, RootCredentialsFileName)
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		var rc rootCredentialsFile
		if err := json.Unmarshal(data, &rc); err != nil {
			return "", "", fmt.Errorf("config: root credentials file %s is corrupt: %v (delete it to generate new credentials)", file, err)
		}
		if rc.AccessKey == "" || validateRootCredentials(rc.AccessKey, rc.SecretKey) != nil {
			return "", "", fmt.Errorf("config: root credentials file %s holds invalid credentials (delete it to generate new ones)", file)
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
	data, err = json.MarshalIndent(rootCredentialsFile{AccessKey: ak, SecretKey: sk}, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := WriteFileAtomic(file, append(data, '\n'), 0o600); err != nil {
		return "", "", fmt.Errorf("storing generated root credentials: %w", err)
	}
	c.Auth.RootAccessKey, c.Auth.RootSecretKey = ak, sk
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
