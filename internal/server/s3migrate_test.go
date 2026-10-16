package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// syncBuffer is a bytes.Buffer safe for concurrent writers (logs).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const legacyTagging = `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><TagSet><Tag><Key>team</Key><Value>blue</Value></Tag></TagSet></Tagging>`

// legacyGatewayHome writes, on remote, what a gateway backing of a build
// before s3 account backings left behind: an s3store at bucket/prefix whose
// marker has the gateway flag, holding the records and configuration
// documents of the buckets, whose objects are in remote buckets of the
// same names.
func legacyGatewayHome(t *testing.T, remote *s3clienttest.Remote, bucket, prefix string, buckets map[string]map[string]string) {
	t.Helper()
	ctx := context.Background()
	rc := remote.Client(t)
	st, err := s3store.Open(s3store.Options{
		Name: "legacy", Client: rc, Endpoint: remote.URL, Region: s3clienttest.Region, AccessKey: s3clienttest.AccessKey,
		SecretKey: s3clienttest.SecretKey, PathStyle: true, Bucket: bucket, Prefix: prefix, TempDir: t.TempDir(),
		RecoveryInterval: -1, Logger: testLogger(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-st.Ready():
	case <-time.After(30 * time.Second):
		t.Fatalf("legacy home did not load: %+v", st.Status())
	}
	for name, objects := range buckets {
		if err := st.CreateBucket(ctx, name, storage.CreateBucketOptions{Region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutBucketConfig(ctx, name, storage.ConfigTagging, []byte(legacyTagging)); err != nil {
			t.Fatal(err)
		}
		if err := rc.CreateBucket(ctx, name); err != nil {
			t.Fatal(err)
		}
		for key, body := range objects {
			remotePut(t, rc, name, key, body)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Mark the store as a gateway's metadata prefix.
	marker := legacyMarker(t, remote, bucket, prefix)
	marker["gateway"] = true
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Put(ctx, bucket, prefix+"gos3.json", bytes.NewReader(raw), int64(len(raw)), s3client.PutOptions{}); err != nil {
		t.Fatal(err)
	}
}

// legacyMarker reads the gos3.json marker at bucket/prefix of remote.
func legacyMarker(t *testing.T, remote *s3clienttest.Remote, bucket, prefix string) map[string]any {
	t.Helper()
	_, rd, err := remote.Backend.OpenObject(context.Background(), bucket, prefix+"gos3.json", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	data, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("marker %s: %v", data, err)
	}
	return m
}

// (f) A server configured with the deprecated keys of a gateway backing
// (bucket, prefix, gateway: true) over a legacy gateway home: the keys are
// mapped (and logged as deprecated), the buckets become native buckets
// adopted on their remote buckets, with their configuration documents, and
// the home becomes an ordinary home. The migration is idempotent.
func TestS3LegacyGatewayMigration(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "gwmeta")
	legacyGatewayHome(t, remote, "gwmeta", "meta/", map[string]map[string]string{
		"shared": {"obj": "native data", "dir/deep": "deeper"},
		"empty":  {},
	})

	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Backings = []config.BackingConfig{{Name: "gw", Backend: config.BackendS3, S3: &config.S3Config{
		Endpoint: remote.URL, LegacyBucket: "gwmeta", LegacyPrefix: "meta", LegacyGateway: true, Addressing: "path",
		AccessKey: s3clienttest.AccessKey, SecretKey: s3clienttest.SecretKey,
	}}}
	logs := &syncBuffer{}
	srv, err := New(cfg, slog.New(slog.NewTextHandler(io.MultiWriter(logs, tWriter{t}), nil)))
	if err != nil {
		t.Fatal(err)
	}
	run := serve(t, srv)
	for _, key := range []string{"bucket", "prefix", "gateway"} {
		if !strings.Contains(logs.String(), "s3 backing \\\"gw\\\": "+key+" (") {
			t.Fatalf("no deprecation warning for %s in:\n%s", key, logs)
		}
	}
	if b := backingStatuses(t, run.url)["gw"]; b.Location != "s3://gwmeta/meta/" || b.DefaultKind != "native" || !b.Healthy {
		t.Fatalf("gw: %+v", b)
	}
	check := func(url string) {
		t.Helper()
		headBucket(t, url, "shared", "gw", "native")
		headBucket(t, url, "empty", "gw", "native")
		getBody(t, url+"/shared/obj", "native data")
		getBody(t, url+"/shared/dir/deep", "deeper")
		res := do(t, "GET", url+"/shared?tagging", nil, &rootCreds, nil)
		if res.status != 200 || !bytes.Contains(res.body, []byte("<Key>team</Key>")) {
			t.Fatalf("tagging of shared: %d %s", res.status, res.body)
		}
	}
	check(run.url)
	if m := legacyMarker(t, remote, "gwmeta", "meta/"); m["gateway"] == true {
		t.Fatalf("the home is still marked as a gateway: %v", m)
	}
	// Writes go to the remote bucket.
	mustStatus(t, "put", do(t, "PUT", run.url+"/shared/new", []byte("fresh"), &rootCreds, nil), 200)
	if _, err := remote.Backend.GetObjectInfo(context.Background(), "shared", "new", ""); err != nil {
		t.Fatalf("new object on the remote: %v", err)
	}

	// Again: nothing left to migrate.
	if err := run.stop(); err != nil {
		t.Fatal(err)
	}
	run = start(t, cfg)
	check(run.url)
	getBody(t, run.url+"/shared/new", "fresh")
	// The migrated buckets were adopted: deleting one keeps its remote
	// bucket.
	mustStatus(t, "delete empty", do(t, "DELETE", run.url+"/empty", nil, &rootCreds, nil), 204)
	if !remoteHas(remote, "empty") {
		t.Fatal("deleting a migrated bucket deleted its remote bucket")
	}
}
