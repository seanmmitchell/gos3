package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

// EncryptionSettings is a bucket's SSE-S3 provider selection. Only the
// provider is bucket state: where a provider keeps its keys or repositories
// is decided by the backing holding the bucket and never replicates with it.
type EncryptionSettings struct {
	Provider string `json:"provider"`
	// Scope says which writes use the provider: EncryptionScopeSSE ("",
	// the default) only writes that request SSE (the bucket's default
	// encryption or an explicit header), EncryptionScopeAll every write
	// (the restic archive toggle: every object version becomes a restic
	// snapshot). EncryptionScopeAll is only valid with EncryptionRestic.
	// Builds before scopes ignore the field (their writes fall back to
	// SSE-only).
	Scope string `json:"scope,omitempty"`
}

// Encryption scopes (EncryptionSettings.Scope).
const (
	EncryptionScopeSSE = "sse"
	EncryptionScopeAll = "all"
)

// ParseEncryptionSettings decodes and validates a stored
// ConfigEncryptionProvider document (provider and scope). ParseEncryptionProvider
// is ParseEncryptionSettings returning only the provider.
func ParseEncryptionSettings(raw []byte) (EncryptionSettings, error) {
	var doc EncryptionSettings
	if err := json.Unmarshal(raw, &doc); err != nil {
		return EncryptionSettings{}, fmt.Errorf("invalid encryption provider: %w", err)
	}
	if !ValidEncryptionProvider(doc.Provider) {
		return EncryptionSettings{}, fmt.Errorf("invalid encryption provider %q", doc.Provider)
	}
	switch doc.Scope {
	case "", EncryptionScopeSSE:
	case EncryptionScopeAll:
		if doc.Provider != EncryptionRestic {
			return EncryptionSettings{}, fmt.Errorf("encryption scope %q needs provider %q", doc.Scope, EncryptionRestic)
		}
	default:
		return EncryptionSettings{}, fmt.Errorf("invalid encryption scope %q", doc.Scope)
	}
	return doc, nil
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
	return ParseEncryptionProvider(raw)
}

// ParseEncryptionProvider decodes a stored ConfigEncryptionProvider
// document and returns its provider. It is the only parser of that
// document: the S3 layer (which caches the result) and the admin API share
// its validation, so a provider that writes would refuse is never shown as
// valid.
func ParseEncryptionProvider(raw []byte) (string, error) {
	doc, err := ParseEncryptionSettings(raw)
	if err != nil {
		return "", err
	}
	return doc.Provider, nil
}

// EncryptedSize returns the size of the data stored for n bytes (of
// content, or of its compressed form) encrypted with EncryptionAES256, or
// -1 when n is negative or the size would overflow (see crypt.EncryptedSize).
func EncryptedSize(n int64) int64 { return crypt.EncryptedSize(n) }
