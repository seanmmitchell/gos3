package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
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
	"github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

type failOldVersionOnce struct {
	storage.Backend
	failed       bool
	truncateWalk bool
}

type leaderCheckedBackend struct {
	storage.Backend
	leader bool
	lists  int
}

type progressBackend struct {
	storage.Backend
	progress []byte
	writes   int
}

type leaderFlippingBackend struct {
	*progressBackend
	leader bool
}

func (b *leaderFlippingBackend) IsClusterLeader() bool { return b.leader }

func (b *leaderFlippingBackend) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	return b.progressBackend.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, func(info storage.ObjectInfo) error {
		b.leader = false
		return fn(info)
	})
}

func (b *progressBackend) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return b.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}

func (b *progressBackend) LoadReplicationProgress(ctx context.Context, bucket string) ([]byte, bool, error) {
	return bytes.Clone(b.progress), b.progress != nil, ctx.Err()
}

func (b *progressBackend) SaveReplicationProgress(ctx context.Context, bucket string, expected []byte, exists bool, next []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if (b.progress != nil) != exists || !bytes.Equal(b.progress, expected) {
		return errors.New("stale progress")
	}
	b.progress = bytes.Clone(next)
	b.writes++
	return nil
}

func TestWorkerPersistsRetryAndBackfillProgress(t *testing.T) {
	ctx := context.Background()
	base := memory.New()
	defer base.Close()
	backend := &progressBackend{Backend: base}
	for _, name := range []string{"source", "target"} {
		if err := backend.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := backend.SetBucketVersioning(ctx, name, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	version := storage.ObjectInfo{VersionID: "one", LastModified: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ObjectMeta: storage.ObjectMeta{ETag: "one"}}
	if _, err := copier.WriteVersion(ctx, backend, "source", "photo", version, strings.NewReader("image"), copier.Policy{}, copier.WriteImport); err != nil {
		t.Fatal(err)
	}
	const arn = "arn:aws:iam::000000000000:role/checkpoint"
	identity, err := iam.Open(iam.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	raw := []byte(`<ReplicationConfiguration><Role>` + arn + `</Role><Rule><ID>all</ID><Status>Enabled</Status><Prefix></Prefix><Destination><Bucket>arn:aws:s3:::target</Bucket></Destination><ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication></Rule></ReplicationConfiguration>`)
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplication, raw); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(s3.ReplicationState{Hash: s3.ReplicationRuleHash(raw), ActivatedAt: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplicationState, state); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(backend, identity, nil)
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("missing role did not leave a retryable copy")
	}
	var progress Progress
	if err := json.Unmarshal(backend.progress, &progress); err != nil || progress.RetryCount != 1 || progress.BackfillComplete || progress.LastError == "" {
		t.Fatalf("failed pass checkpoint: %+v, %v", progress, err)
	}
	if err := identity.PutReplicationRole(ctx, iam.ReplicationRole{ARN: arn, Grants: []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction}}}}); err != nil {
		t.Fatal(err)
	}
	if err := NewWorker(backend, identity, nil).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	progress = Progress{}
	if err := json.Unmarshal(backend.progress, &progress); err != nil || progress.RetryCount != 0 || !progress.BackfillComplete || progress.LastCompletedKey != "photo" || progress.LastError != "" {
		t.Fatalf("recovered pass checkpoint: %+v, %v", progress, err)
	}
	if _, err := backend.GetObjectInfo(ctx, "target", "photo", "one"); err != nil {
		t.Fatal(err)
	}
	writes := backend.writes
	if err := NewWorker(backend, identity, nil).RunOnce(ctx); err != nil || backend.writes != writes {
		t.Fatalf("idle scan wrote a duplicate checkpoint: writes=%d previous=%d err=%v", backend.writes, writes, err)
	}
	checkpoint := bytes.Clone(backend.progress)
	flipping := &leaderFlippingBackend{progressBackend: backend, leader: true}
	if err := NewWorker(flipping, identity, nil).RunOnce(ctx); !errors.Is(err, errReplicationLeaderChanged) {
		t.Fatalf("old leader continued scan: %v", err)
	}
	if !bytes.Equal(backend.progress, checkpoint) {
		t.Fatalf("old leader rewrote progress: %q", backend.progress)
	}
	if _, err := backend.DeleteObject(ctx, "source", "photo", "one"); err != nil {
		t.Fatal(err)
	}
	if err := NewWorker(backend, identity, nil).RunOnce(ctx); err != nil {
		t.Fatalf("empty source scan: %v", err)
	}
	progress = Progress{}
	if err := json.Unmarshal(backend.progress, &progress); err != nil || !progress.BackfillComplete || progress.LastCompletedKey != "" {
		t.Fatalf("empty source checkpoint: %+v, %v", progress, err)
	}
}

