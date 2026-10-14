package s3

import (
	"context"
	"errors"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func (s *Server) prepareEncryption(ctx context.Context, bucket string, meta *storage.ObjectMeta, userAgent ...string) error {
	if meta.SSE == "" {
		meta.Encryption = ""
		return nil
	}
	if meta.SSE != "AES256" {
		return s3err.ErrNotImplemented.WithMessage("AWS KMS encryption is not implemented. Use SSE-S3 (AES256).")
	}
	mode := meta.Encryption
	if mode == "" {
		var err error
		mode, err = storage.EncryptionProvider(ctx, s.backend, bucket)
		if err != nil {
			return err
		}
	}
	if mode == storage.EncryptionRestic && len(userAgent) > 0 && strings.Contains(userAgent[0], storage.ResticUserAgent) {
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
