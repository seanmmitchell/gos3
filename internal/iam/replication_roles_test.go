package iam

import (
	"context"
	"errors"
	"testing"
)

func TestReplicationRolesPersistAndLimitCopies(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	role := ReplicationRole{ARN: "arn:aws:iam::000000000000:role/replication", Grants: []ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Prefix: "images/", Actions: []string{ReplicateObjectAction, ReplicateTagsAction}}}}
	if err := m.PutReplicationRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	role.Grants[0].Actions[0] = ReplicateDeleteAction
	if !m.CanReplicate(ctx, role.ARN, "source", "target", "images/cat.jpg", ReplicateObjectAction, ReplicateTagsAction) {
		t.Fatal("allowed object and tag copy denied")
	}
	for _, tc := range []struct {
		source, target, key string
		actions             []string
	}{
		{"source", "target", "images/cat.jpg", []string{ReplicateDeleteAction}},
		{"source", "target", "private/cat.jpg", []string{ReplicateObjectAction}},
		{"other", "target", "images/cat.jpg", []string{ReplicateObjectAction}},
		{"source", "other", "images/cat.jpg", []string{ReplicateObjectAction}},
	} {
		if m.CanReplicate(ctx, role.ARN, tc.source, tc.target, tc.key, tc.actions...) {
			t.Fatalf("overbroad replication grant: %+v", tc)
		}
	}
	m, err = Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !m.CanReplicate(ctx, role.ARN, "source", "target", "images/cat.jpg", ReplicateObjectAction, ReplicateTagsAction) {
		t.Fatal("role lost after restart")
	}
	got, err := m.GetReplicationRole(ctx, role.ARN)
	if err != nil {
		t.Fatal(err)
	}
	got.Grants[0].Actions[0] = ReplicateDeleteAction
	if !m.CanReplicate(ctx, role.ARN, "source", "target", "images/cat.jpg", ReplicateObjectAction) {
		t.Fatal("GetReplicationRole exposed mutable state")
	}
	if err := m.DeleteReplicationRole(ctx, role.ARN); err != nil {
		t.Fatal(err)
	}
	if m.CanReplicate(ctx, role.ARN, "source", "target", "images/cat.jpg", ReplicateObjectAction) {
		t.Fatal("deleted role still authorizes")
	}
	if _, err := m.GetReplicationRole(ctx, role.ARN); !errors.Is(err, ErrReplicationRoleNotFound) {
		t.Fatalf("missing role = %v", err)
	}
}

func TestReplicationRoleRejectsInvalidGrants(t *testing.T) {
	m, err := Open(Options{})
	if err != nil {
		t.Fatal(err)
	}
	valid := ReplicationRole{ARN: "arn:aws:iam::000000000000:role/r", Grants: []ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Actions: []string{ReplicateObjectAction}}}}
	for _, edit := range []func(*ReplicationRole){
		func(r *ReplicationRole) { r.ARN = "arn:aws:iam::0:role/r" },
		func(r *ReplicationRole) { r.Grants[0].DestinationBucket = "source" },
		func(r *ReplicationRole) { r.Grants[0].Actions = []string{"s3:*"} },
		func(r *ReplicationRole) { r.Grants[0].Actions = []string{ReplicateObjectAction, ReplicateObjectAction} },
	} {
		role := *cloneReplicationRole(valid)
		edit(&role)
		if err := m.PutReplicationRole(context.Background(), role); !errors.Is(err, ErrInvalidReplicationRole) {
			t.Fatalf("invalid role accepted: %+v, %v", role, err)
		}
	}
}
