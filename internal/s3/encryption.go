package s3

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// cacheKindEncryptionSettings is the parsed-config cache kind of the
// bucket's storage.ConfigEncryptionProvider document.
const cacheKindEncryptionSettings = "s3/encryption-settings"

// prepareEncryption decides how a new object with metadata meta is stored
// and readies the backend for it:
//
//   - with SSE, meta.Encryption becomes the bucket's SSE-S3 implementation
//     (the provider of its encryption settings) unless already set (a
//     multipart upload keeps the provider it was created with);
//   - without SSE, the object is stored unencrypted (meta.Encryption ""),
//     unless the bucket's settings archive every write through restic
//     (provider restic with scope storage.EncryptionScopeAll): then
//     meta.Encryption becomes restic while meta.SSE stays empty, so
//     responses report no server-side encryption. A multipart upload
//     created without SSE is decided again when it completes, so the
//     setting at completion applies.
//
// When meta.Encryption is set, the backend prepares the bucket's keys or
// repository (storage.EncryptionBackend).
//
// meta.SSE is AES256 or, only for multipart uploads created by builds that
// accepted AWS KMS, a KMS algorithm: explicit KMS headers and KMS bucket
// defaults never get here (sseFromHeaders, defaultEncryption). Such uploads
// complete as SSE-S3 like new objects of their bucket, instead of failing.
//
// userAgent is the request's User-Agent (the completing request's for a
// multipart upload; "" when there is no request of the writing client, as
// for bucket configuration): a
// restic process's own writes are refused in buckets that would archive
// them through restic, so a repository cannot be stored in a bucket that
// archives into it.
func (s *Server) prepareEncryption(ctx context.Context, bucket string, meta *storage.ObjectMeta, userAgent string) error {
	if meta.SSE == "" {
		meta.Encryption = ""
		set, err := s.encryptionSettings(ctx, bucket)
		if err != nil {
			return err
		}
		if set.Provider != storage.EncryptionRestic || set.Scope != storage.EncryptionScopeAll {
			return nil
		}
		return s.prepareProvider(ctx, bucket, meta, storage.EncryptionRestic, userAgent)
	}
	if isKMS(meta.SSE) {
		s.log.LogAttrs(ctx, slog.LevelWarn, "s3: completing a multipart upload that requested AWS KMS encryption with SSE-S3 (AES256); KMS is not implemented",
			slog.String("bucket", bucket), slog.String("algorithm", meta.SSE))
		meta.SSE, meta.SSEKMSKeyID = "AES256", ""
	}
	if meta.SSE != "AES256" {
		return errKMSNotImplemented()
	}
	mode := meta.Encryption
	if mode == "" {
		set, err := s.encryptionSettings(ctx, bucket)
		if err != nil {
			return err
		}
		mode = set.Provider
	}
	return s.prepareProvider(ctx, bucket, meta, mode, userAgent)
}

// prepareProvider readies the backend for a new object of bucket stored
// with the provider mode and records it in meta.Encryption (see
// prepareEncryption).
func (s *Server) prepareProvider(ctx context.Context, bucket string, meta *storage.ObjectMeta, mode, userAgent string) error {
	if mode == storage.EncryptionRestic && strings.Contains(userAgent, storage.ResticUserAgent) {
		return s3err.ErrInvalidRequest.WithMessage("A restic repository cannot be stored in a bucket that archives its writes through restic.")
	}
	e, ok := s.backend.(storage.EncryptionBackend)
	if !ok {
		return s3err.ErrNotImplemented.WithMessage("This storage backend does not support encryption.")
	}
	if err := e.PrepareEncryption(ctx, bucket, mode); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			return s3err.ErrNotImplemented.WithMessage("This storage backend does not support the selected encryption provider.")
		}
		return err
	}
	meta.Encryption = mode
	return nil
}

// encryptionSettings returns the bucket's encryption settings: its SSE-S3
// implementation (storage.EncryptionAES256 when none is configured) and
// which writes use it (Scope). The stored document is parsed once per
// change (s.configs); an invalid one fails the write rather than silently
// choosing another provider or scope.
func (s *Server) encryptionSettings(ctx context.Context, bucket string) (storage.EncryptionSettings, error) {
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigEncryptionProvider)
	if err != nil {
		return storage.EncryptionSettings{}, err
	}
	if raw == nil {
		return storage.EncryptionSettings{Provider: storage.EncryptionAES256}, nil
	}
	v, err := s.configs.get(bucket, cacheKindEncryptionSettings, raw, func(b []byte) (any, error) {
		return storage.ParseEncryptionSettings(b)
	})
	if err != nil {
		return storage.EncryptionSettings{}, fmt.Errorf("s3: bucket %q: %w", bucket, err)
	}
	return v.(storage.EncryptionSettings), nil
}
