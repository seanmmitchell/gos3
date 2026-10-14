// Package s3store implements the "s3" storage backend: a storage.Backend
// that keeps everything — bucket records, configuration documents, object
// versions and delete markers, multipart uploads and object data — in a
// remote S3 bucket under a prefix, so a backing needs no local directory
// (see docs/backings.md).
//
// CONTRACT: the exported names in this file are used by the server, the
// multi router and the gateway variant; the implementation lives in the
// other files of the package.
//
// Metadata is held in an in-memory index (internal/storage/index) loaded
// at startup from a snapshot plus a journal stored under the prefix; every
// mutation is acknowledged only after its journal batch has been written.
// One process owns a prefix at a time: a lease and epoch seals fence out a
// second one. Object data lives in immutable blobs. AES256 master keys are
// stored wrapped by a key-encryption key (Options.KEK); restic repositories
// of the backing's buckets live under the prefix too.
package s3store

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Options configure Open.
type Options struct {
	// Name is the backing name (logs, errors).
	Name string
	// Client talks to the remote service. Required.
	Client *s3client.Client
	// Endpoint, Region, AccessKey, SecretKey and PathStyle describe the
	// service for programs that cannot use Client (restic's s3 backend).
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
	// Bucket and Prefix locate the backing in the remote service. Prefix is
	// "" or ends with "/".
	Bucket string
	Prefix string
	// PartSize is the multipart part size for uploads (0 = default).
	PartSize int64
	// KEK is the 32-byte key-encryption key wrapping the per-bucket master
	// keys. Without it AES256 and restic are not offered.
	KEK []byte
	// Gateway opens the backing as the metadata store of a gateway backing:
	// object and multipart operations answer storage.ErrNotSupported (the
	// gateway's data store handles them) and AuxState is available.
	Gateway bool
	// TempDir holds spool and restic temporary files ("" = os.TempDir()).
	TempDir string
	// LeaseInterval and LeaseTTL tune ownership fencing (0 = defaults).
	LeaseInterval time.Duration
	LeaseTTL      time.Duration
	// OrphanRetention is how old an unreferenced blob must be before the
	// recovery pass deletes it (0 = DefaultOrphanRetention).
	OrphanRetention time.Duration
	Logger          *slog.Logger

	// StallTimeout bounds how long a journal write may keep failing before
	// the backing stops serving and reloads (0 = DefaultJournalStallTimeout).
	StallTimeout time.Duration
	// SnapshotPolicy sets the metadata snapshot cadence (zero = defaults).
	SnapshotPolicy SnapshotPolicy
	// RecoveryInterval is how often the orphan recovery pass runs
	// (0 = DefaultRecoveryInterval, negative = never).
	RecoveryInterval time.Duration

	// retryMin is the first delay between attempts to load the backing
	// (tests; 0 = 2s).
	retryMin time.Duration
}

// DefaultOrphanRetention is the default of Options.OrphanRetention.
const DefaultOrphanRetention = 24 * time.Hour

// DefaultRecoveryInterval is the default of Options.RecoveryInterval.
const DefaultRecoveryInterval = time.Hour

// Errors returned (wrapped) by the backend.
var (
	// ErrClosed is returned after Close; it wraps
	// storage.ErrBackingUnavailable (the request may be retried elsewhere).
	ErrClosed = fmt.Errorf("%w: s3store: backend closed", storage.ErrBackingUnavailable)
	// ErrFenced means another process took over the backing's prefix; the
	// backend stops serving (wrapped together with
	// storage.ErrBackingUnavailable).
	ErrFenced = errors.New("s3store: prefix owned by another server")
	// ErrInUse means another live server owns the prefix (lease).
	ErrInUse = errors.New("s3store: prefix in use by another server")
	// ErrWrongKEK means Options.KEK does not match the key the backing's
	// master keys are wrapped with.
	ErrWrongKEK = errors.New("s3store: encryption key does not match this backing")
	// ErrNoConditionalWrites means the remote service ignores
	// If-None-Match/If-Match on PUT, which the backend requires.
	ErrNoConditionalWrites = errors.New("s3store: the S3 service does not support conditional writes")
)

// Connection is the part of the options that can change on a live backing
// without changing which data it holds (see Backend.PrepareConnection).
type Connection struct {
	Client    *s3client.Client
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
	PartSize  int64
	KEK       []byte
}
