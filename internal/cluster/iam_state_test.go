package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
)

func TestTwoNodeSharedIAM(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	open := func(name string) *iam.Manager {
		m, err := iam.Open(iam.Options{
			RootAccessKey: "cluster-root", RootSecretKey: "cluster-root-secret",
			SharedState: IAMStateStore{Node: nodes[name]},
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a, b := open("a"), open("b")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := a.CreateUser(ctx, iam.UserSpec{Name: "alice", Policies: []string{iam.PolicyReadWrite}}); err != nil {
		t.Fatal(err)
	}
	if user, err := b.GetUser(ctx, "alice"); err != nil || user.Name != "alice" {
		t.Fatalf("follower IAM read: %+v, %v", user, err)
	}
	key, err := b.CreateAccessKey(ctx, "alice", "", "alice-key", "a-long-enough-secret-key")
	if err != nil {
		t.Fatal(err)
	}
	if secret, identity, err := a.LookupCredentials(key.AccessKey); err != nil || secret != key.SecretKey || identity.User != "alice" {
		t.Fatalf("cross-node access key: %q, %+v, %v", secret, identity, err)
	}
	role := iam.ReplicationRole{ARN: "arn:aws:iam::123456789012:role/copy", Grants: []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{iam.ReplicateObjectAction}}}}
	if err := b.PutReplicationRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if got, err := a.GetReplicationRole(ctx, role.ARN); err != nil || got.ARN != role.ARN {
		t.Fatalf("cross-node replication role: %+v, %v", got, err)
	}
	if err := a.UpdateAccessKey(ctx, key.AccessKey, iam.KeyInactive); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.LookupCredentials(key.AccessKey); err == nil {
		t.Fatal("revoked credential remained usable on the peer")
	}
	for name, node := range nodes {
		if _, found, err := node.GetMetadata(ctx, MetadataIAM, sharedIAMKey); err != nil || !found {
			t.Fatalf("%s missing committed IAM state: %v, %v", name, found, err)
		}
	}
}

func TestTwoNodeSharedSettings(t *testing.T) {
	nodes := newTwoNodeCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	initial := config.SettingsFrom(config.Default())
	a, err := config.NewSharedStore(initial, SettingsStateStore{Node: nodes["a"]})
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.NewSharedStore(initial, SettingsStateStore{Node: nodes["b"]})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Update(func(s *config.Settings) error { s.Name = "cluster-shared"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := a.Refresh(ctx); err != nil || a.Get().Name != "cluster-shared" {
		t.Fatalf("settings through other entry node: %q, %v", a.Get().Name, err)
	}
	for name, node := range nodes {
		if _, found, err := node.GetMetadata(ctx, MetadataSettings, sharedSettingsKey); err != nil || !found {
			t.Fatalf("%s missing committed settings: %v, %v", name, found, err)
		}
	}
}
