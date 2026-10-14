package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage/crypt"
)

const (
	// EncryptionAES256 is the native SSE-S3 implementation: object data is
	// stored in the AES-256-GCM stream format of package crypt, under a key
	// derived for each data file (see ObjectMeta.Encryption).
	EncryptionAES256 = "AES256"
	// EncryptionRestic is the SSE-S3 implementation that archives object
	// data in a per-bucket restic repository.
	EncryptionRestic = "restic"
	// ResticUserAgent is the HTTP User-Agent of the restic processes gos3
	// runs; gos3 refuses restic-encrypted writes made with it, so a restic
	// repository stored in a restic bucket of the same server cannot recurse.
	ResticUserAgent = "gos3-restic"
	// ConfigEncryptionProvider is the bucket configuration document (see
	// Backend.GetBucketConfig) holding the bucket's EncryptionSettings.
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

// EncryptionBackend is implemented by backends that can store object data
// encrypted (WriteOptions.Encryption). Implementations must fail closed on
// setup errors: a write that asks for encryption never stores plaintext.
type EncryptionBackend interface {
	// PrepareEncryption makes the bucket ready for writes encrypted with
	// mode (EncryptionAES256 or EncryptionRestic), creating its keys or
	// repository when needed. It returns errors.ErrUnsupported (possibly
	// wrapped) when the backend cannot store mode, and ErrBucketNotFound
	// for a missing bucket.
	PrepareEncryption(ctx context.Context, bucket, mode string) error
	// ValidateEncryptionSettings checks that cfg can be used for bucket
	// before it is stored, with the same errors as PrepareEncryption. It
	// may create the bucket's keys (but does not switch providers).
	ValidateEncryptionSettings(ctx context.Context, bucket string, cfg EncryptionSettings) error
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

// EncryptedSize returns the size of the data stored for n bytes (of
// content, or of its compressed form) encrypted with EncryptionAES256, or
// -1 when n is negative or the size would overflow (see crypt.EncryptedSize).
func EncryptedSize(n int64) int64 { return crypt.EncryptedSize(n) }
