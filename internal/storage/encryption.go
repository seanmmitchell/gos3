package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/minio/sio"
)

const (
	EncryptionAES256         = "AES256"
	EncryptionRestic         = "restic"
	ResticUserAgent          = "gos3-restic"
	ConfigEncryptionProvider = "encryption-provider"
)

// Repository credentials are internal bucket state, replicated only between
// trusted storage backings. Admin responses must never serialize this type.
type EncryptionSettings struct {
	Provider     string `json:"provider"`
	Repository   string `json:"repository,omitempty"`
	AccessKey    string `json:"accessKey,omitempty"`
	SecretKey    string `json:"secretKey,omitempty"`
	SessionToken string `json:"sessionToken,omitempty"`
	Password     string `json:"password,omitempty"`
	Region       string `json:"region,omitempty"`
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
