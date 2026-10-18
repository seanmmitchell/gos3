// Package storage defines the pluggable storage backend contract for gos3.
//
// A Backend stores buckets, object versions (including delete markers),
// opaque per-bucket configuration documents and in-progress multipart uploads.
// It is deliberately "dumb": the S3 layer (internal/s3) computes ETags,
// checksums, validates requests and decides semantics; the backend persists
// bytes + metadata atomically and implements versioning bookkeeping.
//
// Implementations: internal/storage/memory (ephemeral) and internal/storage/fs
// (durable, bbolt metadata index + data files). Both must pass the conformance
// suite in internal/storage/storagetest.
//
// All methods must be safe for concurrent use.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Sentinel errors. Implementations return these (optionally wrapped with
// fmt.Errorf("...: %w", ErrX)); callers test with errors.Is.
var (
	ErrBucketNotFound     = errors.New("storage: bucket not found")
	ErrBucketExists       = errors.New("storage: bucket already exists")
	ErrBucketNotEmpty     = errors.New("storage: bucket not empty")
	ErrObjectNotFound     = errors.New("storage: object not found")
	ErrVersionNotFound    = errors.New("storage: version not found")
	ErrUploadNotFound     = errors.New("storage: multipart upload not found")
	ErrPartNotFound       = errors.New("storage: part not found")
	ErrConfigNotFound     = errors.New("storage: bucket config not found")
	ErrPreconditionFailed = errors.New("storage: precondition failed")
	ErrInvalidName        = errors.New("storage: invalid name")
	// ErrInvalidBacking is returned by CreateBucket for a backing name that
	// is not configured.
	ErrInvalidBacking = errors.New("storage: unknown backing")
	// ErrBackingUnavailable is returned (wrapped) when the backing that
	// holds a bucket's data is not configured any more or cannot be
	// reached (network failure or a 5xx answer from a remote S3 service).
	ErrBackingUnavailable = errors.New("storage: backing unavailable")
	// ErrNotSupported is returned when the bucket's backing cannot perform
	// an operation (e.g. versioning on a gateway backing).
	ErrNotSupported = errors.New("storage: not supported by the bucket's backing")
	// ErrStopWalk may be returned by a Walk callback to stop iteration early;
	// the Walk method then returns nil.
	ErrStopWalk = errors.New("storage: stop walk")

	// ErrBucketUnavailable is returned (wrapped) when one bucket's remote
	// location cannot be used (its remote bucket was deleted, access to it is
	// denied, a wrapped bucket's container cannot be loaded) while the rest
	// of the backing works. It wraps ErrBackingUnavailable, so the S3 layer
	// answers 503, but the multi router does not mark the backing down.
	ErrBucketUnavailable = fmt.Errorf("storage: the bucket's remote location is unavailable: %w", ErrBackingUnavailable)
	// ErrRemoteDenied is returned (wrapped) when a remote S3 service refused
	// an object operation with 403 (object lock, a remote bucket policy,
	// missing permissions of the backing's credentials).
	ErrRemoteDenied = errors.New("storage: the remote service denied the request")
	// ErrRemoteRejected is returned (wrapped) when a remote S3 service
	// rejected a request with a 4xx answer gos3 has no better mapping for.
	ErrRemoteRejected = errors.New("storage: the remote service rejected the request")
	// ErrInvalidTarget is returned by CreateBucket (and placement changes)
	// for a BucketTarget the backing cannot honour: an unknown container, a
	// kind the backing does not offer, a remote location claimed elsewhere.
	ErrInvalidTarget = errors.New("storage: invalid bucket target")
	// ErrRemoteBucketExists is returned by CreateBucket when a native bucket
	// would create a remote bucket that already exists (an administrator can
	// adopt it instead). It wraps ErrBucketExists.
	ErrRemoteBucketExists = fmt.Errorf("storage: remote bucket exists: %w", ErrBucketExists)
	// ErrConflict is returned (wrapped) when an operation gave up after
	// concurrent changes to the same object or upload kept invalidating
	// it (optimistic retries exhausted). It is an answer about contention
	// that clients cause, never a failure of the backing; retrying later
	// may succeed.
	ErrConflict = errors.New("storage: too much concurrent modification")
)

// NullVersionID is the version ID of objects written while versioning is
// Unversioned or Suspended.
const NullVersionID = "null"

// VersioningStatus is a bucket's versioning state.
type VersioningStatus string

const (
	VersioningUnversioned VersioningStatus = "" // never enabled
	VersioningEnabled     VersioningStatus = "Enabled"
	VersioningSuspended   VersioningStatus = "Suspended"
)

