package s3store

import (
	"context"
	"errors"
	"fmt"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// encryptionBucket returns the bucket after checking that the backing can
// store mode (storage.EncryptionAES256 or storage.EncryptionRestic):
// errors.ErrUnsupported (wrapped) when it cannot.
func (b *Backend) encryptionBucket(ctx context.Context, bucket, mode string) (*bucket, error) {
	if mode != storage.EncryptionAES256 && mode != storage.EncryptionRestic {
		return nil, errors.ErrUnsupported
	}
	if b.opts.Gateway {
		return nil, fmt.Errorf("s3store: encrypted objects on a gateway backing: %w", errors.ErrUnsupported)
	}
	if err := b.checkWriteOptions(storage.WriteOptions{Encryption: mode}); err != nil {
		return nil, err
	}
	bk, err := b.rlockBucket(ctx, bucket)
	if err != nil {
		return nil, err
	}
	lsn := bk.lsn
	bk.mu.RUnlock()
	return bk, b.waitRead(ctx, lsn)
}

// PrepareEncryption implements storage.EncryptionBackend: it creates the
// bucket's master key if needed (wrapped by the backing's KEK) and, for
// restic, prepares its repository. Without a KEK, on a gateway backing,
// for restic without the backing's credentials and for other modes it
// returns errors.ErrUnsupported.
func (b *Backend) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	bk, err := b.encryptionBucket(ctx, bucket, mode)
	if err != nil {
		return err
	}
	if _, err := b.bucketKey(ctx, bk, true); err != nil {
		return err
	}
	if mode == storage.EncryptionRestic {
		_, err = b.prepareRestic(ctx, bk)
	}
	return err
}

// ValidateEncryptionSettings checks that the bucket can use cfg's provider.
// For restic it checks (without initializing anything) that the bucket's
// repository is either reachable with its password or does not exist yet;
// it creates the bucket's master key, from which that password derives.
func (b *Backend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	if cfg.Provider != storage.EncryptionRestic {
		return b.PrepareEncryption(ctx, bucket, cfg.Provider)
	}
	bk, err := b.encryptionBucket(ctx, bucket, cfg.Provider)
	if err != nil {
		return err
	}
	if _, err := b.bucketKey(ctx, bk, true); err != nil {
		return err
	}
	loc, err := b.resticLocation(ctx, bk)
	if err != nil {
		return err
	}
	return b.restic.Validate(ctx, bk.id, loc, &b.recovery)
}

// EncryptionProviders implements storage.EncryptionProvidersBackend: none
// without a KEK (or on a gateway backing); with one AES256, plus restic
// when the restic executable is installed and the backing has credentials
// restic can use. The bucket is not consulted.
func (b *Backend) EncryptionProviders(ctx context.Context, bucket string) ([]string, error) {
	if err := checkCtx(ctx); err != nil {
		return nil, err
	}
	if b.opts.Gateway || len(b.conn.Load().kek) == 0 {
		return []string{}, nil
	}
	providers := []string{storage.EncryptionAES256}
	if b.resticAvailable() {
		providers = append(providers, storage.EncryptionRestic)
	}
	return providers, nil
}
