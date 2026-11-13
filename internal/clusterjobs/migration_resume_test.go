package clusterjobs

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

type consistencyMigrationSource struct {
	storage.Backend
	records []cluster.JobRecord
}

func (s consistencyMigrationSource) ListImportJobs(context.Context) ([]cluster.JobRecord, error) {
	return s.records, nil
}

type consistencyImportedBackend struct {
	storage.Backend
	infos map[string]storage.BucketInfo
}

func (b consistencyImportedBackend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	return b.infos[name], ctx.Err()
}

func TestMigratedCopyJobPreservesBucketBindings(t *testing.T) {
	base := copyTestBackend(t)
	copyTestPut(t, base, "source", "key", "migrated payload")
	backend := migrationIdentifiedBackend{Backend: base}
	runner := &CopyRunner{Backend: backend, IsLeader: func() bool { return true }}
	spec, _, err := runner.Validate(t.Context(), []byte(`{"kind":"copy","source":{"bucket":"source"},"target":{"bucket":"target"},"options":{"configs":"none","versions":"all"}}`))
	if err != nil {
		t.Fatal(err)
	}
	job := jobs.Job{ID: "copy_migrate", Kind: "copy", State: jobs.StateQueued, CreatedAt: time.Now().UTC(), Spec: spec}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	source := consistencyMigrationSource{Backend: backend, records: []cluster.JobRecord{{ID: job.ID, Data: raw}}}
	bundle, err := cluster.ExportStandaloneBundle(t.Context(), source, config.SettingsFrom(config.Default()), nil, "ROOTTESTKEY", "root-test-secret-key", filepath.Join(t.TempDir(), "bundle"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	infos := map[string]storage.BucketInfo{}
	// buildImportImage assigns each exported bucket's InstanceID this way.
	for _, bucket := range bundle.Buckets {
		info := bucket.Info
		info.InstanceID = bucket.InstanceID
		infos[info.Name] = info
	}
	var migrated jobs.Job
	if err := json.Unmarshal(bundle.Jobs[0].Data, &migrated); err != nil {
		t.Fatal(err)
	}
	runner.Backend = consistencyImportedBackend{Backend: backend, infos: infos}
	if err := runner.Run(t.Context(), migrated); err != nil {
		t.Fatalf("supported migration must preserve resumable job: %v", err)
	}
	_, body, err := base.OpenObject(t.Context(), "target", "key", "")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if got, err := io.ReadAll(body); err != nil || string(got) != "migrated payload" {
		t.Fatalf("resumed copy content: %q, %v", got, err)
	}
}

type migrationIdentifiedBackend struct{ storage.Backend }

func (b migrationIdentifiedBackend) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	info, err := b.Backend.GetBucket(ctx, name)
	if name == "source" {
		info.InstanceID = "00112233445566778899aabbccddeeff"
	} else {
		info.InstanceID = "ffeeddccbbaa99887766554433221100"
	}
	return info, err
}
func (b migrationIdentifiedBackend) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	infos, err := b.Backend.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	for i := range infos {
		infos[i], err = b.GetBucket(ctx, infos[i].Name)
		if err != nil {
			return nil, err
		}
	}
	return infos, nil
}
func (b migrationIdentifiedBackend) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return b.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}
func (b consistencyImportedBackend) NewVersionWriter(ctx context.Context, bucket, key string) (storage.VersionWriter, error) {
	return b.Backend.(storage.VersionImporter).NewVersionWriter(ctx, bucket, key)
}