// Well-known bucket configuration document keys for Get/Put/DeleteBucketConfig.
// The S3 layer stores the raw XML (or JSON for policy) bodies as received
// (after validation) and the backend treats them as opaque bytes.
const (
	ConfigPolicy            = "policy"       // JSON bucket policy
	ConfigCORS              = "cors"         // CORSConfiguration XML
	ConfigLifecycle         = "lifecycle"    // LifecycleConfiguration XML
	ConfigTagging           = "tagging"      // Tagging XML
	ConfigEncryption        = "encryption"   // ServerSideEncryptionConfiguration XML
	ConfigObjectLock        = "object-lock"  // ObjectLockConfiguration XML
	ConfigACL               = "acl"          // canned ACL name or AccessControlPolicy XML
	ConfigWebsite           = "website"      // WebsiteConfiguration XML
	ConfigOwnership         = "ownership"    // OwnershipControls XML
	ConfigNotification      = "notification" // NotificationConfiguration XML
	ConfigLogging           = "logging"      // BucketLoggingStatus XML
	ConfigPublicAccessBlock = "public-access-block"
	ConfigReplication       = "replication"
	ConfigAccelerate        = "accelerate"
	ConfigRequestPayment    = "request-payment"
	// ConfigCompression is the gos3 bucket compression setting (JSON, see
	// the admin API), not an S3 configuration.
	ConfigCompression = "compression"
	// ConfigResticSnapshots is the gos3 schedule of whole-bucket restic
	// snapshots of a bucket (JSON, see internal/resticsnap and the admin
	// API), not an S3 configuration. It replicates with the bucket's
	// copies; copy jobs never copy it.
	ConfigResticSnapshots = "restic-snapshots"
)

// BucketInfo describes a bucket.
type BucketInfo struct {
	Name              string
	CreatedAt         time.Time
	Region            string
	Versioning        VersioningStatus
	ObjectLockEnabled bool
	Owner             string // IAM user name that created the bucket ("" = root)
	// Backing names the backing holding the bucket's primary copy. The
	// multi-backing router (internal/storage/multi) sets it; the backends
	// behind it report "" (or what they were given). See docs/backings.md.
	Backing string
	// Kind and Location are reported by backings that hold buckets of
	// several kinds (s3 backings, see BucketKind): the bucket's kind and a
	// human-readable location such as "s3://remote-bucket" or
	// "s3://container/prefix". Other backends leave them empty. They are
	// output only: backends never persist them from their input.
	Kind     BucketKind `json:",omitempty"`
	Location string     `json:",omitempty"`
}

// BucketKind is how an s3 backing stores a bucket (see docs/backings.md).
type BucketKind string

const (
	// BucketKindNative buckets map to one remote bucket whose objects are
	// stored as-is; versioning and object lock are the remote bucket's.
	BucketKindNative BucketKind = "native"
	// BucketKindWrapped buckets are stored in gos3's own format inside a
	// container (a remote bucket/prefix shared by many buckets), with every
	// gos3 feature.
	BucketKindWrapped BucketKind = "wrapped"
)

// BucketTarget chooses where a backing that offers several bucket kinds
// creates a bucket. Backends without kinds refuse a non-nil target with
// ErrInvalidTarget (the multi router checks this before calling them).
type BucketTarget struct {
	// Kind is the bucket kind ("" = the backing's default kind).
	Kind BucketKind `json:"kind,omitempty"`
	// RemoteBucket is the remote bucket of a native bucket ("" = the gos3
	// bucket name) and RemoteRegion its region ("" = discovered or the
	// backing's region).
	RemoteBucket string `json:"remoteBucket,omitempty"`
	RemoteRegion string `json:"remoteRegion,omitempty"`
	// Container is the ID of the container of a wrapped bucket ("" = the
	// backing's default container).
	Container string `json:"container,omitempty"`
	// Adopt makes a native bucket use an existing remote bucket instead of
	// creating one. Only the admin API sets it.
	Adopt bool `json:"adopt,omitempty"`
}

// CreateBucketOptions are passed to CreateBucket.
type CreateBucketOptions struct {
	Region            string
	ObjectLockEnabled bool // implies Versioning = Enabled
	Owner             string
	// Backing names the backing to create the bucket on (the
	// x-gos3-backing header; "" = the default backing). The multi-backing
	// router (internal/storage/multi) validates it, returning
	// ErrInvalidBacking for unknown names, and passes "" to the backing it
	// creates the bucket on; other backends ignore it.
	Backing string
	// Target chooses the bucket kind and location on backings that offer
	// several (nil = the backing's defaults). See BucketTarget.
	Target *BucketTarget
	// Import tells the backing the bucket will receive imported versions
	// (a replica or move target, a copy job target), so it must be created
	// where VersionImporter works. Backends without kinds ignore it.
	Import bool
}

