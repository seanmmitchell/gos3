package admin

import (
	"context"
	"testing"

	"github.com/seanmmitchell/gos3/internal/compression"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func (e *testEnv) setCompressionMode(mode string) {
	e.t.Helper()
	if _, err := e.settings.Update(func(s *config.Settings) error {
		s.Compression.Mode = mode
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

func TestBucketCompression(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	path := "/buckets/photos/compression"

	var got bucketCompression
	e.root("GET", path, nil).json(t, &got)
	if got != (bucketCompression{ServerMode: "optional", Algorithm: "deflate", Level: 1, MinSize: 4096}) {
		t.Fatalf("GET without a setting = %+v", got)
	}

	r := e.root("PUT", path, map[string]any{"enabled": true})
	r.expectStatus(t, 200)
	r.json(t, &got)
	if !got.Enabled || !got.Effective {
		t.Fatalf("PUT enabled = %+v", got)
	}
	stored, err := compression.Load(context.Background(), e.backend, "photos")
	if err != nil || !stored.Enabled {
		t.Fatalf("stored setting = %+v, %v", stored, err)
	}
	var sum bucketSummary
	e.root("GET", "/buckets/photos", nil).json(t, &sum)
	if sum.Compression != (bucketCompressionSummary{Enabled: true, Effective: true}) {
		t.Fatalf("BucketSummary.compression = %+v", sum.Compression)
	}

	// The administrator's policy wins over the bucket's setting, and
	// requests that contradict it are refused.
	e.setCompressionMode(config.CompressionDisabled)
	e.root("GET", path, nil).json(t, &got)
	if !got.Enabled || got.Effective || got.ServerMode != "disabled" {
		t.Fatalf("GET while disabled = %+v", got)
	}
	msg := e.root("PUT", path, map[string]any{"enabled": true}).expectError(t, 409, codeConflict)
	if msg == "" {
		t.Error("no conflict message")
	}
	e.root("PUT", path, map[string]any{"enabled": false}).expectStatus(t, 200)

	e.setCompressionMode(config.CompressionEnforced)
	e.root("GET", path, nil).json(t, &got)
	if got.Enabled || !got.Effective {
		t.Fatalf("GET while enforced = %+v", got)
	}
	e.root("PUT", path, map[string]any{"enabled": false}).expectError(t, 409, codeConflict)
	e.root("PUT", path, map[string]any{"enabled": true}).expectStatus(t, 200)

	// DELETE returns the bucket to the server default.
	e.root("DELETE", path, nil).expectStatus(t, 204)
	e.root("DELETE", path, nil).expectStatus(t, 204)
	e.root("GET", path, nil).json(t, &got)
	if got.Enabled || !got.Effective {
		t.Fatalf("GET after DELETE while enforced = %+v", got)
	}

	// Malformed bodies and missing buckets.
	e.setCompressionMode(config.CompressionOptional)
	e.root("PUT", path, `{"enabled":"yes"}`).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", path, `{"enabled":true,"level":9}`).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", path, ``).expectError(t, 400, codeInvalidRequest)
	e.root("GET", "/buckets/nope/compression", nil).expectError(t, 404, codeNotFound)
	e.root("PUT", "/buckets/nope/compression", map[string]any{"enabled": true}).expectError(t, 404, codeNotFound)
	e.root("DELETE", "/buckets/nope/compression", nil).expectError(t, 404, codeNotFound)
}

// TestBucketCompressionScoped checks that the compression actions are
// evaluated against the bucket, so they can be delegated per bucket.
func TestBucketCompressionScoped(t *testing.T) {
	e := newEnv(t)
	e.createBucket("team-a", storage.CreateBucketOptions{})
	e.createBucket("other", storage.CreateBucketOptions{})
	e.putPolicy("team-compression", map[string]any{
		"Effect":   "Allow",
		"Action":   []string{"admin:GetBucketCompression", "admin:PutBucketCompression"},
		"Resource": "arn:gos3:admin:::bucket/team-*",
	})
	alice := e.addUser("alice", "team-compression")
	e.do(&alice, "PUT", Prefix+"/buckets/team-a/compression", map[string]any{"enabled": true}).expectStatus(t, 200)
	e.do(&alice, "GET", Prefix+"/buckets/team-a/compression", nil).expectStatus(t, 200)
	e.do(&alice, "DELETE", Prefix+"/buckets/team-a/compression", nil).expectStatus(t, 204)
	e.do(&alice, "PUT", Prefix+"/buckets/other/compression", map[string]any{"enabled": true}).expectError(t, 403, codeAccessDenied)
	e.do(&alice, "GET", Prefix+"/buckets/other/compression", nil).expectError(t, 403, codeAccessDenied)
	// Bucket-scoped grants do not show as the service-level capability.
	var w whoAmI
	e.do(&alice, "GET", Prefix+"/whoami", nil).json(t, &w)
	if w.Capabilities.ManageCompression {
		t.Error("manageCompression set for a bucket-scoped grant")
	}
	var root whoAmI
	e.root("GET", "/whoami", nil).json(t, &root)
	if !root.Capabilities.ManageCompression {
		t.Error("manageCompression not set for root")
	}
}

func TestUsageReportsStoredBytes(t *testing.T) {
	e := newEnv(t)
	e.createBucket("b", storage.CreateBucketOptions{})
	ctx := context.Background()
	w, err := e.backend.NewObjectWriterWith(ctx, "b", "k", storage.WriteOptions{
		Compression: storage.Compression{Algorithm: storage.CompressionDeflate, Level: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 1<<20) // zeros: compresses to almost nothing
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit(ctx, storage.ObjectMeta{ETag: "e"}, storage.CommitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e.putObject("b", "plain", "12345")
	var sum bucketSummary
	e.root("GET", "/buckets/b", nil).json(t, &sum)
	u := sum.Usage
	if u == nil || u.Bytes != 1<<20+5 || u.StoredBytes != info.StoredSize+5 || u.CompressedVersions != 1 ||
		u.StoredVersionBytes != u.StoredBytes {
		t.Fatalf("usage = %+v (compressed object stored in %d bytes)", u, info.StoredSize)
	}
	var st statsResponse
	e.root("GET", "/stats", nil).json(t, &st)
	if st.Storage.StoredBytes != u.StoredBytes || st.Storage.CompressedVersions != 1 {
		t.Fatalf("stats storage = %+v", st.Storage)
	}
}

// TestPutConfigWithoutCompression checks that a settings document from a
// client that predates compression keeps the current policy.
func TestPutConfigWithoutCompression(t *testing.T) {
	e := newEnv(t)
	e.setCompressionMode(config.CompressionEnforced)
	var doc map[string]any
	e.root("GET", "/config", nil).json(t, &doc)
	delete(doc, "compression")
	doc["name"] = "renamed"
	e.root("PUT", "/config", doc).expectStatus(t, 200)
	if s := e.settings.Get(); s.Name != "renamed" || s.Compression.Mode != config.CompressionEnforced || s.Compression.Level != 1 {
		t.Fatalf("settings after PUT without compression = %+v", s)
	}
	// Fields missing from "compression" keep their values; present ones apply.
	doc["compression"] = map[string]any{"level": 6}
	e.root("PUT", "/config", doc).expectStatus(t, 200)
	if c := e.settings.Get().Compression; c.Level != 6 || c.Mode != config.CompressionEnforced || c.MinSize != 4096 {
		t.Fatalf("compression after a partial update = %+v", c)
	}
	doc["compression"] = map[string]any{"mode": "sometimes"}
	e.root("PUT", "/config", doc).expectError(t, 400, codeInvalidSettings)
}
