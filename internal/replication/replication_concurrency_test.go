package replication

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/copier"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

const concurrencyRoleARN = "arn:aws:iam::000000000000:role/replication"

func concurrencySetup(t *testing.T) (*memory.Backend, *iam.Manager) {
	t.Helper()
	b := memory.New()
	t.Cleanup(func() { _ = b.Close() })
	for _, name := range []string{"source", "target"} {
		if err := b.CreateBucket(t.Context(), name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := b.SetBucketVersioning(t.Context(), name, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	id, err := iam.Open(iam.Options{RootAccessKey: "ROOTTESTKEY", RootSecretKey: "root-test-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = id.Close() })
	if err := id.PutReplicationRole(t.Context(), iam.ReplicationRole{ARN: concurrencyRoleARN, Grants: []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction}}}}); err != nil {
		t.Fatal(err)
	}
	return b, id
}

func TestIdenticalReplicationConfigPreservesPendingWrites(t *testing.T) {
	b, id := concurrencySetup(t)
	var now atomic.Int64
	now.Store(time.Now().Add(-time.Minute).UnixNano())
	api := httptest.NewServer(s3.New(s3.Options{Backend: b, IAM: id, Verifier: auth.NewVerifier(id, auth.Options{Region: "us-east-1"}), Now: func() time.Time { return time.Unix(0, now.Load()) }}))
	defer api.Close()
	c := awss3.New(awss3.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("ROOTTESTKEY", "root-test-secret-key", ""), BaseEndpoint: aws.String(api.URL), UsePathStyle: true, HTTPClient: api.Client(), Retryer: aws.NopRetryer{}})
	config := &awss3.PutBucketReplicationInput{Bucket: aws.String("source"), ReplicationConfiguration: &types.ReplicationConfiguration{Role: aws.String(concurrencyRoleARN), Rules: []types.ReplicationRule{{Status: types.ReplicationRuleStatusEnabled, Prefix: aws.String(""), Destination: &types.Destination{Bucket: aws.String("arn:aws:s3:::target")}}}}}
	if _, err := c.PutBucketReplication(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	put, err := c.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String("source"), Key: aws.String("key"), Body: strings.NewReader("pending payload")})
	if err != nil {
		t.Fatal(err)
	}
	now.Store(time.Now().Add(time.Minute).UnixNano())
	if _, err := c.PutBucketReplication(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := NewWorker(b, id, nil).RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetObjectInfo(t.Context(), "target", "key", aws.ToString(put.VersionId)); err != nil {
		t.Fatalf("identical configuration PUT lost eligible pending version: %v", err)
	}
}

type destinationWriteRace struct {
	storage.Backend
	inject func()
	fired  bool
}

func (b *destinationWriteRace) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return b.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}
func (b *destinationWriteRace) WalkVersions(ctx context.Context, bucket, prefix, km, vm string, fn func(storage.ObjectInfo) error) error {
	err := b.Backend.WalkVersions(ctx, bucket, prefix, km, vm, fn)
	if err == nil && bucket == "target" && !b.fired {
		b.fired = true
		b.inject()
	}
	return err
}

func TestReplicationDoesNotHideConcurrentNewerDestination(t *testing.T) {
	base, id := concurrencySetup(t)
	raw := []byte(`<ReplicationConfiguration><Role>` + concurrencyRoleARN + `</Role><Rule><Status>Enabled</Status><Prefix></Prefix><Destination><Bucket>arn:aws:s3:::target</Bucket></Destination><ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication></Rule></ReplicationConfiguration>`)
	if err := base.PutBucketConfig(t.Context(), "source", storage.ConfigReplication, raw); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(s3.ReplicationState{Hash: s3.ReplicationRuleHash(raw), ActivatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err := base.PutBucketConfig(t.Context(), "source", storage.ConfigReplicationState, state); err != nil {
		t.Fatal(err)
	}
	if _, err := copier.WriteVersion(t.Context(), base, "source", "key", storage.ObjectInfo{VersionID: "source-version", LastModified: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), ObjectMeta: storage.ObjectMeta{ETag: "old-source"}}, strings.NewReader("old source"), copier.Policy{}, copier.WriteImport); err != nil {
		t.Fatal(err)
	}
	b := &destinationWriteRace{Backend: base}
	var concurrent storage.ObjectInfo
	b.inject = func() {
		w, err := base.NewObjectWriter(t.Context(), "target", "key")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("new local")); err != nil {
			t.Fatal(err)
		}
		concurrent, err = w.Commit(t.Context(), storage.ObjectMeta{ETag: "new-local"}, storage.CommitOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = NewWorker(b, id, nil).RunOnce(t.Context())
	latest, err := base.GetObjectInfo(t.Context(), "target", "key", "")
	if err != nil {
		t.Fatal(err)
	}
	if latest.VersionID != concurrent.VersionID {
		t.Fatalf("replication hid newer acknowledged local version %s behind older source %s", concurrent.VersionID, latest.VersionID)
	}
}
