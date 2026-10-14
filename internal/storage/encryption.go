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

func (s *ResticS3) Validate() error {
	if s == nil {
		return nil
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("restic S3 endpoint must be http(s)://host without credentials, path, query, or fragment")
	}
	if len(s.Endpoint) > 2048 || len(s.Bucket) < 3 || len(s.Bucket) > 63 || strings.ContainsAny(s.Bucket, "/\\?# \t\r\n") || len(s.Prefix) > 1024 || len(s.Region) > 128 || len(s.AccessKey) > 1024 || len(s.SecretKey) > 4096 || len(s.SessionToken) > 16384 {
		return errors.New("invalid restic S3 bucket or setting length")
	}
	for _, part := range strings.Split(s.Prefix, "/") {
		if part == "." || part == ".." {
			return errors.New("restic S3 prefix cannot contain dot segments")
		}
	}
	if s.AccessKey == "" || s.SecretKey == "" {
		return errors.New("restic S3 access key and secret key are required")
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
	var doc struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("invalid encryption provider: %w", err)
	}
	if doc.Provider != EncryptionAES256 && doc.Provider != EncryptionRestic {
		return "", errors.New("invalid encryption provider")
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
