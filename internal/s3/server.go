// Package s3 implements the S3 REST API HTTP layer: request routing (path- and
// virtual-hosted-style), operation dispatch, authentication/authorization
// middleware, CORS, XML encoding, checksums and all S3 operation handlers on
// top of a storage.Backend.
//
// CONTRACT: Options and NewHandler are fixed (the server wiring codes against
// them). Everything else in this package is owned by the s3 work items.
package s3

import (
	"log/slog"
	"net/http"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Options configures the S3 handler.
type Options struct {
	Backend  storage.Backend
	Verifier *auth.Verifier
	IAM      *iam.Manager
	Metrics  *metrics.Registry // may be nil
	Settings *config.Store     // region, domains (virtual host), global CORS
	Logger   *slog.Logger
	// MaxObjectSize bounds single PUT uploads (default 5 GiB).
	MaxObjectSize int64
	// Version is the gos3 version reported in the Server header.
	Version string
}

// NewHandler returns the S3 API http.Handler. It must not handle the reserved
// /_gos3/ path prefix (the server wiring routes that elsewhere for non
// virtual-host requests). CONTRACT STUB — replaced by the s3-core work item.
func NewHandler(opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
}
