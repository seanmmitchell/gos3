package memory

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"

	"github.com/minio/sio"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func (b *Backend) PrepareEncryption(ctx context.Context, bucket, mode string) error {
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return err
	}
	if mode != storage.EncryptionAES256 {
		return errors.ErrUnsupported
	}
	return nil
}

func (b *Backend) ValidateEncryptionSettings(ctx context.Context, bucket string, cfg storage.EncryptionSettings) error {
	return b.PrepareEncryption(ctx, bucket, cfg.Provider)
}

func encryptVersion(v *version) error {
	if v.meta.Encryption == "" {
		return nil
	}
	if v.meta.Encryption != storage.EncryptionAES256 {
		return errors.ErrUnsupported
	}
	v.dataKey = make([]byte, 32)
	_, _ = rand.Read(v.dataKey)
	var out bytes.Buffer
	if _, err := sio.Encrypt(&out, bytes.NewReader(v.data), storage.EncryptionConfig(v.dataKey)); err != nil {
		return err
	}
	v.data = out.Bytes()
	return nil
}

func decryptedData(v *version) ([]byte, error) {
	if v.meta.Encryption == "" {
		return v.data, nil
	}
	var out bytes.Buffer
	_, err := sio.Decrypt(&out, bytes.NewReader(v.data), storage.EncryptionConfig(v.dataKey))
	return out.Bytes(), err
}
