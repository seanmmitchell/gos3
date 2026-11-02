package admin

import (
	"context"
	"testing"

	"github.com/seanmmitchell/gos3/internal/cluster"
	"github.com/seanmmitchell/gos3/internal/iam"
)

func TestClusterStatusAuthorizationAndStandalone(t *testing.T) {
	standalone := newEnv(t)
	standalone.root("GET", "/cluster", nil).expectError(t, 404, codeNotFound)
	e := newEnv(t, func(d *Deps) {
		d.ClusterStatus = func(context.Context) cluster.Status {
			return cluster.Status{ClusterID: "c1", NodeID: "a", Available: true, MinMemoryAcks: 2, MinDurableAcks: 2, LastRepair: cluster.ReplicaRepairStatus{Repaired: 3}}
		}
	})
	e.do(nil, "GET", Prefix+"/cluster", nil).expectError(t, 401, codeUnauthenticated)
	reader := e.addUser("reader", iam.PolicyReadOnly)
	e.do(&reader, "GET", Prefix+"/cluster", nil).expectError(t, 403, codeAccessDenied)
	diagnostics := e.addUser("diagnostics", iam.PolicyDiagnostics)
	r := e.do(&diagnostics, "GET", Prefix+"/cluster", nil)
	r.expectStatus(t, 200)
	var status cluster.Status
	r.json(t, &status)
	if status.ClusterID != "c1" || status.NodeID != "a" || !status.Available || status.MinDurableAcks != 2 || status.LastRepair.Repaired != 3 {
		t.Fatalf("cluster status: %+v", status)
	}
}
