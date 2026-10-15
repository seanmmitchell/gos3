package storage_test

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestPhysicalSize(t *testing.T) {
	const c = 64 << 10 // crypt.ChunkSize
	meta := func(enc string) storage.ObjectMeta { return storage.ObjectMeta{Encryption: enc} }
	for _, tc := range []struct {
		name string
		info storage.ObjectInfo
		want int64
	}{
		{"plain", storage.ObjectInfo{Size: 100}, 100},
		{"compressed", storage.ObjectInfo{Size: 100, Compression: storage.CompressionDeflate, StoredSize: 40}, 40},
		{"aes256", storage.ObjectInfo{ObjectMeta: meta(storage.EncryptionAES256), Size: 2*c + 1}, 2*c + 1 + 3*16},
		{"aes256 compressed", storage.ObjectInfo{ObjectMeta: meta(storage.EncryptionAES256), Size: 100,
			Compression: storage.CompressionDeflate, StoredSize: 40}, 40 + 16},
		// Empty objects have no data file.
		{"aes256 empty", storage.ObjectInfo{ObjectMeta: meta(storage.EncryptionAES256)}, 0},
		// Restic data is shared between snapshots: count the content, so
		// usage never shows it as (near 100%) compression savings.
		{"restic", storage.ObjectInfo{ObjectMeta: meta(storage.EncryptionRestic), Size: 1 << 30}, 1 << 30},
	} {
		if got := tc.info.PhysicalSize(); got != tc.want {
			t.Errorf("%s: PhysicalSize() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
