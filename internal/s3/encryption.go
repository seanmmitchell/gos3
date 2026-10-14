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

// cacheKindEncryptionProvider is the parsed-config cache kind of the
// bucket's storage.ConfigEncryptionProvider document.
const cacheKindEncryptionProvider = "s3/encryption-provider"

// prepareEncryption decides how a new object with metadata meta is stored
// and readies the backend for it. Without SSE the object is stored
// unencrypted (meta.Encryption ""). With SSE, meta.Encryption becomes the
// bucket's SSE-S3 implementation (encryptionProvider) unless already set
// (a multipart upload keeps the provider it was created with), and the
// backend prepares its keys or repository (storage.EncryptionBackend).
//
// meta.SSE is AES256 or, only for multipart uploads created by builds that
// accepted AWS KMS, a KMS algorithm: explicit KMS headers and KMS bucket
// defaults never get here (sseFromHeaders, defaultEncryption). Such uploads
// complete as SSE-S3 like new objects of their bucket, instead of failing.
//
// userAgent is the request's User-Agent ("" when there is no request of
// the writing client, as for completions and bucket configuration): a
// restic process's own writes are refused in restic buckets, so a
// repository cannot be stored in a bucket that archives into it.
func (s *Server) prepareEncryption(ctx context.Context, bucket string, meta *storage.ObjectMeta, userAgent string) error {
	if meta.SSE == "" {
		meta.Encryption = ""
		return nil
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
		var err error
		if mode, err = s.encryptionProvider(ctx, bucket); err != nil {
			return err
		}
	}
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

// encryptionProvider returns the bucket's SSE-S3 implementation
// (storage.EncryptionAES256 when none is configured). The stored document
// is parsed once per change (s.configs); an invalid one fails the write
// rather than silently choosing another provider.
func (s *Server) encryptionProvider(ctx context.Context, bucket string) (string, error) {
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigEncryptionProvider)
	if err != nil {
		return "", err
	}
	if raw == nil {
		return storage.EncryptionAES256, nil
	}
	v, err := s.configs.get(bucket, cacheKindEncryptionProvider, raw, func(b []byte) (any, error) {
		return storage.ParseEncryptionProvider(b)
	})
	if err != nil {
		return "", fmt.Errorf("s3: bucket %q: %w", bucket, err)
	}
	return v.(string), nil
}
