package s3

import "net/http"

// handlerFunc is the signature of every S3 operation handler (as a method
// expression, e.g. (*Server).handleGetObject). A handler either writes a
// complete success response and returns nil, or returns an error without
// having written anything; the core then renders the error (see
// Server.writeError) and records metrics either way.
type handlerFunc func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error

// resourceKind is the kind of resource an operation addresses.
type resourceKind uint8

const (
	resService resourceKind = iota // no bucket (ListBuckets)
	resBucket                      // a bucket
	resObject                      // an object key in a bucket
)

func (k resourceKind) String() string {
	switch k {
	case resBucket:
		return "BUCKET"
	case resObject:
		return "OBJECT"
	}
	return "SERVICE"
}

// opSpec describes one S3 operation.
type opSpec struct {
	// Name is the operation name used for metrics and logs ("GetObject").
	Name string
	// Action is the IAM action authorized for the primary resource.
	Action string
	// VersionAction, when set, replaces Action if the request carries a
	// versionId query parameter (e.g. s3:GetObjectVersion). It implies
	// Versioned.
	VersionAction string
	// Versioned: the operation acts on the version named by the versionId
	// query parameter, so authorization evaluates that version (its
	// s3:ExistingObjectTag keys, s3:VersionId). Other operations (writes in
	// particular) ignore a versionId and are authorized against the
	// current object; see authzVersionID.
	Versioned bool
	Kind      resourceKind
	// AuthzByHandler: the core does not authorize the primary resource; the
	// handler authorizes itself (DeleteObjects per key).
	AuthzByHandler bool
	// NoAuth: the core does not authenticate (PostObject authenticates and
	// authorizes from the form); the request reaches the handler as
	// anonymous with the raw body.
	NoAuth bool
	// ObjectRead: when denied, the core retries authorization with the
	// object's canned ACL (GetObject, HeadObject).
	ObjectRead bool
	// NoBucketLookup: the bucket need not exist (CreateBucket).
	NoBucketLookup bool
	// RequiresObjectRead: besides Action, the caller must be allowed to
	// read the object itself (s3:GetObject, s3:GetObjectVersion with a
	// versionId; with the object-ACL fallback), as AWS requires for
	// GetObjectAttributes, whose response reveals the content's ETag and
	// checksums.
	RequiresObjectRead bool
	// NewKey: the operation creates an object at the request's key, which
	// must therefore pass validateNewObjectKey (not only validateObjectKey).
	NewKey bool

	handler handlerFunc
}

// action returns the IAM action for a request with the given versionId.
func (o *opSpec) action(versionID string) string {
	if versionID != "" && o.VersionAction != "" {
		return o.VersionAction
	}
	return o.Action
}

// authzVersionID returns the version that authorizing op for rc addresses:
// the versionId query parameter for operations that act on it (Versioned
// or with a VersionAction), "" (the current object) for the others. A
// write creates the current object even with a dummy versionId, so that
// cannot select a version without the current object's tags (J-03).
func authzVersionID(op *opSpec, rc *reqContext) string {
	if op.Versioned || op.VersionAction != "" {
		return rc.versionID()
	}
	return ""
}

// opPreflight is the pseudo-operation name of CORS preflight (OPTIONS)
// requests, which the core answers itself.
const opPreflight = "PreflightRequest"

// opUnknown is the operation name recorded for requests that fail before
// an operation is identified.
const opUnknown = "Unknown"

