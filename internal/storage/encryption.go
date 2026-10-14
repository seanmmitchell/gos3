package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/minio/sio"
)

const (
	EncryptionAES256         = "AES256"
	EncryptionRestic         = "restic"
	ResticUserAgent          = "gos3-restic"
	ConfigEncryptionProvider = "encryption-provider"
)

// Only provider selection is bucket state. Repository locations and S3
// credentials belong to the backing and must never replicate with a bucket.
type EncryptionSettings struct {
	Provider string `json:"provider"`
}

// ResticS3 defines the S3 storage of one backing's restic repositories.
// A nil setting uses the backing's local filesystem. Secrets are write-only
// in admin responses; passwords are derived from each backing's bucket keys.
type ResticS3 struct {
	Endpoint     string `json:"endpoint" yaml:"endpoint"`
	Bucket       string `json:"bucket" yaml:"bucket"`
	Prefix       string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Region       string `json:"region,omitempty" yaml:"region,omitempty"`
	AccessKey    string `json:"accessKey,omitempty" yaml:"access_key,omitempty"`
	SecretKey    string `json:"secretKey,omitempty" yaml:"secret_key,omitempty"`
	SessionToken string `json:"sessionToken,omitempty" yaml:"session_token,omitempty"`
}

type ResticBackingBackend interface {
	ResticBackingSettings(context.Context) (*ResticS3, bool, error)
	ConfigureResticBacking(context.Context, *ResticS3) error
}

// Validate checks the settings before they are stored or used. A nil
// setting (local repositories) is valid. The endpoint gets the checks of
// config.ValidateBaseURL (which storage cannot import, as config imports
// storage): an absolute http(s) URL with a valid host and port and no
// credentials, path, query or fragment. The bucket must be an S3 bucket
// name, the prefix must not contain dot segments or control characters,
// a region must have config.ValidRegion's syntax, and both static keys are
// required. Errors name the offending field.
func (s *ResticS3) Validate() error {
	if s == nil {
		return nil
	}
	if len(s.Endpoint) > maxResticEndpointLen {
		return fmt.Errorf("restic S3 endpoint: longer than %d bytes", maxResticEndpointLen)
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return fmt.Errorf("restic S3 endpoint: %q is not a valid URL", s.Endpoint)
	}
	if err := validResticEndpoint(u); err != nil {
		return fmt.Errorf("restic S3 endpoint: %q %w", s.Endpoint, err)
	}
	if !validResticBucket(s.Bucket) {
		return fmt.Errorf("restic S3 bucket: %q must be 3-63 characters of letters, digits, '.', '-' and '_', starting and ending with a letter or digit", s.Bucket)
	}
	if len(s.Prefix) > 1024 || hasControl(s.Prefix) {
		return errors.New("restic S3 prefix: at most 1024 bytes without control characters")
	}
	for _, part := range strings.Split(s.Prefix, "/") {
		if part == "." || part == ".." {
			return errors.New("restic S3 prefix: cannot contain dot segments")
		}
	}
	if s.Region != "" && !validResticRegion(s.Region) {
		return fmt.Errorf("restic S3 region: %q is not a valid region name", s.Region)
	}
	if s.AccessKey == "" || s.SecretKey == "" {
		return errors.New("restic S3 access key and secret key are required")
	}
	if len(s.AccessKey) > 1024 || len(s.SecretKey) > 4096 || len(s.SessionToken) > 16384 {
		return errors.New("restic S3 credentials are too long")
	}
	return nil
}

// EncryptionBackend verifies that encrypted writes are supported and prepares
// their keys/repository. Implementations must fail closed on setup errors.
type EncryptionBackend interface {
	PrepareEncryption(context.Context, string, string) error
	ValidateEncryptionSettings(context.Context, string, EncryptionSettings) error
}

// EncryptionProvider selects the implementation of SSE-S3 for a bucket.
// An absent setting uses native AES-256-GCM. Enabling SSE remains the job of
// the standard S3 bucket encryption configuration (or an explicit PUT header).
func EncryptionProvider(ctx context.Context, b Backend, bucket string) (string, error) {
	raw, err := b.GetBucketConfig(ctx, bucket, ConfigEncryptionProvider)
	if errors.Is(err, ErrConfigNotFound) {
		return EncryptionAES256, nil
	}
	if err != nil {
		return "", err
	}
	return ParseEncryptionProvider(raw)
}

// ParseEncryptionProvider decodes a stored ConfigEncryptionProvider
// document and returns its provider. It is the only parser of that
// document: the S3 layer (which caches the result) and the admin API share
// its validation, so a provider that writes would refuse is never shown as
// valid.
func ParseEncryptionProvider(raw []byte) (string, error) {
	var doc EncryptionSettings
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("invalid encryption provider: %w", err)
	}
	if !ValidEncryptionProvider(doc.Provider) {
		return "", fmt.Errorf("invalid encryption provider %q", doc.Provider)
	}
	return doc.Provider, nil
}

// EncryptionConfig pins the authenticated streaming format and cipher suite.
func EncryptionConfig(key []byte) sio.Config {
	return sio.Config{Key: key, MinVersion: sio.Version20, MaxVersion: sio.Version20, CipherSuites: []byte{sio.AES_256_GCM}}
}

func EncryptedSize(n int64) int64 {
	size, err := sio.EncryptedSize(uint64(n))
	if err != nil {
		return -1
	}
	return int64(size)
}
