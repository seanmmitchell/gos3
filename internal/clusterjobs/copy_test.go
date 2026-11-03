package clusterjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/seanmmitchell/gos3/internal/copyjob"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func copyTestBackend(t *testing.T) *memory.Backend {
	t.Helper()
	b := memory.New()
	t.Cleanup(func() { _ = b.Close() })
	for _, name := range []string{"source", "target"} {
		if err := b.CreateBucket(context.Background(), name, storage.CreateBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := b.SetBucketVersioning(context.Background(), name, storage.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func copyTestPut(t *testing.T, b *memory.Backend, bucket, key, body string) storage.ObjectInfo {
	t.Helper()
	ctx := context.Background()
	w, err := b.NewObjectWriter(ctx, bucket, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	oi, err := w.Commit(ctx, storage.ObjectMeta{ETag: body}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return oi
}

func TestCopyRunnerPreservesVersionsAndRetriesIdempotently(t *testing.T) {
	b := copyTestBackend(t)
	first := copyTestPut(t, b, "source", "pre-key", "first")
	second := copyTestPut(t, b, "source", "pre-key", "second")
	r := &CopyRunner{Backend: b, IsLeader: func() bool { return true }}
	norm, labels, err := r.Validate(context.Background(), []byte(`{"kind":"copy","source":{"bucket":"source","prefix":"pre-"},"target":{"bucket":"target","prefix":"dst-"},"options":{"configs":"none","versions":"all"}}`))
	if err != nil || labels["source"] != "source" || labels["target"] != "target" {
		t.Fatalf("validation: %s, %+v, %v", norm, labels, err)
	}
	job := jobs.Job{ID: "copy_1", Kind: "copy", Spec: norm}
	for i := 0; i < 2; i++ {
		if err := r.RunWithProgress(context.Background(), job, func(_ context.Context, _ string, status, checkpoint json.RawMessage) error {
			job.Status = append([]byte(nil), status...)
			job.Checkpoint = append([]byte(nil), checkpoint...)
			return nil
		}); err != nil {
			t.Fatalf("copy pass %d: %v", i, err)
		}
	}
	for _, want := range []storage.ObjectInfo{first, second} {
		info, reader, err := b.OpenObject(context.Background(), "target", "dst-key", want.VersionID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil || string(got) != want.ETag || !info.LastModified.Equal(want.LastModified) {
			t.Fatalf("copied %s: %+v, %q, %v", want.VersionID, info, got, err)
		}
	}
}

func TestCopyRunnerRefusesTargetChangedAfterSubmission(t *testing.T) {
	b := copyTestBackend(t)
	copyTestPut(t, b, "source", "key", "source")
	r := &CopyRunner{Backend: b, IsLeader: func() bool { return true }}
	spec := []byte(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"},"options":{"configs":"none","versions":"all"}}`)
	norm, _, err := r.Validate(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	copyTestPut(t, b, "target", "key", "other writer")
	if err := r.Run(context.Background(), jobs.Job{ID: "copy_3", Kind: "copy", Spec: norm}); !errors.Is(err, jobs.ErrConflict) || !errors.Is(err, jobs.ErrPermanent) {
		t.Fatalf("copy overwrote a target changed after submission: %v", err)
	}
	_, reader, err := b.OpenObject(context.Background(), "target", "key", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(got) != "other writer" {
		t.Fatalf("target writer's object changed: %q, %v", got, err)
	}
}

func TestCopyRunnerRejectsUnsafeOrUnsupportedRequests(t *testing.T) {
	b := copyTestBackend(t)
	r := &CopyRunner{Backend: b, IsLeader: func() bool { return true }}
	for name, raw := range map[string]string{
		"mirror":          `{"kind":"mirror","source":{"bucket":"source"},"target":{"bucket":"target"},"options":{"configs":"none"}}`,
		"target creation": `{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"newtarget","create":{}},"options":{"configs":"none"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := r.Validate(context.Background(), json.RawMessage(raw)); !errors.Is(err, jobs.ErrInvalidSpec) {
				t.Fatalf("request was accepted: %v", err)
			}
		})
	}
	copyTestPut(t, b, "target", "occupied", "keep")
	if _, _, err := r.Validate(context.Background(), []byte(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"},"options":{"configs":"none"}}`)); !errors.Is(err, jobs.ErrConflict) {
		t.Fatalf("nonempty target accepted with onExisting=fail: %v", err)
	}
}

func TestCopyRunnerCopiesSelectedConfigsAndMandatoryACLGuards(t *testing.T) {
	b := copyTestBackend(t)
	ctx := context.Background()
	put := func(bucket, key, doc string) {
		t.Helper()
		if err := b.PutBucketConfig(ctx, bucket, key, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	put("source", storage.ConfigCORS, `<CORSConfiguration/>`)
	put("source", storage.ConfigPublicAccessBlock, `<PublicAccessBlockConfiguration><IgnorePublicAcls>true</IgnorePublicAcls></PublicAccessBlockConfiguration>`)
	put("source", storage.ConfigPolicy, `{"Statement":{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::source/*"}}`)
	put("source", storage.ConfigReplication, `<ReplicationConfiguration/>`)
	put("source", storage.ConfigReplicationState, `{"hash":"source-only"}`)
	r := &CopyRunner{Backend: b, IsLeader: func() bool { return true }}
	norm, _, err := r.Validate(ctx, []byte(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx, jobs.Job{ID: "copy_configs", Kind: "copy", Spec: norm}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{storage.ConfigCORS, storage.ConfigPublicAccessBlock, storage.ConfigPolicy} {
		if _, err := b.GetBucketConfig(ctx, "target", key); err != nil {
			t.Fatalf("target config %s: %v", key, err)
		}
	}
	if got, err := b.GetBucketConfig(ctx, "target", storage.ConfigPolicy); err != nil || !json.Valid(got) || !bytes.Contains(got, []byte("arn:aws:s3:::target/*")) {
		t.Fatalf("rewritten target policy: %s, %v", got, err)
	}
	if _, err := b.GetBucketConfig(ctx, "target", storage.ConfigReplication); !errors.Is(err, storage.ErrConfigNotFound) {
		t.Fatalf("replication rules were copied: %v", err)
	}
	if err := b.DeleteBucketConfig(ctx, "target", storage.ConfigPublicAccessBlock); err != nil {
		t.Fatal(err)
	}
	none := copyjob.Spec{}
	if err := json.Unmarshal(norm, &none); err != nil {
		t.Fatal(err)
	}
	none.Options.Configs = copyjob.ConfigsNone
	if _, err := syncClusterConfigs(ctx, b, none, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetBucketConfig(ctx, "target", storage.ConfigPublicAccessBlock); err != nil {
		t.Fatalf("mandatory ACL guard absent with configs none: %v", err)
	}
	none.Options.Configs = copyjob.ConfigsAll
	if _, err := syncClusterConfigs(ctx, b, none, false); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{storage.ConfigReplication, storage.ConfigReplicationState} {
		if _, err := b.GetBucketConfig(ctx, "target", key); !errors.Is(err, storage.ErrConfigNotFound) {
			t.Fatalf("source-only %s was copied with configs all: %v", key, err)
		}
	}
}

func TestCopyRunnerRejectsUnsupportedSourceEncryptionConfig(t *testing.T) {
	b := copyTestBackend(t)
	ctx := context.Background()
	if err := b.PutBucketConfig(ctx, "source", storage.ConfigEncryption, []byte(`<ServerSideEncryptionConfiguration/>`)); err != nil {
		t.Fatal(err)
	}
	r := &CopyRunner{Backend: b, IsLeader: func() bool { return true }}
	_, _, err := r.Validate(ctx, []byte(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"}}`))
	if !errors.Is(err, jobs.ErrInvalidSpec) {
		t.Fatalf("accepted unsupported encryption config: %v", err)
	}
}

type failingConfigBackend struct {
	storage.Backend
	failBucket string
	failKey    string
	failErr    error
}

func (b failingConfigBackend) PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error {
	if bucket == b.failBucket && (b.failKey == "" || b.failKey == key) {
		if b.failErr != nil {
			return b.failErr
		}
		return storage.ErrBackingUnavailable
	}
	return b.Backend.PutBucketConfig(ctx, bucket, key, data)
}

func TestClusterConfigCopySkipsUnsupportedDocumentButRequiresACLGuard(t *testing.T) {
	b := copyTestBackend(t)
	ctx := context.Background()
	if err := b.PutBucketConfig(ctx, "source", storage.ConfigCORS, []byte(`<CORSConfiguration/>`)); err != nil {
		t.Fatal(err)
	}
	sp := copyjob.Spec{Source: copyjob.SourceSpec{Bucket: "source"}, Target: copyjob.TargetSpec{Bucket: "target"}, Options: copyjob.SpecOptions{Configs: copyjob.ConfigsSafe}}
	unsupported := failingConfigBackend{Backend: b, failBucket: "target", failKey: storage.ConfigCORS, failErr: storage.ErrNotSupported}
	warnings, err := syncClusterConfigs(ctx, unsupported, sp, false)
	if err != nil || len(warnings) != 1 {
		t.Fatalf("optional unsupported config: warnings=%v err=%v", warnings, err)
	}
	if err := b.PutBucketConfig(ctx, "source", storage.ConfigPublicAccessBlock, []byte(`<PublicAccessBlockConfiguration><IgnorePublicAcls>true</IgnorePublicAcls></PublicAccessBlockConfiguration>`)); err != nil {
		t.Fatal(err)
	}
	sp.Options.Configs = copyjob.ConfigsNone
	unsupported.failKey = storage.ConfigPublicAccessBlock
	_, err = syncClusterConfigs(ctx, unsupported, sp, false)
	if !errors.Is(err, jobs.ErrPermanent) || !errors.Is(err, storage.ErrNotSupported) {
		t.Fatalf("ACL guard was skipped: %v", err)
	}
}

func TestCopyRunnerStopsBeforeObjectsWhenConfigCommitFails(t *testing.T) {
	b := copyTestBackend(t)
	ctx := context.Background()
	copyTestPut(t, b, "source", "key", "body")
	if err := b.PutBucketConfig(ctx, "source", storage.ConfigCORS, []byte(`<CORSConfiguration/>`)); err != nil {
		t.Fatal(err)
	}
	r := &CopyRunner{Backend: failingConfigBackend{Backend: b, failBucket: "target"}, IsLeader: func() bool { return true }}
	norm, _, err := r.Validate(ctx, []byte(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(ctx, jobs.Job{ID: "copy_config_failure", Kind: "copy", Spec: norm}); !errors.Is(err, storage.ErrBackingUnavailable) {
		t.Fatalf("config commit failure was not returned: %v", err)
	}
	if _, _, err := b.OpenObject(ctx, "target", "key", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("object copied despite failed config commit: %v", err)
	}
}

func TestCopyRunnerResumesAfterCommittedKeyCheckpoint(t *testing.T) {
	b := copyTestBackend(t)
	copyTestPut(t, b, "source", "pre-a", "a")
	copyTestPut(t, b, "target", "dst-a", "a")
	copyTestPut(t, b, "source", "pre-b", "b")
	r := &CopyRunner{Backend: b, IsLeader: func() bool { return true }}
	norm, _, err := r.Validate(context.Background(), []byte(`{"kind":"copy","source":{"bucket":"source","prefix":"pre-"},"target":{"bucket":"target","prefix":"dst-"},"options":{"configs":"none","versions":"all","onExisting":"merge","finalPass":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{ID: "copy_2", Kind: "copy", Spec: norm, Checkpoint: []byte(`{"pass":0,"lastKey":"pre-a"}`)}
	var phase string
	var statusDoc, checkpointDoc json.RawMessage
	err = r.RunWithProgress(context.Background(), job, func(_ context.Context, nextPhase string, status, checkpoint json.RawMessage) error {
		phase, statusDoc, checkpointDoc = nextPhase, append([]byte(nil), status...), append([]byte(nil), checkpoint...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		KeysScanned int64 `json:"keysScanned"`
		Passes      int64 `json:"passes"`
	}
	var checkpoint copyCheckpoint
	if err := json.Unmarshal(statusDoc, &status); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(checkpointDoc, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if phase != "copying" || status.KeysScanned != 1 || status.Passes != 1 || checkpoint.Pass != 1 || checkpoint.LastKey != "" {
		t.Fatalf("resumed progress: phase=%s status=%+v checkpoint=%+v", phase, status, checkpoint)
	}
	_, reader, err := b.OpenObject(context.Background(), "target", "dst-b", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(got) != "b" {
		t.Fatalf("resumed key: %q, %v", got, err)
	}
}
