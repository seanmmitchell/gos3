package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// S3BackingConfig is the remote service of an s3 backing
// (BackingConfig.S3, see docs/backings.md).
type S3BackingConfig struct {
	// Mode is S3ModeBlob (object data stored as opaque blobs, all metadata
	// in the backing's local path; every gos3 feature works) or
	// S3ModeGateway (objects stored natively, readable by other S3
	// clients; no versioning or object lock).
	Mode string `yaml:"mode" json:"mode"`
	// Endpoint is the service URL, http(s)://host[:port] (no path).
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// Region is the signing region (default us-east-1).
	Region string `yaml:"region" json:"region"`
	// Bucket is the remote bucket. Required in blob mode. In gateway mode
	// it is optional: when set, every gos3 bucket is a key prefix
	// "<prefix><bucket>/" in it; when empty, every gos3 bucket maps to the
	// remote bucket of the same name (created when missing).
	Bucket string `yaml:"bucket" json:"bucket"`
	// Prefix is prepended to every remote key (e.g. "gos3/").
	Prefix string `yaml:"prefix" json:"prefix"`
	// AccessKey / SecretKey authenticate to the service. When empty they
	// are read from GOS3_BACKING_<NAME>_ACCESS_KEY / _SECRET_KEY (see
	// BackingCredentialEnv).
	AccessKey string `yaml:"access_key" json:"accessKey"`
	SecretKey string `yaml:"secret_key" json:"secretKey"`
	// Addressing is "path" (<endpoint>/<bucket>/<key>), "virtual"
	// (<bucket>.<host>/<key>) or "auto"/"" (virtual for amazonaws.com
	// endpoints, path otherwise).
	Addressing string `yaml:"addressing" json:"addressing"`
	// PartSize is the multipart part size for uploads to the service and
	// the largest object sent with one PUT (default 16 MiB, 5 MiB-5 GiB).
	PartSize int64 `yaml:"part_size" json:"partSize"`
}

// S3 backing modes (S3BackingConfig.Mode).
const (
	S3ModeBlob    = "blob"
	S3ModeGateway = "gateway"
)

// Limits of S3BackingConfig.PartSize.
const (
	MinBackingPartSize = 5 << 20
	MaxBackingPartSize = 5 << 30
)

// remoteBucketRe accepts the bucket names of S3 services (stricter rules
// are the service's business).
var remoteBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-_]{1,254}$`)

// PathStyle reports whether requests to the service use path-style
// addressing (see Addressing).
func (b S3BackingConfig) PathStyle() bool {
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

// BackingCredentialEnv returns the names of the environment variables that
// supply the credentials of an s3 backing whose access_key / secret_key
// are not configured: GOS3_BACKING_<NAME>_ACCESS_KEY and _SECRET_KEY (NAME
// upper-cased, '-' as '_').
func BackingCredentialEnv(name string) (accessKey, secretKey string) {
	n := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return "GOS3_BACKING_" + n + "_ACCESS_KEY", "GOS3_BACKING_" + n + "_SECRET_KEY"
}

// applyBackingCredentials fills empty s3 backing credentials from the
// environment (see BackingCredentialEnv).
func (c *Config) applyBackingCredentials(lookup func(string) (string, bool)) {
	for i := range c.Storage.Backings {
		b := &c.Storage.Backings[i]
		if b.S3 == nil {
			continue
		}
		ak, sk := BackingCredentialEnv(strings.ToLower(strings.TrimSpace(b.Name)))
		if v, ok := lookup(ak); ok && b.S3.AccessKey == "" {
			b.S3.AccessKey = strings.TrimSpace(v)
		}
		if v, ok := lookup(sk); ok && b.S3.SecretKey == "" {
			b.S3.SecretKey = strings.TrimSpace(v)
		}
	}
}

// normalizeS3Backings lower-cases enumerations and trims the values of s3
// backings.
func (c *Config) normalizeS3Backings() {
	for i := range c.Storage.Backings {
		b := c.Storage.Backings[i].S3
		if b == nil {
			continue
		}
		b.Mode = strings.ToLower(strings.TrimSpace(b.Mode))
		b.Endpoint = strings.TrimRight(strings.TrimSpace(b.Endpoint), "/")
		b.Region = strings.TrimSpace(b.Region)
		b.Bucket = strings.TrimSpace(b.Bucket)
		b.Addressing = strings.ToLower(strings.TrimSpace(b.Addressing))
		if b.Addressing == "auto" {
			b.Addressing = ""
		}
	}
}

// validateS3Backing checks the s3 settings of backing name. Errors start
// with the offending field name.
func validateS3Backing(name string, b S3BackingConfig) error {
	switch b.Mode {
	case S3ModeBlob, S3ModeGateway:
	default:
		return fmt.Errorf("mode: %q is invalid (expected %s or %s)", b.Mode, S3ModeBlob, S3ModeGateway)
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("endpoint: %q must be an absolute http:// or https:// URL without a path", b.Endpoint)
	}
	if b.Region != "" && !regionRe.MatchString(b.Region) {
		return fmt.Errorf("region: %q must be lowercase letters, digits and '-'", b.Region)
	}
	if b.Mode == S3ModeBlob && b.Bucket == "" {
		return errors.New("bucket: required in blob mode")
	}
	if b.Bucket != "" && !remoteBucketRe.MatchString(b.Bucket) {
		return fmt.Errorf("bucket: %q is not a valid bucket name", b.Bucket)
	}
	if len(b.Prefix) > 512 || !utf8.ValidString(b.Prefix) || strings.HasPrefix(b.Prefix, "/") {
		return errors.New("prefix: must be valid UTF-8 of at most 512 bytes, not starting with '/'")
	}
	if b.AccessKey == "" || b.SecretKey == "" {
		ak, sk := BackingCredentialEnv(name)
		return fmt.Errorf("access_key: access_key and secret_key are required (or set %s and %s)", ak, sk)
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
