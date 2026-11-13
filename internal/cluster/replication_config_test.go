package cluster

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestClusterReplicationRulesAndActivationCommitTogether(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	backend := NewS3Backend(nodes["a"])
	if err := backend.CreateBucket(ctx, "source", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := backend.SetBucketVersioning(ctx, "source", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	rules := []byte(`<ReplicationConfiguration><Role>role</Role></ReplicationConfiguration>`)
	state := []byte(`{"hash":"one"}`)
	if err := backend.PutReplicationConfiguration(ctx, "source", rules, state); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		for key, want := range map[string][]byte{storage.ConfigReplication: rules, storage.ConfigReplicationState: state} {
			got, found, err := node.GetMetadata(ctx, MetadataBuckets, bucketConfigKey("source", key))
			if err != nil || !found || !bytes.Equal(got, want) {
				t.Fatalf("voter %s configuration %s: %q, %t, %v", node.cfg.NodeID, key, got, found, err)
			}
		}
	}
	if err := backend.SetBucketVersioning(ctx, "source", storage.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	if err := backend.PutReplicationConfiguration(ctx, "source", []byte("new rules"), []byte("new state")); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatalf("rule update on suspended bucket: %v", err)
	}
	if got, err := backend.GetBucketConfig(ctx, "source", storage.ConfigReplication); err != nil || !bytes.Equal(got, rules) {
		t.Fatalf("suspended update changed rules: %q, %v", got, err)
	}
	if err := NewS3Backend(nodes["b"]).DeleteReplicationConfiguration(ctx, "source"); err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		for _, key := range []string{storage.ConfigReplication, storage.ConfigReplicationState} {
			if got, found, err := node.GetMetadata(ctx, MetadataBuckets, bucketConfigKey("source", key)); err != nil || found || got != nil {
				t.Fatalf("voter %s retained %s: %q, %t, %v", node.cfg.NodeID, key, got, found, err)
			}
		}
	}
}

func TestIdenticalReplicationConfigAcrossVotersPreservesActivation(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	a, b := NewS3Backend(nodes["a"]), NewS3Backend(nodes["b"])
	if err := a.CreateBucket(ctx, "source", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetBucketVersioning(ctx, "source", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	rules := []byte("identical rules")
	first := []byte(`{"hash":"same","activatedAt":"2026-11-01T00:00:00Z"}`)
	later := []byte(`{"hash":"same","activatedAt":"2026-11-02T00:00:00Z"}`)
	if err := a.PutReplicationConfiguration(ctx, "source", rules, first); err != nil {
		t.Fatal(err)
	}
	if err := b.PutReplicationConfiguration(ctx, "source", rules, later); err != nil {
		t.Fatal(err)
	}
	if got, err := a.GetBucketConfig(ctx, "source", storage.ConfigReplicationState); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("identical update reset activation: %s, %v", got, err)
	}
	if err := b.PutReplicationConfiguration(ctx, "source", []byte("changed rules"), later); err != nil {
		t.Fatal(err)
	}
	if got, err := a.GetBucketConfig(ctx, "source", storage.ConfigReplicationState); err != nil || !bytes.Equal(got, later) {
		t.Fatalf("changed rules did not advance activation: %s, %v", got, err)
	}
}
