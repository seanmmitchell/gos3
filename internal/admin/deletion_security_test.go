package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

type refuseBucketDelete struct{ storage.Backend }

func (b refuseBucketDelete) DeleteBucket(context.Context, string) error {
	return storage.ErrBucketNotEmpty
}

func TestFailedForceDeletePreservesConfiguration(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Backend = refuseBucketDelete{d.Backend} })
	e.createBucket("guarded", storage.CreateBucketOptions{ObjectLockEnabled: true})
	ctx := t.Context()
	for _, key := range storage.AllConfigKeys {
		if err := e.backend.PutBucketConfig(ctx, "guarded", key, []byte("guard")); err != nil {
			t.Fatal(err)
		}
	}
	// Use valid documents for the lock guard's preflight.
	if err := e.backend.DeleteBucketConfig(ctx, "guarded", storage.ConfigObjectLock); err != nil {
		t.Fatal(err)
	}
	e.root("DELETE", "/buckets/guarded?force=true", nil).expectStatus(t, 409)
	for _, key := range storage.AllConfigKeys {
		got, err := e.backend.GetBucketConfig(ctx, "guarded", key)
		if key == storage.ConfigObjectLock {
			if !errors.Is(err, storage.ErrConfigNotFound) {
				t.Fatal(err)
			}
		} else if err != nil || string(got) != "guard" {
			t.Errorf("surviving bucket lost %s: %q %v", key, got, err)
		}
	}
}
