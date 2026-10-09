// Package admin implements the gos3 admin REST API (JSON) mounted at
// /_gos3/admin/v1/ — server info & stats, runtime settings, IAM users / access
// keys / policies, bucket overview & management helpers and discovery. It is
// specified in docs/admin-api.md and consumed by the web UI.
//
// Requests are authenticated with AWS SigV4 (same credentials as S3) and
// authorized with iam actions "admin:<Action>".
//
// CONTRACT: Deps and NewHandler are fixed. Implementation is owned by the
// admin work item (replace the stub, add files freely).
package admin

import (
	"log/slog"
	"net/http"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/discovery"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Prefix is the mount point of the admin API.
const Prefix = "/_gos3/admin/v1"

// Deps are the admin API's dependencies.
type Deps struct {
	Backend   storage.Backend
	Verifier  *auth.Verifier
	IAM       *iam.Manager
	Metrics   *metrics.Registry // may be nil
	Settings  *config.Store
	Discovery *discovery.Service // may be nil when disabled
	// Info returns this server's public identity document.
	Info func() discovery.Info
	// BackendName is "fs" or "memory"; DataPath is the data directory ("" if none).
	BackendName string
	DataPath    string
	Logger      *slog.Logger
}

// NewHandler returns the admin API handler. It expects full request paths
// (starting with Prefix). CONTRACT STUB — replaced by the admin work item.
func NewHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"NotImplemented","message":"admin API not implemented"}}`, http.StatusNotImplemented)
	})
}
