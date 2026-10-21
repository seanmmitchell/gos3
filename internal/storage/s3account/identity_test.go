package s3account

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestNativeBucketIdentityUsesRemoteCreationDate(t *testing.T) {
	remote := s3clienttest.Start(t)
	ctx := context.Background()
	b := openReady(t, localOptions(t, remote, NewClaims(), t.TempDir()))
	noErr(t, "create native bucket", b.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}))
	first, err := b.GetBucketIdentity(ctx, "photos")
	noErr(t, "first identity", err)
	if first.CreatedAt.IsZero() {
		t.Fatal("remote creation date was not recorded")
	}
	cached, err := b.GetBucket(ctx, "photos")
	noErr(t, "cached bucket", err)
	// Recreate the remote bucket without updating gos3's local attachment.
	noErr(t, "delete remote bucket", remote.Backend.DeleteBucket(ctx, "photos"))
	time.Sleep(20 * time.Millisecond) // remote ListBuckets reports millisecond timestamps
	remote.CreateBucket(t, "photos")
	second, err := b.GetBucketIdentity(ctx, "photos")
	noErr(t, "replacement identity", err)
	if second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replacement kept its identity: %v", second.CreatedAt)
	}
	stillCached, err := b.GetBucket(ctx, "photos")
	noErr(t, "cached bucket after replacement", err)
	if !stillCached.CreatedAt.Equal(cached.CreatedAt) {
		t.Fatal("test did not preserve the cached attachment time")
	}
	denyList := func(r *http.Request) int {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			return http.StatusForbidden
		}
		return 0
	}
	remote.Fail.Store(&denyList)
	if _, err := b.GetBucketIdentity(ctx, "photos"); !errors.Is(err, storage.ErrRemoteDenied) {
		t.Fatalf("unverifiable identity must fail closed: %v", err)
	}
}
