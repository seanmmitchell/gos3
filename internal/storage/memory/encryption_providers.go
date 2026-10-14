package memory

import (
	"context"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// EncryptionProviders implements storage.EncryptionProvidersBackend: only
// native AES256. Restic archives need filesystem storage, so a memory
// backing can neither hold a restic bucket nor a replica of one.
func (b *Backend) EncryptionProviders(ctx context.Context, bucket string) ([]string, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	return []string{storage.EncryptionAES256}, nil
}

var _ storage.EncryptionProvidersBackend = (*Backend)(nil)
