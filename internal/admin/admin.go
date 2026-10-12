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
	"github.com/seanmmitchell/gos3/internal/clientip"
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
	// TrustProxyHeaders derives the aws:SourceIp and aws:SecureTransport
	// condition keys, honouring X-Forwarded-For / X-Forwarded-Proto from
	// trusted reverse proxies (server.trust_proxy_headers and
	// server.trusted_proxies) exactly like the S3 API, so one policy gives
	// the same answer on both APIs behind a reverse proxy. The zero value
	// ignores those headers.
	TrustProxyHeaders clientip.Resolver
}

// NewHandler returns the admin API handler. It expects full request paths
// (starting with Prefix).
//
// The handler answers CORS preflights (OPTIONS) from the global CORS
// settings without authentication, authenticates every other request with
// d.Verifier (SigV4 header or presigned URL; anonymous requests get 401),
// authorizes it with d.IAM ("admin:<Action>", see docs/admin-api.md) and
// renders JSON responses; errors use the envelope
// {"error":{"code":"…","message":"…"}}.
//
// Backend, Verifier, IAM and Settings are required; NewHandler panics if one
// of them is nil (a wiring bug). Metrics, Discovery, Info and Logger are
// optional.
func NewHandler(d Deps) http.Handler {
	return newAPI(d)
}