func (b *leaderCheckedBackend) IsClusterLeader() bool { return b.leader }

func (b *leaderCheckedBackend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	b.lists++
	return b.Backend.ListBuckets(ctx)
}

func TestWorkerScansOnlyClusterLeader(t *testing.T) {
	backend := &leaderCheckedBackend{Backend: memory.New()}
	defer backend.Close()
	worker := NewWorker(backend, nil, nil)
	if err := worker.RunOnce(context.Background()); err != nil || backend.lists != 0 {
		t.Fatalf("follower scanned bucket namespace: lists=%d err=%v", backend.lists, err)
	}
	backend.leader = true
	if err := worker.RunOnce(context.Background()); err != nil || backend.lists != 1 {
		t.Fatalf("leader skipped bucket namespace: lists=%d err=%v", backend.lists, err)
	}
}

func (b *failOldVersionOnce) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return b.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}

func (b *failOldVersionOnce) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	if versionID == "old" && !b.failed {
		b.failed = true
		return storage.ObjectInfo{}, nil, errors.New("injected source read failure")
	}
	return b.Backend.OpenObject(ctx, bucket, key, versionID)
}

func (b *failOldVersionOnce) WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error {
	if bucket == "source" && b.truncateWalk {
		b.truncateWalk = false
		return b.Backend.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, func(info storage.ObjectInfo) error {
			if err := fn(info); err != nil {
				return err
			}
			return errors.New("injected source walk failure")
		})
	}
	return b.Backend.WalkVersions(ctx, bucket, prefix, keyMarker, versionIDMarker, fn)
}

func TestWorkerRetriesOldestVersionBeforePublishingNewer(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	defer backend.Close()
	for _, name := range []string{"source", "target"} {
		if err := backend.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := backend.SetBucketVersioning(ctx, name, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	for i, version := range []string{"old", "new"} {
		info := storage.ObjectInfo{VersionID: version, LastModified: time.Date(2020+i, 1, 1, 0, 0, 0, 0, time.UTC), ObjectMeta: storage.ObjectMeta{ETag: version}}
		if _, err := copier.WriteVersion(ctx, backend, "source", "k", info, strings.NewReader(version), copier.Policy{}, copier.WriteImport); err != nil {
			t.Fatal(err)
		}
	}
	const arn = "arn:aws:iam::000000000000:role/ordered"
	identity, err := iam.Open(iam.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	if err := identity.PutReplicationRole(ctx, iam.ReplicationRole{ARN: arn, Grants: []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction}}}}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`<ReplicationConfiguration><Role>` + arn + `</Role><Rule><ID>all</ID><Status>Enabled</Status><Prefix></Prefix><Destination><Bucket>arn:aws:s3:::target</Bucket></Destination><ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication></Rule></ReplicationConfiguration>`)
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplication, raw); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(s3.ReplicationState{Hash: s3.ReplicationRuleHash(raw), ActivatedAt: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplicationState, state); err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(&failOldVersionOnce{Backend: backend, truncateWalk: true}, identity, nil)
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("injected source walk failure was not reported")
	}
	if _, err := backend.GetObjectInfo(ctx, "target", "k", "new"); err == nil {
		t.Fatal("partial version enumeration copied a newer destination version")
	}
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("injected source read failure was not reported")
	}
	if _, err := backend.GetObjectInfo(ctx, "target", "k", "new"); err == nil {
		t.Fatal("newer destination version published while older copy was pending")
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"old", "new"} {
		if _, err := backend.GetObjectInfo(ctx, "target", "k", version); err != nil {
			t.Fatalf("%s was not copied after retry: %v", version, err)
		}
	}
	latest, err := backend.GetObjectInfo(ctx, "target", "k", "")
	if err != nil || latest.VersionID != "new" {
		t.Fatalf("wrong latest after ordered retry: %+v, %v", latest, err)
	}
}

