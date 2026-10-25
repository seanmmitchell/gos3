package admin

import (
	"net/http"

	"github.com/seanmmitchell/gos3/internal/updates"
)

// Any authenticated UI user may read this non-sensitive cached result.
func (a *api) updateStatus(rc *reqCtx) (int, any, error) {
	if a.d.Updates == nil {
		return http.StatusOK, updates.Status{NotifyFor: []string{}}, nil
	}
	return http.StatusOK, a.d.Updates.Status(), nil
}
