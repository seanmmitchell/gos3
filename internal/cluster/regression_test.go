package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestAdmissionReleasesFailedStagesBeforeRetry(t *testing.T) {
	stores := make([]*StageStore, 2)
	for i := range stores {
		s, err := OpenStageStore(filepath.Join(t.TempDir(), "stage.db"), 4)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
		defer s.Close()
	}
	selected := []string{"a", "b"}
	policy := AckPolicy{Memory: 2, Durable: 2}
	publish := func(context.Context) error { return nil }
	err := AdmitWrite(t.Context(), "failed", []byte("data"), selected, policy, map[string]ReplicaStage{
		"a": failingReplica{stage: stores[0], stageOK: true},
		"b": failingReplica{stage: stores[1], stageOK: true},
	}, publish)
	if !errors.Is(err, ErrInsufficientAcks) {
		t.Fatalf("expected failed durable writes: %v", err)
	}
	if err := AdmitWrite(t.Context(), "retry", []byte("data"), selected, policy, map[string]ReplicaStage{
		"a": stores[0], "b": stores[1],
	}, publish); err != nil {
		t.Fatalf("recovered replicas rejected a new operation: %v", err)
	}
	for _, s := range stores {
		if data, err := s.Read(t.Context(), "retry"); err != nil || string(data) != "data" {
			t.Fatalf("cleanup removed durable data: %q, %v", data, err)
		}
	}
}

type stalledPayloadStore struct{ DurableReplicaStore }

