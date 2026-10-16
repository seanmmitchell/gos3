// Package s3account implements the s3 backing: one storage.Backend per
// connection to an S3 service (endpoint + credentials), holding buckets of
// two kinds (see docs/backings.md, "Bucket kinds"):
//
//   - native buckets map to one remote bucket each (adopted or created by
//     gos3) whose objects are stored as-is; versioning and object lock are
//     passed through to the remote bucket (internal/storage/native);
//   - wrapped buckets are stored in gos3's own format (internal/storage/
//     s3store) inside a container: the backing's home (the shared
//     container), a dedicated remote bucket, or an existing remote bucket
//     under a prefix. A container holds any number of wrapped buckets.
//
// The backing's own records (which gos3 bucket is of which kind and lives
// where, the configuration documents of native buckets, native multipart
// state) live in the home (an s3store aux namespace) or, for a backing
// without a home, in a local state database (internal/storage/localstate).
//
// CONTRACT: the exported names in this file are used by the server, the
// admin API and the multi router; the implementation lives in the other
// files of the package.
package s3account

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// Location is a remote bucket and a key prefix in it ("" or ending in
// "/").
type Location struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
}

// String returns "s3://bucket/prefix".
func (l Location) String() string { return "s3://" + l.Bucket + "/" + l.Prefix }

// ClaimRole is what a backing uses a remote location for.
type ClaimRole string

const (
	RoleHome      ClaimRole = "home"      // the backing's home (records + shared container)
	RoleContainer ClaimRole = "container" // a container of wrapped buckets
	RoleNative    ClaimRole = "native"    // the remote bucket of a native bucket
)

// Claim is one use of a remote location.
type Claim struct {
	Backing string
	// Endpoint is the service the location is on (the backing's endpoint
	// URL); claims on different services never conflict.
	Endpoint string
	Location
	Role ClaimRole
	// Bucket is the gos3 bucket of a native claim.
	GosBucket string
}

// Claims is the server-wide registry of remote locations in use, shared by
// every s3 backing so no two uses overlap: a native bucket's remote bucket
// is used by nothing else, and containers/homes never nest. Locations are
// compared per service (endpoints normalised to lower-case host:port, the
// scheme's default port filled in, as the configuration's overlap check
// does), remote buckets case-insensitively.
type Claims interface {
	// Claim records a use; it fails with an error wrapping
	// storage.ErrInvalidTarget when the use conflicts with another one.
	// release forgets the claim (idempotent).
	Claim(c Claim) (release func(), err error)
	// Holders returns the claims on a remote bucket of the service at
	// endpoint.
	Holders(endpoint, bucket string) []Claim
}

// Options configures Open.
type Options struct {
	// Name is the backing name.
	Name string
	// Conn is the connection: client, credentials (for restic), part size
	// and the key-encryption key of wrapped buckets' AES256 master keys.
	Conn s3store.Connection
	// Home is where the backing keeps its records and its shared
	// container; nil keeps the records in LocalDir and offers no shared
	// container (wrapped buckets then need a dedicated container).
	Home *Location
	// LocalDir holds the local records when Home is nil (required then).
	LocalDir string
	// DefaultKind is the kind of buckets created without a target ("" =
	// wrapped with a home, native without).
	DefaultKind storage.BucketKind
	// TempDir holds spool and restic temporary files.
	TempDir string
	// Claims is the server-wide claims registry (required).
	Claims Claims
	// Logger receives logs (nil discards).
	Logger *slog.Logger
	// Tune, when set, adjusts the options of every s3store the backing
	// opens (tests: lease and snapshot timings). TuneNative does the same
	// for the native store.
	Tune       func(*s3store.Options)
	TuneNative func(*native.Options)
}

// Status is the backing's state for health reports.
type Status struct {
	// Ready is set once the records are loaded (the home is serving, or
	// the local records are open).
	Ready bool
	// Err is why the home cannot be loaded (nil while it can).
	Err error
	// Home is the home store's status (zero without a home).
	Home s3store.Status
	// Containers are the backing's containers (the home first).
	Containers []ContainerInfo
}

// ContainerInfo describes a container of wrapped buckets.
type ContainerInfo struct {
	ID       string    `json:"id"`
	Label    string    `json:"label"`
	Location Location  `json:"location"`
	Region   string    `json:"region,omitempty"`
	Role     string    `json:"role"` // "home", "dedicated" or "existing"
	Default  bool      `json:"default,omitempty"`
	Buckets  int       `json:"buckets"`
	State    string    `json:"state"` // "loading", "ready", "failed"
	Error    string    `json:"error,omitempty"`
	Added    time.Time `json:"added,omitzero"`
}

// ContainerSpec asks AddContainer for a new container.
type ContainerSpec struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix,omitempty"`
	Region string `json:"region,omitempty"`
	// CreateBucket creates the remote bucket (a dedicated container);
	// otherwise it must exist.
	CreateBucket bool `json:"createBucket,omitempty"`
	// Attach adopts a location that already holds a gos3 store (a
	// container detached from another server); otherwise the location must
	// be empty.
	Attach bool   `json:"attach,omitempty"`
	Label  string `json:"label,omitempty"`
}