func TestWorkerCopiesEligibleVersionsAndRetries(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	for _, name := range []string{"source", "target"} {
		if err := backend.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := backend.SetBucketVersioning(ctx, name, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := iam.Open(iam.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const arn = "arn:aws:iam::000000000000:role/replication"
	role := iam.ReplicationRole{ARN: arn, Grants: []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction, iam.ReplicateDeleteAction}}}}
	if err := identity.PutReplicationRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`<ReplicationConfiguration><Role>` + arn + `</Role><Rule><ID>all</ID><Status>Enabled</Status><Prefix></Prefix><Destination><Bucket>arn:aws:s3:::target</Bucket></Destination><DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication></Rule></ReplicationConfiguration>`)
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplication, raw); err != nil {
		t.Fatal(err)
	}
	activate := func(backfill bool) {
		t.Helper()
		if backfill {
			raw = []byte(strings.Replace(string(raw), "</Rule>", "<ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication></Rule>", 1))
			if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplication, raw); err != nil {
				t.Fatal(err)
			}
		}
		state, _ := json.Marshal(s3.ReplicationState{Hash: s3.ReplicationRuleHash(raw), ActivatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplicationState, state); err != nil {
			t.Fatal(err)
		}
	}
	put := func(id, data string, at time.Time) {
		t.Helper()
		_, err := copier.WriteVersion(ctx, backend, "source", "k", storage.ObjectInfo{VersionID: id, LastModified: at, ObjectMeta: storage.ObjectMeta{ETag: id}}, strings.NewReader(data), copier.Policy{}, copier.WriteImport)
		if err != nil {
			t.Fatal(err)
		}
	}
	old := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	put("old", "old data", old)
	put("new", "new data", newer)
	activate(false)
	worker := NewWorker(backend, identity, nil)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.GetObjectInfo(ctx, "target", "k", "old"); err == nil {
		t.Fatal("older version copied without backfill")
	}
	copied, err := backend.GetObjectInfo(ctx, "target", "k", "new")
	if err != nil || copied.ReplicationOrigin == "" || !copied.LastModified.Equal(newer) {
		t.Fatalf("new version: %+v, %v", copied, err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	activate(true)
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("late backfill silently reordered destination history")
	}
	if _, err := backend.GetObjectInfo(ctx, "target", "k", "old"); err == nil {
		t.Fatal("late backfill changed destination history")
	}
	if err := backend.CreateBucket(ctx, "target2", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.SetBucketVersioning(ctx, "target2", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	role.Grants = append(role.Grants, iam.ReplicationGrant{SourceBucket: "source", DestinationBucket: "target2", Actions: []string{iam.ReplicateObjectAction, iam.ReplicateDeleteAction}})
	if err := identity.PutReplicationRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), "arn:aws:s3:::target", "arn:aws:s3:::target2", 1))
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplication, raw); err != nil {
		t.Fatal(err)
	}
	activate(false)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	latest, err := backend.GetObjectInfo(ctx, "target2", "k", "")
	if err != nil || latest.VersionID != "new" {
		t.Fatalf("backfill order: %+v, %v", latest, err)
	}
	if _, err := backend.GetObjectInfo(ctx, "target2", "k", "old"); err != nil {
		t.Fatal(err)
	}
	marker := storage.ObjectInfo{VersionID: "marker", LastModified: newer.Add(time.Hour), DeleteMarker: true}
	if _, err := copier.WriteVersion(ctx, backend, "source", "k", marker, nil, copier.Policy{}, copier.WriteImport); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := backend.GetObjectInfo(ctx, "target2", "k", "marker")
	if err != nil || !got.DeleteMarker || got.ReplicationOrigin == "" {
		t.Fatalf("delete marker: %+v, %v", got, err)
	}
	role.Grants = append(role.Grants, iam.ReplicationGrant{SourceBucket: "target2", DestinationBucket: "source", Actions: []string{iam.ReplicateObjectAction, iam.ReplicateDeleteAction}})
	if err := identity.PutReplicationRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	reverse := []byte(strings.Replace(string(raw), "arn:aws:s3:::target2", "arn:aws:s3:::source", 1))
	if err := backend.PutBucketConfig(ctx, "target2", storage.ConfigReplication, reverse); err != nil {
		t.Fatal(err)
	}
	reverseState, _ := json.Marshal(s3.ReplicationState{Hash: s3.ReplicationRuleHash(reverse), ActivatedAt: old})
	if err := backend.PutBucketConfig(ctx, "target2", storage.ConfigReplicationState, reverseState); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sourceVersions := 0
	if err := backend.WalkVersions(ctx, "source", "", "", "", func(storage.ObjectInfo) error { sourceVersions++; return nil }); err != nil {
		t.Fatal(err)
	}
	if sourceVersions != 3 {
		t.Fatalf("replication loop created %d source versions", sourceVersions)
	}
	if err := identity.DeleteReplicationRole(ctx, arn); err != nil {
		t.Fatal(err)
	}
	put("denied", "no copy", newer.Add(2*time.Hour))
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("missing role did not leave a retryable copy")
	}
	if _, err := backend.GetObjectInfo(ctx, "target2", "k", "denied"); err == nil {
		t.Fatal("version copied without role")
	}
}

