package fs

import (
	"context"
	"os/exec"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// resticLookPath finds the restic executable (replaced by tests).
var resticLookPath = exec.LookPath

// EncryptionProviders implements storage.EncryptionProvidersBackend:
// native AES256 always, restic when the restic executable is on the
// server's PATH. The bucket is not consulted (availability is the same for
// every bucket of the backing), so a replica that does not hold the bucket
// yet can still be asked.
func (b *Backend) EncryptionProviders(ctx context.Context, bucket string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	providers := []string{storage.EncryptionAES256}
	if _, err := resticLookPath("restic"); err == nil {
		providers = append(providers, storage.EncryptionRestic)
	}
	return providers, nil
}

var _ storage.EncryptionProvidersBackend = (*Backend)(nil)
