package admin

import (
	"net/http"
)

func (a *api) clusterStatus(rc *reqCtx) (int, any, error) {
	if a.d.ClusterStatus == nil {
		return 0, nil, notFound("cluster mode is not enabled")
	}
	return http.StatusOK, a.d.ClusterStatus(rc.ctx), nil
}