// RemoteUse is one use of a remote bucket (see RemoteBucket.Uses).
type RemoteUse struct {
	Role        string `json:"role"` // home, container, native, claimed, gos3
	Backing     string `json:"backing,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	Bucket      string `json:"bucket,omitempty"` // gos3 bucket of a native use
	ContainerID string `json:"containerId,omitempty"`
	Created     bool   `json:"created,omitempty"` // native: created by gos3
}

// RemoteBucket is one bucket of the remote service as seen by the backing.
type RemoteBucket struct {
	Name      string    `json:"name"`
	Region    string    `json:"region,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	// Status summarises Uses: "native", "home", "container", "claimed"
	// (used by another backing), "gos3" (holds a gos3 store this server
	// does not use), "inaccessible" or "unmanaged".
	Status string      `json:"status"`
	Uses   []RemoteUse `json:"uses,omitempty"`
	Note   string      `json:"note,omitempty"`
}

// ListRemoteOptions selects a page of RemoteBuckets.
type ListRemoteOptions struct {
	Prefix            string
	ContinuationToken string
	MaxBuckets        int
	// Probe checks unmanaged buckets for gos3 markers (one request each).
	Probe bool
}

// RemoteBucketList is a page of RemoteBuckets. Complete is false when the
// credentials may not list buckets: Buckets then holds the locations the
// backing knows, and others can still be adopted by name.
type RemoteBucketList struct {
	Buckets           []RemoteBucket `json:"buckets"`
	Complete          bool           `json:"complete"`
	ListError         string         `json:"listError,omitempty"`
	ContinuationToken string         `json:"continuationToken,omitempty"`
}

// AccountInfo summarises the backing's configuration.
type AccountInfo struct {
	Home             *Location            `json:"home,omitempty"`
	Records          string               `json:"records"` // "home" or "local"
	DefaultKind      storage.BucketKind   `json:"defaultKind"`
	DefaultContainer string               `json:"defaultContainer,omitempty"`
	Kinds            []storage.BucketKind `json:"kinds"`
	NativeBuckets    int                  `json:"nativeBuckets"`
	WrappedBuckets   int                  `json:"wrappedBuckets"`
}

// RemoteManager is the remote-bucket browser of an s3 backing, used by the
// admin API. Its methods do remote I/O without holding the multi router's
// locks; bucket creation itself goes through the router (CreateBucket with
// a storage.BucketTarget).
type RemoteManager interface {
	// RemoteBuckets lists the remote service's buckets with their uses.
	RemoteBuckets(ctx context.Context, opts ListRemoteOptions) (RemoteBucketList, error)
	// CheckAdoptable checks that a remote bucket can become a native
	// bucket (no claim, no gos3 store in it) and returns its region.
	CheckAdoptable(ctx context.Context, remote string) (region string, err error)
	// CreateRemoteBucket creates an unmanaged remote bucket.
	CreateRemoteBucket(ctx context.Context, name, region string, objectLock bool) error
	// Containers lists the containers (the home first).
	Containers(ctx context.Context) []ContainerInfo
	// AddContainer registers (and, with CreateBucket, creates) a container
	// and waits (bounded) until it is loaded.
	AddContainer(ctx context.Context, spec ContainerSpec) (ContainerInfo, error)
	// RemoveContainer forgets an empty container (deleting nothing).
	RemoveContainer(ctx context.Context, id string) error
	// SetDefaultContainer makes a container the default of new wrapped
	// buckets.
	SetDefaultContainer(ctx context.Context, id string) error
	// Info summarises the backing.
	Info() AccountInfo
}

// NativeStore is what the backing needs from the native store
// (implemented by *native.Store).
type NativeStore interface {
	AttachBucket(ctx context.Context, req native.AttachRequest) (native.RemoteState, error)
	Refresh(ctx context.Context, bucket string) (native.RemoteState, error)
	SetVersioning(ctx context.Context, bucket string, st storage.VersioningStatus) error
	EnableObjectLock(ctx context.Context, bucket string) error
	PutObjectLockConfig(ctx context.Context, bucket string, xml []byte) error
	CheckEmpty(ctx context.Context, bucket string) error
	DetachBucket(ctx context.Context, bucket string, removeRemote bool) error
	Close() error

	objectStore
	storage.ConditionalDeleter
	storage.CheckedDeleter
	storage.ConditionalMetaUpdater
	storage.UploadWalker
}

var _ NativeStore = (*native.Store)(nil)

// objectStore is the object and multipart part of storage.Backend.
type objectStore interface {
	NewObjectWriter(ctx context.Context, bucket, key string) (storage.ObjectWriter, error)
	GetObjectInfo(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, error)
	OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error)
	UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error)
	DeleteObject(ctx context.Context, bucket, key, versionID string) (storage.DeleteResult, error)
	WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(storage.ObjectInfo) error) error
	WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(storage.ObjectInfo) error) error

	CreateMultipartUpload(ctx context.Context, bucket, key string, meta storage.ObjectMeta, initiator string) (storage.UploadInfo, error)
	GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (storage.UploadInfo, error)
	NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (storage.PartWriter, error)
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]storage.PartInfo, error)
	ListMultipartUploads(ctx context.Context, bucket string) ([]storage.UploadInfo, error)
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
}

// Backend-level methods beyond storage.Backend (implemented in backend.go;
// the server and the admin API use them):
//
//	Open(Options) (*Backend, error)
//	    No remote I/O that can fail it: the home loads in the background.
//	(*Backend).Ready() <-chan struct{}        records loaded
//	(*Backend).Status() Status
//	(*Backend).Location() string              home URL or "local:"
//	(*Backend).PrepareConnection(ctx, s3store.Connection) (commit func(), err error)
//	(*Backend).SetDefaultKind(storage.BucketKind) error
//	(*Backend).Close() error
//	RemoteManager, storage.BucketCapabilities, storage.BucketReleaser,
//	storage.HealthProber, storage.KindReporter and every optional storage
//	interface s3store implements.