// ObjectMeta is the metadata supplied by the S3 layer when committing an
// object or creating a multipart upload. The backend persists it verbatim.
type ObjectMeta struct {
	ETag               string // unquoted, e.g. "d41d8cd98f00b204e9800998ecf8427e" or "<hex>-3"
	ContentType        string
	ContentEncoding    string
	ContentDisposition string
	ContentLanguage    string
	CacheControl       string
	Expires            string            // raw HTTP-date header value
	UserMeta           map[string]string // x-amz-meta-* (lowercased name without prefix) -> value
	Tags               map[string]string
	StorageClass       string // "" means STANDARD
	ACL                string // canned ACL ("" = private)
	// Checksums maps algorithm (CRC32, CRC32C, CRC64NVME, SHA1, SHA256) to the
	// base64 checksum value.
	Checksums    map[string]string
	ChecksumType string  // "FULL_OBJECT" or "COMPOSITE" ("" when no checksum)
	PartSizes    []int64 // sizes of each part for multipart objects (len == parts count)
	// PartChecksums holds per-part checksums for multipart objects (same order
	// as PartSizes) for GetObjectAttributes / GetObject?partNumber.
	PartChecksums []map[string]string
	// Encryption records the actual storage implementation. Empty on legacy
	// objects whose SSE fields were only recorded, without encryption.
	Encryption string
	// Server-side encryption requested by the client.
	SSE         string // "AES256", "aws:kms" or ""
	SSEKMSKeyID string
	// Object Lock
	RetentionMode   string    // "GOVERNANCE", "COMPLIANCE" or ""
	RetainUntil     time.Time // zero = none
	LegalHold       bool
	WebsiteRedirect string // x-amz-website-redirect-location
	// MultipartRetentionSource is set only on an in-progress upload. "default"
	// means its retention must be calculated when the version is published;
	// "explicit" means the caller supplied the retention headers. It is
	// cleared before the completed object's metadata is stored.
	MultipartRetentionSource string
}

// ObjectInfo describes one object version (or delete marker).
type ObjectInfo struct {
	ObjectMeta
	Bucket       string
	Key          string
	VersionID    string // NullVersionID for null versions; never ""
	IsLatest     bool
	DeleteMarker bool
	Size         int64
	LastModified time.Time
	// Compression is the algorithm the backend stored the data with (see
	// CompressingBackend; "" = stored as is). It is set by the backend
	// and transparent to readers: Size and OpenObject are always about
	// the uncompressed content.
	Compression string
	// StoredSize is the number of bytes the backend stores for the data
	// when Compression is set (0 otherwise; see PhysicalSize).
	StoredSize int64
}

// PhysicalSize is the number of data bytes the backend stores for the
// version: the content's size, less when it is compressed, plus the
// overhead of native encryption (EncryptionAES256; an empty object stores
// nothing). Restic versions count their content size: their data lives in
// a repository whose packs are deduplicated, compressed and shared between
// snapshots, so it cannot be attributed to single versions (and the small
// local snapshot reference is not counted). Usage figures therefore never
// report restic data as compression savings. The memory backend keeps
// AES256 data as is, so for it the encryption overhead is notional.
func (o ObjectInfo) PhysicalSize() int64 {
	n := o.Size
	if o.Compression != "" && o.Encryption != EncryptionRestic {
		n = o.StoredSize
	}
	if o.Encryption == EncryptionAES256 && n > 0 {
		return EncryptedSize(n)
	}
	return n
}

// CommitOptions are atomic preconditions evaluated against the current latest
// version of the key at commit time (S3 conditional writes).
type CommitOptions struct {
	// IfNoneMatch: when true the commit fails with ErrPreconditionFailed if a
	// current (non-delete-marker) object exists for the key ("If-None-Match: *").
	IfNoneMatch bool
	// IfMatch: when non-empty the commit fails with ErrPreconditionFailed unless
	// the current object exists and its ETag equals IfMatch (unquoted).
	IfMatch string
	// PartETags is used by CompleteMultipartUpload only. When non-nil it holds
	// one unquoted ETag per listed part number (same order); the completion
	// fails with ErrPartNotFound if a part's current ETag differs, i.e. the
	// part was replaced after the caller validated it. The check is atomic
	// with the commit.
	PartETags []string
	// BeforePublish runs after any data copy, under the backend's atomic
	// bucket commit lock/transaction, with the current object's metadata (nil
	// if absent or a delete marker), the new version's actual creation time,
	// and current bucket configurations. It may update meta or refuse the
	// commit. The callback must not call the same backend.
	BeforePublish func(meta, current *ObjectMeta, created time.Time, config func(key string) []byte) error
}

