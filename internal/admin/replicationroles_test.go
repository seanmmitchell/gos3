package admin

import (
	"net/http"
	"testing"

	"github.com/seanmmitchell/gos3/internal/iam"
)

func TestReplicationRoleAdminLifecycle(t *testing.T) {
	e := newEnv(t)
	path := Prefix + "/replication-roles/backup"
	role := map[string]any{"grants": []iam.ReplicationGrant{{SourceBucket: "source", DestinationBucket: "target", Prefix: "photos/", Actions: []string{iam.ReplicateObjectAction, iam.ReplicateTagsAction}}}}
	e.do(nil, http.MethodGet, Prefix+"/replication-roles", nil).expectError(t, http.StatusUnauthorized, codeUnauthenticated)
	e.do(&rootCreds, http.MethodPut, path, role).expectStatus(t, http.StatusOK)
	res := e.do(&rootCreds, http.MethodGet, path, nil)
	res.expectStatus(t, http.StatusOK)
	var got iam.ReplicationRole
	res.json(t, &got)
	if got.ARN != "arn:aws:iam::000000000000:role/backup" || len(got.Grants) != 1 {
		t.Fatalf("role = %+v", got)
	}
	res = e.do(&rootCreds, http.MethodGet, Prefix+"/replication-roles", nil)
	res.expectStatus(t, http.StatusOK)
	var listed struct {
		Roles []iam.ReplicationRole `json:"roles"`
	}
	res.json(t, &listed)
	if len(listed.Roles) != 1 || listed.Roles[0].ARN != got.ARN {
		t.Fatalf("roles = %+v", listed.Roles)
	}
	e.do(&rootCreds, http.MethodPut, Prefix+"/replication-roles/bad!", role).expectError(t, http.StatusBadRequest, codeInvalidRequest)
	e.do(&rootCreds, http.MethodDelete, path, nil).expectStatus(t, http.StatusNoContent)
	e.do(&rootCreds, http.MethodGet, path, nil).expectError(t, http.StatusNotFound, codeNotFound)
}
