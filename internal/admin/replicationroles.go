package admin

import (
	"net/http"

	"github.com/seanmmitchell/gos3/internal/iam"
)

func localReplicationRoleARN(name string) string {
	return "arn:aws:iam::000000000000:role/" + name
}

func (a *api) listReplicationRoles(rc *reqCtx) (int, any, error) {
	roles, err := a.d.IAM.ListReplicationRoles(rc.ctx)
	if err != nil {
		return 0, nil, err
	}
	if roles == nil {
		roles = []iam.ReplicationRole{}
	}
	return http.StatusOK, map[string]any{"roles": roles}, nil
}

func (a *api) getReplicationRole(rc *reqCtx) (int, any, error) {
	role, err := a.d.IAM.GetReplicationRole(rc.ctx, localReplicationRoleARN(rc.r.PathValue("name")))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, role, nil
}

func (a *api) putReplicationRole(rc *reqCtx) (int, any, error) {
	var body struct {
		Grants []iam.ReplicationGrant `json:"grants"`
	}
	if err := rc.decodeBody(&body, false); err != nil {
		return 0, nil, err
	}
	role := iam.ReplicationRole{ARN: localReplicationRoleARN(rc.r.PathValue("name")), Grants: body.Grants}
	if err := a.d.IAM.PutReplicationRole(rc.ctx, role); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: replication role saved", "arn", role.ARN, "by", rc.id.User)
	return http.StatusOK, role, nil
}

func (a *api) deleteReplicationRole(rc *reqCtx) (int, any, error) {
	arn := localReplicationRoleARN(rc.r.PathValue("name"))
	if err := a.d.IAM.DeleteReplicationRole(rc.ctx, arn); err != nil {
		return 0, nil, err
	}
	a.log.Info("admin: replication role deleted", "arn", arn, "by", rc.id.User)
	return http.StatusNoContent, nil, nil
}