// ObjectWriter streams the data of a new object version. Nothing becomes
// visible until Commit succeeds. Exactly one of Commit or Abort should be
// called; Abort after Commit is a no-op.
type ObjectWriter interface {
	io.Writer
	// Commit atomically publishes the written bytes as a new version of the
	// key, applying the bucket's versioning semantics:
	//   - Unversioned: VersionID = NullVersionID, replaces any existing null version.
	//   - Enabled:     a new unique VersionID is generated; older versions are kept.
	//   - Suspended:   VersionID = NullVersionID, replaces any existing null
	//                  version (or null delete marker); other versions are kept.
	// Size and LastModified are set by the backend. Returns the stored info.
	Commit(ctx context.Context, meta ObjectMeta, opts CommitOptions) (ObjectInfo, error)
	Abort() error
}

// DeleteResult is returned by DeleteObject.
type DeleteResult struct {
	VersionID    string // version deleted, or version ID of the created delete marker
	DeleteMarker bool   // true if a delete marker was created or the deleted version was a delete marker
}

// UploadInfo describes an in-progress multipart upload.
type UploadInfo struct {
	UploadID  string
	Bucket    string
	Key       string
	Initiated time.Time
	Meta      ObjectMeta // metadata from CreateMultipartUpload
	Initiator string     // IAM user name ("" = root)
}

// PartMeta is supplied by the S3 layer when committing a part.
type PartMeta struct {
	ETag      string            // unquoted MD5 hex of the part
	Checksums map[string]string // algorithm -> base64
}

// PartInfo describes an uploaded part.
type PartInfo struct {
	PartMeta
	PartNumber   int
	Size         int64
	LastModified time.Time
}

// PartWriter streams one multipart part. Committing a part number that
// already exists replaces it.
type PartWriter interface {
	io.Writer
	Commit(ctx context.Context, meta PartMeta) (PartInfo, error)
	Abort() error
}

