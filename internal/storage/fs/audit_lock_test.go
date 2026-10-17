package fs

import (
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestImportCannotReplaceLockedVersion(t *testing.T) {
	for _, vid := range []string{storage.NullVersionID, "imported-id"} {
		t.Run(vid, func(t *testing.T) {
			b := openTest(t, "")
			if err := b.CreateBucket(ctx, "vault", storage.CreateBucketOptions{}); err != nil {
				t.Fatal(err)
			}
			v := storage.ObjectInfo{VersionID: vid, LastModified: time.Now(), ObjectMeta: storage.ObjectMeta{LegalHold: true, ETag: md5hex("original")}}
			if _, err := tryImport(b, "vault", "k", "original", v); err != nil {
				t.Fatal(err)
			}
			v.LegalHold = false
			v.ETag = md5hex("replacement")
			if _, err := tryImport(b, "vault", "k", "replacement", v); !errors.Is(err, storage.ErrPreconditionFailed) {
				t.Fatalf("locked import replaced: %v", err)
			}
			got, err := b.GetObjectInfo(ctx, "vault", "k", vid)
			if err != nil || !got.LegalHold || got.ETag != md5hex("original") {
				t.Fatalf("locked version changed: %+v, %v", got, err)
			}
		})
	}
}