func (s stalledPayloadStore) Read(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s stalledPayloadStore) RepairCommitted(ctx context.Context, _ string, _ []byte, _ [32]byte) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestReadVerifiedPayloadUsesHealthyReplicaWhenAnotherStalls(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	backend := NewS3Backend(nodes["a"])
	if err := backend.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	writer, err := backend.NewObjectWriter(ctx, "photos", "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "payload"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Commit(ctx, storage.ObjectMeta{}, storage.CommitOptions{}); err != nil {
		t.Fatal(err)
	}
	selected, err := SelectReplicas(nodes["a"].cfg.ID, "photos", "key", []string{"a", "b"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	first := nodes[selected[0]]
	original := first.payloads
	first.payloads = stalledPayloadStore{original}
	first.transport.stage = first.payloads
	for _, reader := range []string{selected[1], selected[0]} {
		t.Run(reader, func(t *testing.T) {
			b := NewS3Backend(nodes[reader])
			b.operationTimeout = time.Second
			_, body, err := b.OpenObject(ctx, "photos", "key", "")
			if err != nil {
				t.Fatalf("healthy replica should satisfy read: %v", err)
			}
			defer body.Close()
			data, err := io.ReadAll(body)
			if err != nil || string(data) != "payload" {
				t.Fatalf("body = %q, %v", data, err)
			}
		})
	}
}

func TestAdmissionReleasesRemoteStages(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	for _, node := range nodes {
		node.stages.maxUsed = 4
	}
	a, err := nodes["a"].transport.Replica("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := nodes["a"].transport.Replica("b")
	if err != nil {
		t.Fatal(err)
	}
	err = AdmitWrite(t.Context(), "failed", []byte("data"), []string{"a", "b"}, AckPolicy{Memory: 2, Durable: 2}, map[string]ReplicaStage{
		"a": failingReplica{stage: a, stageOK: true}, "b": failingReplica{stage: b, stageOK: true},
	}, func(context.Context) error { t.Fatal("published without durable acks"); return nil })
	if !errors.Is(err, ErrInsufficientAcks) {
		t.Fatalf("expected failed admission: %v", err)
	}
	for _, store := range []ReplicaStage{a, b} {
		if err := store.Stage(t.Context(), "retry", []byte("data"), sha256.Sum256([]byte("data"))); err != nil {
			t.Fatalf("remote cleanup did not free staging: %v", err)
		}
	}
}

func TestStageLeasesReclaimAbandonedMemory(t *testing.T) {
	for _, kind := range []string{"filesystem", "s3"} {
		t.Run(kind, func(t *testing.T) {
			var store DurableReplicaStore
			var expire func()
			if kind == "filesystem" {
				s, err := OpenStageStore(filepath.Join(t.TempDir(), "stage.db"), 4)
				if err != nil {
					t.Fatal(err)
				}
				store = s
				expire = func() {
					s.mu.Lock()
					defer s.mu.Unlock()
					staged := s.memory["orphan"]
					staged.expiresAt = time.Now().Add(-time.Second)
					s.memory["orphan"] = staged
				}
			} else {
				// Stage and Release must work while the S3 backing is offline.
				client, err := s3client.New(s3client.Config{Endpoint: "https://unused.invalid", AccessKey: "a", SecretKey: "s", PathStyle: true})
				if err != nil {
					t.Fatal(err)
				}
				s, err := NewS3StageStore(client, "replicas", "node/", 4)
				if err != nil {
					t.Fatal(err)
				}
				store = s
				expire = func() {
					s.mu.Lock()
					defer s.mu.Unlock()
					staged := s.memory["orphan"]
					staged.expiresAt = time.Now().Add(-time.Second)
					s.memory["orphan"] = staged
				}
			}
			defer store.Close()
			payload := []byte("data")
			digest := sha256.Sum256(payload)
			if err := store.Stage(t.Context(), "orphan", payload, digest); err != nil {
				t.Fatal(err)
			}
			if err := store.Stage(t.Context(), "retry", payload, digest); !errors.Is(err, ErrStageFull) {
				t.Fatalf("unexpired stage was discarded: %v", err)
			}
			expire()
			if err := store.Stage(t.Context(), "retry", payload, digest); err != nil {
				t.Fatalf("expired stage retained memory: %v", err)
			}
			if err := store.Release(t.Context(), "retry"); err != nil {
				t.Fatal(err)
			}
			if err := store.Stage(t.Context(), "after-release", payload, digest); err != nil {
				t.Fatalf("released stage retained memory: %v", err)
			}
		})
	}
}

func TestAdmissionRetainsMemoryAcksUntilPublication(t *testing.T) {
	replicas := make(map[string]ReplicaStage)
	var volatile *StageStore
	for _, name := range []string{"a", "b", "c"} {
		s, err := OpenStageStore(filepath.Join(t.TempDir(), "stage.db"), 4)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		replicas[name] = s
		if name == "c" {
			volatile = s
			replicas[name] = waitingPersistReplica{s}
		}
	}
	if err := AdmitWrite(t.Context(), "memory-only", []byte("data"), []string{"a", "b", "c"}, AckPolicy{Memory: 3, Durable: 2}, replicas, func(context.Context) error {
		volatile.mu.Lock()
		defer volatile.mu.Unlock()
		if volatile.used != 4 {
			t.Errorf("memory ack was released before publication: %d", volatile.used)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	volatile.mu.Lock()
	defer volatile.mu.Unlock()
	if volatile.used != 0 {
		t.Errorf("memory ack was retained after publication: %d", volatile.used)
	}
}

type waitingPersistReplica struct{ ReplicaStage }

func (r waitingPersistReplica) Persist(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestMetadataTransactionCanReplaceMaximumSizeRecord(t *testing.T) {
	old, next := []byte(strings.Repeat("a", 1<<20)), []byte(strings.Repeat("b", 1<<20))
	transaction := MetadataTransaction{OperationID: "00112233445566778899aabbccddeeff",
		Conditions: []MetadataCondition{{Namespace: MetadataIAM, Key: sharedIAMKey, Exists: true, Value: old}},
		Changes:    []MetadataCommand{{Namespace: MetadataIAM, Key: sharedIAMKey, Value: next}},
	}
	encoded, err := EncodeMetadataTransaction(transaction)
	if err != nil {
		t.Fatalf("cannot encode two permitted records: %v", err)
	}
	if _, found, err := decodeMetadataTransaction(encoded); err != nil || !found {
		t.Fatalf("cannot decode maximum record replacement: %v", err)
	}
	transaction.Changes = append(transaction.Changes, MetadataCommand{Namespace: MetadataIAM, Key: "other", Value: next})
	if _, err := EncodeMetadataTransaction(transaction); !errors.Is(err, errInvalidMetadataCommand) {
		t.Fatalf("oversized transaction accepted: %v", err)
	}
}

func TestImportedLargeIAMDocumentRemainsMutable(t *testing.T) {
	users := make([]iam.User, 800)
	for i := range users {
		users[i] = iam.User{Name: fmt.Sprintf("user%d", i), Description: strings.Repeat("x", 1024),
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	}
	document, err := json.Marshal(struct {
		Version  int               `json:"version"`
		Users    []iam.User        `json:"users"`
		Policies []iam.NamedPolicy `json:"policies"`
	}{1, users, nil})
	if err != nil {
		t.Fatal(err)
	}
	source, settings := importFixture(t)
	if _, err := VerifyStandalone(t.Context(), source, settings, document, "ROOTTESTKEY", 1024); err != nil {
		t.Fatalf("valid source must pass import preflight: %v", err)
	}
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	// Install the exact IAM record that buildImportImage restores at import.
	if err := nodes["a"].AdmitMetadataWrite(ctx, [16]byte{255}, "system", "iam", document,
		MetadataCommand{Namespace: MetadataIAM, Key: sharedIAMKey, Value: document}); err != nil {
		t.Fatal(err)
	}
	identity, err := iam.Open(iam.Options{RootAccessKey: "ROOTTESTKEY", RootSecretKey: "root-test-secret-key",
		SharedState: IAMStateStore{Node: nodes["a"]}})
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	if err := identity.DeleteUser(ctx, "user0"); err != nil {
		t.Fatalf("cannot revoke a user after importing %d bytes of IAM state: %v", len(document), err)
	}
	if _, err := identity.GetUser(ctx, "user0"); !errors.Is(err, iam.ErrUserNotFound) {
		t.Fatalf("deleted user still exists: %v", err)
	}
}
