package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

func TestS3StageStoreConfirmsOnlyDurableExactCopies(t *testing.T) {
	type object struct {
		data   []byte
		digest string
	}
	var mu sync.Mutex
	objects := map[string]object{}
	failBefore, failAfter, puts := false, false, 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.URL.Path
		switch r.Method {
		case http.MethodGet:
			obj, ok := objects[key]
			if !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
				return
			}
			w.Header().Set("X-Amz-Meta-Gos3-Sha256", obj.digest)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(obj.data)
		case http.MethodPut:
			if failBefore {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if _, exists := objects[key]; exists && r.Header.Get("If-None-Match") == "*" {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if !strings.Contains(key, "/probe-") {
				puts++
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			objects[key] = object{data: data, digest: r.Header.Get("X-Amz-Meta-Gos3-Sha256")}
			if failAfter {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("ETag", `"remote-copy"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer remote.Close()
	client, err := s3client.New(s3client.Config{Endpoint: remote.URL, AccessKey: "a", SecretKey: "s", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewS3StageStore(client, "replicas", "cluster/node-a/", 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("verified object bytes")
	digest := sha256.Sum256(payload)
	if err := store.Stage(ctx, "good", []byte("wrong"), digest); err == nil {
		t.Fatal("mismatched memory stage was acknowledged")
	}
	if err := store.Stage(ctx, "good", payload, digest); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx, "good"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Read(ctx, "good")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("durable remote bytes = %q, %v", got, err)
	}
	mu.Lock()
	putCount := puts
	mu.Unlock()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewS3StageStore(client, "replicas", "cluster/node-a/", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Stage(ctx, "good", payload, digest); err != nil {
		t.Fatalf("restart retry: %v", err)
	}
	if err := store.Persist(ctx, "good"); err != nil {
		t.Fatalf("restart durable retry: %v", err)
	}
	mu.Lock()
	if puts != putCount {
		t.Fatalf("idempotent retry wrote another copy: %d to %d", putCount, puts)
	}
	mu.Unlock()
	if err := store.Stage(ctx, "good", []byte("different"), sha256.Sum256([]byte("different"))); err != nil {
		t.Fatalf("memory staging after restart: %v", err)
	}
	if err := store.Persist(ctx, "good"); err == nil {
		t.Fatal("operation ID reused with different remote bytes was durably acknowledged")
	}

	mu.Lock()
	failBefore = true
	mu.Unlock()
	if err := store.Stage(ctx, "uncertain", payload, digest); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(ctx, "uncertain"); err == nil {
		t.Fatal("S3 503 before commit was counted as durable")
	}
	mu.Lock()
	failBefore, failAfter = false, true
	mu.Unlock()
	if err := store.Persist(ctx, "uncertain"); err != nil {
		t.Fatalf("readback did not resolve S3 503 after exact commit: %v", err)
	}
	mu.Lock()
	failAfter = false
	key := "/replicas/cluster/node-a/" + hex.EncodeToString([]byte("uncertain"))
	obj := objects[key]
	obj.data = []byte("corrupt bytes")
	objects[key] = obj
	mu.Unlock()
	if _, err := store.Read(ctx, "uncertain"); err == nil {
		t.Fatal("corrupt remote bytes passed digest verification")
	}
	if err := store.Repair(ctx, "uncertain", payload); err != nil {
		t.Fatalf("repair remote copy: %v", err)
	}
	if got, err := store.Read(ctx, "uncertain"); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("repaired remote bytes = %q, %v", got, err)
	}
	mu.Lock()
	obj = objects[key]
	obj.data = []byte("different but self-consistent")
	wrongDigest := sha256.Sum256(obj.data)
	obj.digest = hex.EncodeToString(wrongDigest[:])
	objects[key] = obj
	mu.Unlock()
	if err := store.Repair(ctx, "uncertain", payload); err == nil {
		t.Fatal("ordinary repair overwrote a valid conflicting operation")
	}
	if err := store.RepairCommitted(ctx, "uncertain", payload, digest); err != nil {
		t.Fatalf("committed digest did not authorize repair: %v", err)
	}
	if got, err := store.Read(ctx, "uncertain"); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("committed remote repair = %q, %v", got, err)
	}
}

func TestS3StageStoreRejectsBackingWithoutConditionalPuts(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "replicas")
	remote.IgnoreConditionals.Store(true)
	client, err := s3client.New(s3client.Config{
		Endpoint: remote.URL, AccessKey: s3clienttest.AccessKey,
		SecretKey: s3clienttest.SecretKey, PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewS3StageStore(client, "replicas", "cluster/node-a/", 1024)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	payload := []byte("verified object bytes")
	if err := store.Stage(ctx, "write-1", payload, sha256.Sum256(payload)); err != nil {
		t.Fatalf("memory acceptance: %v", err)
	}
	if err := store.Persist(ctx, "write-1"); !errors.Is(err, ErrS3ConditionalsRequired) {
		t.Fatalf("durable acceptance without conditional writes = %v", err)
	}
}
