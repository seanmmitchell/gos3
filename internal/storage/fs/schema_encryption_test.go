package fs

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestEncryptedRecordEncoding covers recordFormatVersionV3, used for
// versions that record their encryption: round trips (with and without
// compression), and rejection of every truncation and of trailing bytes.
// It goes through encodeRecord/decodeRecord only.
func TestEncryptedRecordEncoding(t *testing.T) {
	encrypted := fullVersionRecord()
	encrypted.Meta.Encryption = storage.EncryptionAES256
	both := encrypted
	both.Compression, both.StoredSize = storage.CompressionDeflate, 12345
	restic := fullVersionRecord()
	restic.Meta.Encryption = storage.EncryptionRestic
	for _, rec := range []versionRecord{encrypted, both, restic} {
		data, err := encodeRecord(&rec)
		if err != nil {
			t.Fatal(err)
		}
		eq(t, "format", data[0], recordFormatVersionV3)
		var got versionRecord
		if err := decodeRecord(data, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, rec) {
			t.Fatalf("round trip:\n got %#v\nwant %#v", got, rec)
		}
		for n := 1; n < len(data); n++ {
			if err := decodeRecord(data[:n], &got); !errors.Is(err, errCorrupt) {
				t.Fatalf("truncated at %d/%d: %v", n, len(data), err)
			}
		}
		if err := decodeRecord(append(bytes.Clone(data), 0), &got); !errors.Is(err, errCorrupt) {
			t.Fatalf("trailing byte: %v", err)
		}
	}
}
