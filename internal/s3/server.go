// Package s3 implements the S3 REST API HTTP layer: request routing (path- and
// virtual-hosted-style), operation dispatch, authentication/authorization
// middleware, CORS, XML encoding, checksums and all S3 operation handlers on
// top of a storage.Backend.
//
// CONTRACT: Options and NewHandler are fixed (the server wiring codes against
// them). Everything else in this package is owned by the s3 work items.
package s3

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/clientip"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/metrics"
	"github.com/seanmmitchell/gos3/internal/s3err"
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

	// ClientIP derives the client address (aws:SourceIp, access log) and
	// aws:SecureTransport, honouring X-Forwarded-For / X-Forwarded-Proto
	// from trusted reverse proxies (server.trust_proxy_headers and
	// server.trusted_proxies). The zero value ignores those headers.
	ClientIP clientip.Resolver
	// Now overrides the clock (tests). Defaults to time.Now.
	Now func() time.Time
}

// DefaultMaxObjectSize is the default single PUT limit (5 GiB, as in AWS).
const DefaultMaxObjectSize int64 = 5 << 30

// defaultRegion is used when the settings carry no region.
const defaultRegion = "us-east-1"

// Server is the S3 API handler returned by NewHandler. It is safe for
// concurrent use.
type Server struct {
	backend       storage.Backend
	verifier      *auth.Verifier
	iam           *iam.Manager
	metrics       *metrics.Registry
	settings      *config.Store
	log           *slog.Logger
	maxObjectSize int64
	serverHeader  string
	clientIP      clientip.Resolver
	now           func() time.Time
	// hostSeed makes x-amz-id-2 values unique per server instance.
	hostSeed [16]byte

	// configs caches parsed bucket configuration documents (policies, CORS,
	// public access block) keyed by bucket + document kind; entries are
	// revalidated against the stored bytes on every use.
	configs *configCache

	// bucketStateLocks serialize the read-check-write sequences that keep
	// related bucket settings consistent (versioning vs. object lock and
	// replication, bucket ACL vs. object ownership, policy vs. public
	// access block). Buckets hash onto a fixed set of mutexes. They belong
	// to the server, not the package: a handler may hold one while the
	// backend calls a remote S3 service, which in tests is another Server
	// in the same process.
	bucketStateLocks [64]sync.Mutex

	// handlerOverride lets tests replace operation handlers; nil in
	// production.
	handlerOverride func(op string) handlerFunc

	// Request body timeouts (see countingBody); tests shorten them.
	bodyIdleTimeout     time.Duration // a stalled body read fails with RequestTimeout
	unreadBodyGrace     time.Duration // draining a body the handler did not read
	postPreambleTimeout time.Duration // PostObject form fields before the policy is verified
	preAuthBodyTimeout  time.Duration // bodies read before authorization (AuthzByHandler ops)
}

// Default request body timeouts. AWS answers RequestTimeout after about
// 20 s without data; gos3 is more tolerant of slow links. The unread rest
// of a rejected request's body and PostObject's form fields (its client is
// not authenticated until they have been read) must arrive within a fixed
// time.
//
// The same holds for the bodies read before the caller is authorized
// (DeleteObjects, PutObjectTagging, PutObjectRetention,
// PutObjectLegalHold): at most maxDeleteObjectsBody (about 6.2 MiB, a
// request of 1000 maximal keys) or 64 KiB. 60 s allows such a maximal
// DeleteObjects at about 110 KB/s (under 1 Mbit/s); typical bodies are a
// few KiB to about 100 KiB.
const (
	defaultBodyIdleTimeout     = 60 * time.Second
	defaultUnreadBodyGrace     = 10 * time.Second
	defaultPostPreambleTimeout = 60 * time.Second
	defaultPreAuthBodyTimeout  = 60 * time.Second
)

var _ http.Handler = (*Server)(nil)

// NewHandler returns the S3 API http.Handler (dynamic type *Server). It must
// not handle the reserved /_gos3/ path prefix (the server wiring routes that
// elsewhere for non virtual-host requests).
func NewHandler(opts Options) http.Handler { return New(opts) }

// New is NewHandler with the concrete return type.
//
// Missing options get safe defaults: a nil Settings store means region
// us-east-1, no virtual-host domains and no global CORS; a nil Logger
// discards logs; a nil Verifier is created from IAM (or rejects every
// signed request when IAM is nil too); with a nil IAM only the root identity
// is authorized. Backend is required.
func New(opts Options) *Server {
	if opts.Backend == nil {
		panic("s3: Options.Backend is required")
	}
	s := &Server{
		backend:       opts.Backend,
		verifier:      opts.Verifier,
		iam:           opts.IAM,
		metrics:       opts.Metrics,
		settings:      opts.Settings,
		log:           opts.Logger,
		maxObjectSize: opts.MaxObjectSize,
		serverHeader:  "gos3",
		clientIP:      opts.ClientIP,
		now:           opts.Now,
		configs:       newConfigCache(defaultConfigCacheSize),

		bodyIdleTimeout:     defaultBodyIdleTimeout,
		unreadBodyGrace:     defaultUnreadBodyGrace,
		postPreambleTimeout: defaultPostPreambleTimeout,
		preAuthBodyTimeout:  defaultPreAuthBodyTimeout,
	}
	if s.settings == nil {
		s.settings = config.NewStore(config.Settings{Region: defaultRegion}, "")
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.maxObjectSize <= 0 {
		s.maxObjectSize = DefaultMaxObjectSize
	}
	if s.now == nil {
		s.now = time.Now
	}
	if opts.Version != "" {
		s.serverHeader = "gos3/" + opts.Version
	}
	if s.verifier == nil {
		var creds auth.CredentialProvider = noCredentials{}
		if s.iam != nil {
			creds = s.iam
		}
		s.verifier = auth.NewVerifier(creds, auth.Options{Region: s.region()})
	}
	_, _ = rand.Read(s.hostSeed[:])
	return s
}

// noCredentials rejects every access key (used when neither a Verifier nor
// an IAM manager is configured).
type noCredentials struct{}

func (noCredentials) LookupCredentials(string) (string, auth.Identity, error) {
	return "", auth.Identity{}, s3err.ErrInvalidAccessKeyID
}

// region returns the configured server region.
func (s *Server) region() string {
	if r := s.settings.Get().Region; r != "" {
		return r
	}
	return defaultRegion
}

// MaxObjectSize returns the single PUT size limit in bytes.
func (s *Server) MaxObjectSize() int64 { return s.maxObjectSize }

// hostID returns the x-amz-id-2 value for a request ID: an opaque base64
// token like AWS's extended request ID.
func (s *Server) hostID(requestID string) string {
	h := sha256.New()
	h.Write(s.hostSeed[:])
	h.Write([]byte(requestID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