// opTable lists every S3 operation. Handler bodies live in the area files
// (handlers_bucket.go, handlers_object.go, handlers_multipart.go,
// handlers_bucketconfig.go).
var opTable = []*opSpec{
	// --- Service ---
	{Name: "ListBuckets", Action: "s3:ListAllMyBuckets", Kind: resService, handler: (*Server).handleListBuckets},

	// --- Buckets ---
	{Name: "CreateBucket", Action: "s3:CreateBucket", Kind: resBucket, NoBucketLookup: true, handler: (*Server).handleCreateBucket},
	{Name: "DeleteBucket", Action: "s3:DeleteBucket", Kind: resBucket, handler: (*Server).handleDeleteBucket},
	{Name: "HeadBucket", Action: "s3:ListBucket", Kind: resBucket, handler: (*Server).handleHeadBucket},
	{Name: "GetBucketLocation", Action: "s3:GetBucketLocation", Kind: resBucket, handler: (*Server).handleGetBucketLocation},
	{Name: "ListObjects", Action: "s3:ListBucket", Kind: resBucket, handler: (*Server).handleListObjects},
	{Name: "ListObjectsV2", Action: "s3:ListBucket", Kind: resBucket, handler: (*Server).handleListObjectsV2},
	{Name: "ListObjectVersions", Action: "s3:ListBucketVersions", Kind: resBucket, handler: (*Server).handleListObjectVersions},
	{Name: "ListMultipartUploads", Action: "s3:ListBucketMultipartUploads", Kind: resBucket, handler: (*Server).handleListMultipartUploads},
	{Name: "DeleteObjects", Action: "s3:DeleteObject", Kind: resBucket, AuthzByHandler: true, handler: (*Server).handleDeleteObjects},
	{Name: "PostObject", Action: "s3:PutObject", Kind: resBucket, NoAuth: true, AuthzByHandler: true, handler: (*Server).handlePostObject},

	// --- Bucket configuration ---
	{Name: "GetBucketVersioning", Action: "s3:GetBucketVersioning", Kind: resBucket, handler: (*Server).handleGetBucketVersioning},
	{Name: "PutBucketVersioning", Action: "s3:PutBucketVersioning", Kind: resBucket, handler: (*Server).handlePutBucketVersioning},
	{Name: "GetBucketTagging", Action: "s3:GetBucketTagging", Kind: resBucket, handler: (*Server).handleGetBucketTagging},
	{Name: "PutBucketTagging", Action: "s3:PutBucketTagging", Kind: resBucket, handler: (*Server).handlePutBucketTagging},
	{Name: "DeleteBucketTagging", Action: "s3:PutBucketTagging", Kind: resBucket, handler: (*Server).handleDeleteBucketTagging},
	{Name: "GetBucketCors", Action: "s3:GetBucketCORS", Kind: resBucket, handler: (*Server).handleGetBucketCors},
	{Name: "PutBucketCors", Action: "s3:PutBucketCORS", Kind: resBucket, handler: (*Server).handlePutBucketCors},
	{Name: "DeleteBucketCors", Action: "s3:PutBucketCORS", Kind: resBucket, handler: (*Server).handleDeleteBucketCors},
	{Name: "GetBucketPolicy", Action: "s3:GetBucketPolicy", Kind: resBucket, handler: (*Server).handleGetBucketPolicy},
	{Name: "PutBucketPolicy", Action: "s3:PutBucketPolicy", Kind: resBucket, handler: (*Server).handlePutBucketPolicy},
	{Name: "DeleteBucketPolicy", Action: "s3:DeleteBucketPolicy", Kind: resBucket, handler: (*Server).handleDeleteBucketPolicy},
	{Name: "GetBucketPolicyStatus", Action: "s3:GetBucketPolicyStatus", Kind: resBucket, handler: (*Server).handleGetBucketPolicyStatus},
	{Name: "GetBucketLifecycle", Action: "s3:GetLifecycleConfiguration", Kind: resBucket, handler: (*Server).handleGetBucketLifecycle},
	{Name: "PutBucketLifecycle", Action: "s3:PutLifecycleConfiguration", Kind: resBucket, handler: (*Server).handlePutBucketLifecycle},
	{Name: "DeleteBucketLifecycle", Action: "s3:PutLifecycleConfiguration", Kind: resBucket, handler: (*Server).handleDeleteBucketLifecycle},
	{Name: "GetBucketEncryption", Action: "s3:GetEncryptionConfiguration", Kind: resBucket, handler: (*Server).handleGetBucketEncryption},
	{Name: "PutBucketEncryption", Action: "s3:PutEncryptionConfiguration", Kind: resBucket, handler: (*Server).handlePutBucketEncryption},
	{Name: "DeleteBucketEncryption", Action: "s3:PutEncryptionConfiguration", Kind: resBucket, handler: (*Server).handleDeleteBucketEncryption},
	{Name: "GetBucketAcl", Action: "s3:GetBucketAcl", Kind: resBucket, handler: (*Server).handleGetBucketAcl},
	{Name: "PutBucketAcl", Action: "s3:PutBucketAcl", Kind: resBucket, handler: (*Server).handlePutBucketAcl},
	{Name: "GetObjectLockConfiguration", Action: "s3:GetBucketObjectLockConfiguration", Kind: resBucket, handler: (*Server).handleGetObjectLockConfiguration},
	{Name: "PutObjectLockConfiguration", Action: "s3:PutBucketObjectLockConfiguration", Kind: resBucket, handler: (*Server).handlePutObjectLockConfiguration},
	{Name: "GetBucketOwnershipControls", Action: "s3:GetBucketOwnershipControls", Kind: resBucket, handler: (*Server).handleGetBucketOwnershipControls},
	{Name: "PutBucketOwnershipControls", Action: "s3:PutBucketOwnershipControls", Kind: resBucket, handler: (*Server).handlePutBucketOwnershipControls},
	{Name: "DeleteBucketOwnershipControls", Action: "s3:PutBucketOwnershipControls", Kind: resBucket, handler: (*Server).handleDeleteBucketOwnershipControls},
	{Name: "GetBucketWebsite", Action: "s3:GetBucketWebsite", Kind: resBucket, handler: (*Server).handleGetBucketWebsite},
	{Name: "PutBucketWebsite", Action: "s3:PutBucketWebsite", Kind: resBucket, handler: (*Server).handlePutBucketWebsite},
	{Name: "DeleteBucketWebsite", Action: "s3:DeleteBucketWebsite", Kind: resBucket, handler: (*Server).handleDeleteBucketWebsite},
	{Name: "GetBucketNotification", Action: "s3:GetBucketNotification", Kind: resBucket, handler: (*Server).handleGetBucketNotification},
	{Name: "PutBucketNotification", Action: "s3:PutBucketNotification", Kind: resBucket, handler: (*Server).handlePutBucketNotification},
	{Name: "GetBucketLogging", Action: "s3:GetBucketLogging", Kind: resBucket, handler: (*Server).handleGetBucketLogging},
	{Name: "PutBucketLogging", Action: "s3:PutBucketLogging", Kind: resBucket, handler: (*Server).handlePutBucketLogging},
	{Name: "GetPublicAccessBlock", Action: "s3:GetBucketPublicAccessBlock", Kind: resBucket, handler: (*Server).handleGetPublicAccessBlock},
	{Name: "PutPublicAccessBlock", Action: "s3:PutBucketPublicAccessBlock", Kind: resBucket, handler: (*Server).handlePutPublicAccessBlock},
	{Name: "DeletePublicAccessBlock", Action: "s3:PutBucketPublicAccessBlock", Kind: resBucket, handler: (*Server).handleDeletePublicAccessBlock},
	{Name: "GetBucketReplication", Action: "s3:GetReplicationConfiguration", Kind: resBucket, handler: (*Server).handleGetBucketReplication},
	{Name: "PutBucketReplication", Action: "s3:PutReplicationConfiguration", Kind: resBucket, handler: (*Server).handlePutBucketReplication},
	{Name: "DeleteBucketReplication", Action: "s3:PutReplicationConfiguration", Kind: resBucket, handler: (*Server).handleDeleteBucketReplication},
	{Name: "GetBucketAccelerate", Action: "s3:GetAccelerateConfiguration", Kind: resBucket, handler: (*Server).handleGetBucketAccelerate},
	{Name: "PutBucketAccelerate", Action: "s3:PutAccelerateConfiguration", Kind: resBucket, handler: (*Server).handlePutBucketAccelerate},
	{Name: "GetBucketRequestPayment", Action: "s3:GetBucketRequestPayment", Kind: resBucket, handler: (*Server).handleGetBucketRequestPayment},
	{Name: "PutBucketRequestPayment", Action: "s3:PutBucketRequestPayment", Kind: resBucket, handler: (*Server).handlePutBucketRequestPayment},

	// --- Objects ---
	{Name: "PutObject", Action: "s3:PutObject", Kind: resObject, NewKey: true, handler: (*Server).handlePutObject},
	{Name: "CopyObject", Action: "s3:PutObject", Kind: resObject, NewKey: true, handler: (*Server).handleCopyObject},
	{Name: "GetObject", Action: "s3:GetObject", VersionAction: "s3:GetObjectVersion", Kind: resObject, ObjectRead: true, handler: (*Server).handleGetObject},
	{Name: "HeadObject", Action: "s3:GetObject", VersionAction: "s3:GetObjectVersion", Kind: resObject, ObjectRead: true, handler: (*Server).handleHeadObject},
	{Name: "DeleteObject", Action: "s3:DeleteObject", VersionAction: "s3:DeleteObjectVersion", Kind: resObject, handler: (*Server).handleDeleteObject},
	{Name: "GetObjectTagging", Action: "s3:GetObjectTagging", VersionAction: "s3:GetObjectVersionTagging", Kind: resObject, handler: (*Server).handleGetObjectTagging},
	{Name: "PutObjectTagging", Action: "s3:PutObjectTagging", VersionAction: "s3:PutObjectVersionTagging", Kind: resObject, AuthzByHandler: true, handler: (*Server).handlePutObjectTagging},
	{Name: "DeleteObjectTagging", Action: "s3:DeleteObjectTagging", VersionAction: "s3:DeleteObjectVersionTagging", Kind: resObject, handler: (*Server).handleDeleteObjectTagging},
	{Name: "GetObjectAcl", Action: "s3:GetObjectAcl", VersionAction: "s3:GetObjectVersionAcl", Kind: resObject, handler: (*Server).handleGetObjectAcl},
	{Name: "PutObjectAcl", Action: "s3:PutObjectAcl", VersionAction: "s3:PutObjectVersionAcl", Kind: resObject, handler: (*Server).handlePutObjectAcl},
	{Name: "GetObjectRetention", Action: "s3:GetObjectRetention", Versioned: true, Kind: resObject, handler: (*Server).handleGetObjectRetention},
	{Name: "PutObjectRetention", Action: "s3:PutObjectRetention", Versioned: true, Kind: resObject, AuthzByHandler: true, handler: (*Server).handlePutObjectRetention},
	{Name: "GetObjectLegalHold", Action: "s3:GetObjectLegalHold", Versioned: true, Kind: resObject, handler: (*Server).handleGetObjectLegalHold},
	{Name: "PutObjectLegalHold", Action: "s3:PutObjectLegalHold", Versioned: true, Kind: resObject, AuthzByHandler: true, handler: (*Server).handlePutObjectLegalHold},
	{Name: "GetObjectAttributes", Action: "s3:GetObjectAttributes", VersionAction: "s3:GetObjectVersionAttributes", Kind: resObject, RequiresObjectRead: true, handler: (*Server).handleGetObjectAttributes},
	{Name: "RestoreObject", Action: "s3:RestoreObject", Kind: resObject, handler: (*Server).handleRestoreObject},
	{Name: "SelectObjectContent", Action: "s3:GetObject", Kind: resObject, handler: (*Server).handleSelectObjectContent},

	// --- Multipart ---
	{Name: "CreateMultipartUpload", Action: "s3:PutObject", Kind: resObject, NewKey: true, handler: (*Server).handleCreateMultipartUpload},
	{Name: "UploadPart", Action: "s3:PutObject", Kind: resObject, handler: (*Server).handleUploadPart},
	{Name: "UploadPartCopy", Action: "s3:PutObject", Kind: resObject, handler: (*Server).handleUploadPartCopy},
	{Name: "CompleteMultipartUpload", Action: "s3:PutObject", Kind: resObject, NewKey: true, handler: (*Server).handleCompleteMultipartUpload},
	{Name: "AbortMultipartUpload", Action: "s3:AbortMultipartUpload", Kind: resObject, handler: (*Server).handleAbortMultipartUpload},
	{Name: "ListParts", Action: "s3:ListMultipartUploadParts", Kind: resObject, handler: (*Server).handleListParts},
}

// ops indexes opTable by name.
var ops = func() map[string]*opSpec {
	m := make(map[string]*opSpec, len(opTable))
	for _, o := range opTable {
		if _, dup := m[o.Name]; dup {
			panic("s3: duplicate op " + o.Name)
		}
		m[o.Name] = o
	}
	return m
}()

// lookupOp returns the spec of a named operation (nil if unknown).
func lookupOp(name string) *opSpec { return ops[name] }
