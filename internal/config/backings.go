package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Backing modes (BackingConfig.Mode).
const (
	// BackingModeBlob stores object data as opaque blobs in a remote
	// bucket; buckets, versions and all metadata stay in the fs backend.
	BackingModeBlob = "blob"
	// BackingModeGateway stores objects natively in the remote service
	// (pass-through); only bucket configuration stays local.
	BackingModeGateway = "gateway"
)

// BackingLocal is the reserved backing name of the server's own storage.
const BackingLocal = "local"

// Limits of BackingConfig.PartSize.
const (
	MinBackingPartSize = 5 << 20
	MaxBackingPartSize = 5 << 30
)

// MaxBackings bounds the number of configured backings.
const MaxBackings = 64

var backingNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// PathStyle reports whether requests to the backing use path-style
// addressing (see BackingConfig.Addressing).
func (b BackingConfig) PathStyle() bool {
	switch b.Addressing {
	case "path":
		return true
	case "virtual":
		return false
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return host != "amazonaws.com" && !strings.HasSuffix(host, ".amazonaws.com")
}

// Backing returns the configured backing with the given name.
func (c StorageConfig) Backing(name string) (BackingConfig, bool) {
	for _, b := range c.Backings {
		if b.Name == name {
			return b, true
		}
	}
	return BackingConfig{}, false
}

// BackingCredentialEnv returns the names of the environment variables that
// supply the credentials of a backing whose access_key / secret_key are not
// configured: GOS3_BACKING_<NAME>_ACCESS_KEY and _SECRET_KEY.
func BackingCredentialEnv(name string) (accessKey, secretKey string) {
	n := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return "GOS3_BACKING_" + n + "_ACCESS_KEY", "GOS3_BACKING_" + n + "_SECRET_KEY"
}

// applyBackingCredentials fills empty backing credentials from the
// environment (see BackingCredentialEnv).
func (c *Config) applyBackingCredentials(lookup func(string) (string, bool)) {
	for i := range c.Storage.Backings {
		b := &c.Storage.Backings[i]
		ak, sk := BackingCredentialEnv(strings.ToLower(strings.TrimSpace(b.Name)))
		if v, ok := lookup(ak); ok && b.AccessKey == "" {
			b.AccessKey = strings.TrimSpace(v)
		}
		if v, ok := lookup(sk); ok && b.SecretKey == "" {
			b.SecretKey = strings.TrimSpace(v)
		}
	}
}

// normalizeBackings lower-cases names and enumerations and trims values.
func (c *Config) normalizeBackings() {
	for i := range c.Storage.Backings {
		b := &c.Storage.Backings[i]
		b.Name = strings.ToLower(strings.TrimSpace(b.Name))
		b.Mode = strings.ToLower(strings.TrimSpace(b.Mode))
		b.Endpoint = strings.TrimRight(strings.TrimSpace(b.Endpoint), "/")
		b.Region = strings.TrimSpace(b.Region)
		b.Bucket = strings.TrimSpace(b.Bucket)
		b.Addressing = strings.ToLower(strings.TrimSpace(b.Addressing))
		if b.Addressing == "auto" {
			b.Addressing = ""
		}
	}
	c.Storage.DefaultBacking = strings.ToLower(strings.TrimSpace(c.Storage.DefaultBacking))
	if c.Storage.DefaultBacking == BackingLocal {
		c.Storage.DefaultBacking = ""
	}
}

// validateBackings checks storage.backings and storage.default_backing.
func (c Config) validateBackings() error {
	if len(c.Storage.Backings) > MaxBackings {
		return fmt.Errorf("storage.backings: at most %d entries allowed", MaxBackings)
	}
	seen := make(map[string]bool)
	for i, b := range c.Storage.Backings {
		field := fmt.Sprintf("storage.backings[%d]", i)
		if err := validateBacking(b); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
		if seen[b.Name] {
			return fmt.Errorf("%s.name: %q is used twice", field, b.Name)
		}
		seen[b.Name] = true
		if b.Mode == BackingModeBlob && c.Storage.Backend != BackendFS {
			return fmt.Errorf("%s.mode: blob backings need the fs storage backend (the metadata of their objects is kept there)", field)
		}
	}
	if d := c.Storage.DefaultBacking; d != "" && !seen[d] {
		return fmt.Errorf("storage.default_backing: %q is not a configured backing (expected %q or one of storage.backings)", d, BackingLocal)
	}
	return nil
}

func validateBacking(b BackingConfig) error {
	switch {
	case b.Name == BackingLocal:
		return fmt.Errorf("name: %q is reserved for the server's own storage", BackingLocal)
	case !backingNameRe.MatchString(b.Name):
		return fmt.Errorf("name: %q must be 1-32 lowercase letters, digits or '-', starting with a letter or digit", b.Name)
	}
	switch b.Mode {
	case BackingModeBlob, BackingModeGateway:
	default:
		return fmt.Errorf("mode: %q is invalid (expected %s or %s)", b.Mode, BackingModeBlob, BackingModeGateway)
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("endpoint: %q must be an absolute http:// or https:// URL without a path", b.Endpoint)
	}
	if b.Region != "" && !regionRe.MatchString(b.Region) {
		return fmt.Errorf("region: %q must be lowercase letters, digits and '-'", b.Region)
	}
	if b.Mode == BackingModeBlob && b.Bucket == "" {
		return errors.New("bucket: required in blob mode")
	}
	if b.Bucket != "" && !remoteBucketRe.MatchString(b.Bucket) {
		return fmt.Errorf("bucket: %q is not a valid bucket name", b.Bucket)
	}
	if len(b.Prefix) > 512 || !utf8.ValidString(b.Prefix) || strings.HasPrefix(b.Prefix, "/") {
		return errors.New("prefix: must be valid UTF-8 of at most 512 bytes, not starting with '/'")
	}
	if b.AccessKey == "" || b.SecretKey == "" {
		ak, sk := BackingCredentialEnv(b.Name)
		return fmt.Errorf("access_key and secret_key are required (or set %s and %s)", ak, sk)
	}
	switch b.Addressing {
	case "", "path", "virtual":
	default:
		return fmt.Errorf("addressing: %q is invalid (expected auto, path or virtual)", b.Addressing)
	}
	if b.PartSize != 0 && (b.PartSize < MinBackingPartSize || b.PartSize > MaxBackingPartSize) {
		return fmt.Errorf("part_size: must be between %d (5 MiB) and %d (5 GiB) bytes, got %d", MinBackingPartSize, MaxBackingPartSize, b.PartSize)
	}
	return nil
}

var (
	// remoteBucketRe accepts the bucket names of S3 services (stricter
	// rules are the service's business).
	remoteBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-_]{1,254}$`)
)

// backings is the setter of GOS3_STORAGE_BACKINGS: a JSON array of
// BackingConfig objects (JSON field names as in BackingConfig's json tags,
// e.g. "accessKey"); empty clears the list.
func backings(c *Config, v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		c.Storage.Backings = nil
		return nil
	}
	var list []BackingConfig
	if err := json.Unmarshal([]byte(v), &list); err != nil {
		return fmt.Errorf("invalid JSON array of backings: %w", err)
	}
	c.Storage.Backings = list
	return nil
}
