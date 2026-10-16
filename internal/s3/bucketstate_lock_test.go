package s3

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

// remoteVersioningBackend passes SetBucketVersioning on to another S3
// server, as a native bucket of an s3 backing does with its remote bucket.
type remoteVersioningBackend struct {
	*memory.Backend
	remote *testEnv
}

func (b *remoteVersioningBackend) SetBucketVersioning(ctx context.Context, name string, status storage.VersioningStatus) error {
	_, err := b.remote.client.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String(name),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatus(status)},
	})
	if err != nil {
		return err
	}
	return b.Backend.SetBucketVersioning(ctx, name, status)
}

// TestBucketStateLocksPerServer checks that the locks serializing bucket
// settings belong to one Server: a handler holding a bucket's lock while
// its backend calls another server in the same process (a remote S3
// service in tests) must not wait for itself.
func TestBucketStateLocksPerServer(t *testing.T) {
	remote := newTestEnv(t)
	remote.createBucket("photos")
	rb := &remoteVersioningBackend{remote: remote}
	e := newTestEnv(t, withOptions(func(o *Options) {
		rb.Backend = o.Backend.(*memory.Backend)
		o.Backend = rb
	}))
	e.createBucket("photos")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := e.client.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
		Bucket:                  aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	if err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	out, err := remote.client.GetBucketVersioning(ctx, &awss3.GetBucketVersioningInput{Bucket: aws.String("photos")})
	if err != nil || out.Status != types.BucketVersioningStatusEnabled {
		t.Fatalf("remote versioning: %v %v", out.Status, err)
	}
}