// Backend is the storage backend contract.
type Backend interface {
	// --- Buckets ---

	// ListBuckets returns all buckets sorted by name.
	ListBuckets(ctx context.Context) ([]BucketInfo, error)
	// CreateBucket creates a bucket. ErrBucketExists if it already exists.
	// Bucket name validation is the S3 layer's job; the backend may return
	// ErrInvalidName for names it cannot store.
	CreateBucket(ctx context.Context, name string, opts CreateBucketOptions) error
	GetBucket(ctx context.Context, name string) (BucketInfo, error)
	// DeleteBucket removes an empty bucket (no versions or delete markers
	// remain, otherwise ErrBucketNotEmpty) along with its configuration
	// documents. In-progress multipart uploads do not make a bucket
	// non-empty: they are aborted with it, as on AWS.
	DeleteBucket(ctx context.Context, name string) error
	// SetBucketVersioning changes the versioning state. Setting
	// VersioningUnversioned on a bucket that has been versioned is invalid and
	// the S3 layer never does it.
	SetBucketVersioning(ctx context.Context, name string, status VersioningStatus) error
	// SetBucketObjectLock marks object lock as enabled on an existing bucket
	// (S3 allows enabling it on versioning-enabled buckets).
	SetBucketObjectLock(ctx context.Context, name string, enabled bool) error

	// GetBucketConfig returns an opaque configuration document (see Config*
	// constants). ErrConfigNotFound if unset; ErrBucketNotFound if no bucket.
	GetBucketConfig(ctx context.Context, bucket, key string) ([]byte, error)
	PutBucketConfig(ctx context.Context, bucket, key string, data []byte) error
	// DeleteBucketConfig is idempotent (no error if unset).
	DeleteBucketConfig(ctx context.Context, bucket, key string) error

	// --- Objects ---

	// NewObjectWriter starts writing a new version of bucket/key.
	// ErrBucketNotFound if the bucket does not exist.
	NewObjectWriter(ctx context.Context, bucket, key string) (ObjectWriter, error)
	// GetObjectInfo returns metadata for a version. versionID "" means the
	// latest version, which may be a delete marker (DeleteMarker=true, nil
	// error). ErrObjectNotFound if the key has no versions at all;
	// ErrVersionNotFound if versionID is given but unknown (versionID "null"
	// addresses the null version).
	GetObjectInfo(ctx context.Context, bucket, key, versionID string) (ObjectInfo, error)
	// OpenObject is GetObjectInfo plus a reader over the full object content.
	// The returned reader is nil iff info.DeleteMarker is true. The reader must
	// keep returning the content of that exact version even if the object is
	// overwritten or deleted concurrently. Caller must Close it.
	OpenObject(ctx context.Context, bucket, key, versionID string) (ObjectInfo, io.ReadSeekCloser, error)
	// UpdateObjectMeta atomically mutates the stored metadata of an existing
	// version (not a delete marker) without changing data, version ID or
	// LastModified. Used for tagging, ACL, retention and legal hold.
	UpdateObjectMeta(ctx context.Context, bucket, key, versionID string, fn func(*ObjectMeta) error) (ObjectInfo, error)
	// DeleteObject deletes per S3 semantics:
	//   - versionID != "": permanently remove that version (or delete marker);
	//     ErrVersionNotFound if absent. If it was the latest, the next newest
	//     becomes latest.
	//   - versionID == "" and bucket Unversioned: remove the null version;
	//     deleting a missing key is not an error (DeleteResult{}).
	//   - versionID == "" and Enabled: add a delete marker with a new version ID.
	//   - versionID == "" and Suspended: add a delete marker with version ID
	//     NullVersionID, replacing any existing null version.
	DeleteObject(ctx context.Context, bucket, key, versionID string) (DeleteResult, error)
	// WalkObjects calls fn with the latest version of each key whose latest
	// version is NOT a delete marker, in ascending byte-wise key order,
	// restricted to keys with the given prefix and strictly greater than
	// after. Returning ErrStopWalk from fn stops iteration (Walk returns nil);
	// any other error is returned as-is.
	WalkObjects(ctx context.Context, bucket, prefix, after string, fn func(ObjectInfo) error) error
	// WalkVersions calls fn for every version and delete marker of keys with
	// the given prefix, keys ascending and, within a key, newest first.
	// Starting point (S3 ListObjectVersions marker semantics):
	//   - keyMarker == "": start at the beginning.
	//   - versionIDMarker == "": start strictly after all versions of keyMarker.
	//   - otherwise: start with the version immediately following
	//     versionIDMarker (older than it) within keyMarker.
	// A versionIDMarker whose version was deleted (e.g. between pages) must
	// not make the walk skip the remaining versions of keyMarker: an ID that
	// encodes its position (as generated IDs may) resumes at that position;
	// NullVersionID carries none, so a "null" marker of a key without a null
	// version starts with the newest remaining version of keyMarker.
	// Re-delivering a version the caller has already seen is acceptable;
	// silently skipping one is not. Other IDs that are not versions of
	// keyMarker start after all of its versions.
	// A walk is not a snapshot when keys change concurrently, but IsLatest
	// is only ever set on the first entry delivered for a key (and only if
	// it was the newest version when read), never on a later entry of the
	// same key.
	WalkVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker string, fn func(ObjectInfo) error) error

	// --- Multipart uploads ---

	CreateMultipartUpload(ctx context.Context, bucket, key string, meta ObjectMeta, initiator string) (UploadInfo, error)
	GetMultipartUpload(ctx context.Context, bucket, key, uploadID string) (UploadInfo, error)
	// NewPartWriter returns ErrUploadNotFound for unknown uploads.
	NewPartWriter(ctx context.Context, bucket, key, uploadID string, partNumber int) (PartWriter, error)
	// ListParts returns all committed parts sorted by part number.
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error)
	// ListMultipartUploads returns all in-progress uploads of the bucket sorted
	// by key, then by Initiated (oldest first), then UploadID.
	ListMultipartUploads(ctx context.Context, bucket string) ([]UploadInfo, error)
	// CompleteMultipartUpload concatenates the given parts (in the given order;
	// ErrPartNotFound if any is missing) into a new object version committed
	// with meta and opts exactly like ObjectWriter.Commit, then removes the
	// upload and all its parts (including parts not listed).
	CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, partNumbers []int, meta ObjectMeta, opts CommitOptions) (ObjectInfo, error)
	// AbortMultipartUpload removes the upload and its parts.
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error

	// Close releases resources (open databases, background goroutines).
	Close() error
}