func TestS3RuleExecutesAfterSourceWrite(t *testing.T) {
	ctx := context.Background()
	backend, err := multi.Open(ctx, multi.Options{Backings: []multi.Backing{{Name: "default", Kind: "memory", Backend: memory.New()}}})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	for _, bucket := range []string{"source", "target"} {
		if err := backend.CreateBucket(ctx, bucket, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := backend.SetBucketVersioning(ctx, bucket, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	const arn = "arn:aws:iam::000000000000:role/replication"
	identity, err := iam.Open(iam.Options{RootAccessKey: "ROOTTESTKEY", RootSecretKey: "root-test-secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	if err := identity.PutReplicationRole(ctx, iam.ReplicationRole{ARN: arn, Grants: []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction, iam.ReplicateDeleteAction}}}}); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(s3.New(s3.Options{Backend: backend, IAM: identity, Verifier: auth.NewVerifier(identity, auth.Options{Region: "us-east-1"})}))
	defer api.Close()
	client := awss3.New(awss3.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("ROOTTESTKEY", "root-test-secret-key", ""), BaseEndpoint: aws.String(api.URL), UsePathStyle: true, HTTPClient: api.Client(), Retryer: aws.NopRetryer{}})
	_, err = client.PutBucketReplication(ctx, &awss3.PutBucketReplicationInput{Bucket: aws.String("source"), ReplicationConfiguration: &types.ReplicationConfiguration{Role: aws.String(arn), Rules: []types.ReplicationRule{{Status: types.ReplicationRuleStatusEnabled, Prefix: aws.String(""), Destination: &types.Destination{Bucket: aws.String("arn:aws:s3:::target")}, DeleteMarkerReplication: &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusEnabled}}}}})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWorker(backend, identity, nil)
	worker.interval = 10 * time.Millisecond
	worker.Start(ctx)
	defer worker.Stop()
	put, err := client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("source"), Key: aws.String("k"), Body: strings.NewReader("payload")})
	if err != nil {
		t.Fatal(err)
	}
	var body io.ReadSeekCloser
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, body, err = backend.OpenObject(ctx, "target", "k", aws.ToString(put.VersionId))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	content, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(content) != "payload" {
		t.Fatalf("copied body %q: %v", content, err)
	}
	deleted, err := client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("source"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	var marker storage.ObjectInfo
	deadline = time.Now().Add(3 * time.Second)
	for {
		marker, err = backend.GetObjectInfo(ctx, "target", "k", aws.ToString(deleted.VersionId))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !marker.DeleteMarker || marker.ReplicationOrigin == "" {
		t.Fatalf("replicated delete marker: %+v, %v", marker, err)
	}
}

func TestWorkerResumesFromDurableDestinationVersions(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	backend, err := fs.Open(path, fs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	for _, name := range []string{"source", "target"} {
		if err := backend.CreateBucket(ctx, name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := backend.SetBucketVersioning(ctx, name, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := iam.Open(iam.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const arn = "arn:aws:iam::000000000000:role/replication"
	if err := identity.PutReplicationRole(ctx, iam.ReplicationRole{ARN: arn, Grants: []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction}}}}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`<ReplicationConfiguration><Role>` + arn + `</Role><Rule><Status>Enabled</Status><Prefix></Prefix><Destination><Bucket>arn:aws:s3:::target</Bucket></Destination></Rule></ReplicationConfiguration>`)
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplication, raw); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(s3.ReplicationState{Hash: s3.ReplicationRuleHash(raw), ActivatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplicationState, state); err != nil {
		t.Fatal(err)
	}
	version := storage.ObjectInfo{VersionID: "v1", LastModified: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), ObjectMeta: storage.ObjectMeta{ETag: "test"}}
	if _, err := copier.WriteVersion(ctx, backend, "source", "k", version, strings.NewReader("content"), copier.Policy{}, copier.WriteImport); err != nil {
		t.Fatal(err)
	}
	if err := NewWorker(backend, identity, nil).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err = fs.Open(path, fs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if err := NewWorker(backend, identity, nil).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := backend.WalkVersions(ctx, "target", "", "", "", func(info storage.ObjectInfo) error {
		count++
		if info.ReplicationOrigin == "" || info.VersionID != "v1" {
			t.Errorf("replicated version after restart: %+v", info)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("retry created %d versions, want one", count)
	}
}

func TestWorkerTagFilterAndPerCopyAuthorization(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	for _, bucket := range []string{"source", "target"} {
		if err := backend.CreateBucket(ctx, bucket, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := backend.SetBucketVersioning(ctx, bucket, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	identity, err := iam.Open(iam.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const arn = "arn:aws:iam::000000000000:role/replication"
	grant := iam.ReplicationGrant{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction, iam.ReplicateTagsAction}}
	if err := identity.PutReplicationRole(ctx, iam.ReplicationRole{ARN: arn, Grants: []iam.ReplicationGrant{grant}}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`<ReplicationConfiguration><Role>` + arn + `</Role><Rule><Status>Enabled</Status><Filter><Tag><Key>class</Key><Value>public</Value></Tag></Filter><Destination><Bucket>arn:aws:s3:::target</Bucket></Destination><ExistingObjectReplication><Status>Enabled</Status></ExistingObjectReplication></Rule></ReplicationConfiguration>`)
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplication, raw); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(s3.ReplicationState{Hash: s3.ReplicationRuleHash(raw), ActivatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err := backend.PutBucketConfig(ctx, "source", storage.ConfigReplicationState, state); err != nil {
		t.Fatal(err)
	}
	put := func(key, class string) {
		t.Helper()
		v := storage.ObjectInfo{VersionID: key, LastModified: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), ObjectMeta: storage.ObjectMeta{ETag: key, Tags: map[string]string{"class": class}}}
		if _, err := copier.WriteVersion(ctx, backend, "source", key, v, strings.NewReader(key), copier.Policy{}, copier.WriteImport); err != nil {
			t.Fatal(err)
		}
	}
	put("private", "private")
	put("public", "public")
	worker := NewWorker(backend, identity, nil)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.GetObjectInfo(ctx, "target", "private", "private"); err == nil {
		t.Fatal("nonmatching tag was copied")
	}
	if got, err := backend.GetObjectInfo(ctx, "target", "public", "public"); err != nil || got.Tags["class"] != "public" {
		t.Fatalf("matching tag copy: %+v, %v", got, err)
	}
	if _, err := backend.UpdateObjectMeta(ctx, "target", "public", "public", func(meta *storage.ObjectMeta) error {
		meta.ContentType = "application/changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("changed destination metadata was silently accepted")
	}
	if got, err := backend.GetObjectInfo(ctx, "target", "public", "public"); err != nil || got.ContentType != "application/changed" {
		t.Fatalf("destination conflict was overwritten: %+v, %v", got, err)
	}
	grant.Actions = []string{iam.ReplicateObjectAction}
	if err := identity.PutReplicationRole(ctx, iam.ReplicationRole{ARN: arn, Grants: []iam.ReplicationGrant{grant}}); err != nil {
		t.Fatal(err)
	}
	put("denied", "public")
	if err := worker.RunOnce(ctx); err == nil {
		t.Fatal("tag copy succeeded without ReplicateTags")
	}
	if _, err := backend.GetObjectInfo(ctx, "target", "denied", "denied"); err == nil {
		t.Fatal("unauthorized tag copy appeared")
	}
}
