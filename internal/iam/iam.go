// Package iam implements gos3 identity and access management: users, access
// keys, named IAM-style policies (built-in + custom), bucket policy parsing and
// the authorization decision engine used by the S3 and admin APIs.
//
// CONTRACT: exported names/signatures in this file are fixed (other packages
// code against them concurrently). The implementation is owned by the iam
// work item; it may add files and unexported helpers freely and replace the
// stub bodies below.
package iam

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
)

// Errors returned by Manager CRUD methods (test with errors.Is).
var (
	ErrUserNotFound   = errors.New("iam: user not found")
	ErrUserExists     = errors.New("iam: user already exists")
	ErrKeyNotFound    = errors.New("iam: access key not found")
	ErrPolicyNotFound = errors.New("iam: policy not found")
	ErrPolicyBuiltin  = errors.New("iam: built-in policies cannot be modified")
	ErrPolicyInUse    = errors.New("iam: policy is attached to users")
	ErrInvalidName    = errors.New("iam: invalid name")
	ErrInvalidPolicy  = errors.New("iam: invalid policy document")
	ErrTooManyKeys    = errors.New("iam: too many access keys for user")
	ErrRootImmutable  = errors.New("iam: the root user cannot be modified")
)

// Built-in policy names.
const (
	PolicyAdmin       = "admin"       // s3:* and admin:* on everything
	PolicyReadWrite   = "readwrite"   // s3:* on all buckets/objects
	PolicyReadOnly    = "readonly"    // s3:Get*, s3:List*, s3:HeadBucket-equivalents
	PolicyWriteOnly   = "writeonly"   // s3:PutObject, multipart upload actions, s3:ListBucketMultipartUploads
	PolicyDiagnostics = "diagnostics" // admin:ServerInfo, admin:GetStats, admin:GetDiscovery, admin:ListBackings, admin:GetBucketPlacement, admin:ListJobs, admin:GetJob, admin:GetBucketSnapshots
)

// Key status values.
const (
	KeyActive   = "Active"
	KeyInactive = "Inactive"
)

// AccessKey is an access key pair belonging to a user.
type AccessKey struct {
	AccessKey   string    `json:"accessKey"`
	SecretKey   string    `json:"secretKey,omitempty"` // only populated when created
	Status      string    `json:"status"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// User is an IAM user.
type User struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Disabled    bool        `json:"disabled"`
	Policies    []string    `json:"policies"`
	Keys        []AccessKey `json:"keys"` // SecretKey always empty here
	CreatedAt   time.Time   `json:"createdAt"`
	UpdatedAt   time.Time   `json:"updatedAt"`
}

// UserSpec creates a user.
type UserSpec struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Policies    []string `json:"policies"`
	Disabled    bool     `json:"disabled"`
}

// UserUpdate updates a user; nil fields are left unchanged.
type UserUpdate struct {
	Description *string   `json:"description,omitempty"`
	Policies    *[]string `json:"policies,omitempty"`
	Disabled    *bool     `json:"disabled,omitempty"`
}

// StringOrSlice is a JSON value that may be a string or an array of strings.
type StringOrSlice []string

// Principal is a policy principal: "*" or {"AWS": "..."|[...]}.
// gos3 matches AWS principals against IAM user names, access keys, "*" and
// account-principal ARNs of the form arn:aws:iam::<account>:root (also
// written as a bare 12-digit account ID, which never matches a user name
// or access key). User ARNs are unsupported because gos3 has no account ID
// or IAM paths. Service and Federated principals are accepted and kept but
// match no gos3 identity.
type Principal struct {
	Any bool          // "*" or {"AWS":"*"}
	AWS StringOrSlice // user names / ARNs
	// other holds Service and Federated principal values by type; they
	// never match a request.
	other map[string]StringOrSlice
}

// Statement is one IAM policy statement.
type Statement struct {
	Sid          string                              `json:"Sid,omitempty"`
	Effect       string                              `json:"Effect"` // "Allow" | "Deny"
	Principal    *Principal                          `json:"Principal,omitempty"`
	NotPrincipal *Principal                          `json:"NotPrincipal,omitempty"`
	Action       StringOrSlice                       `json:"Action,omitempty"`
	NotAction    StringOrSlice                       `json:"NotAction,omitempty"`
	Resource     StringOrSlice                       `json:"Resource,omitempty"`
	NotResource  StringOrSlice                       `json:"NotResource,omitempty"`
	Condition    map[string]map[string]StringOrSlice `json:"Condition,omitempty"`
}

// PolicyDocument is an IAM policy document (identity or bucket policy).
type PolicyDocument struct {
	Version   string      `json:"Version,omitempty"`
	ID        string      `json:"Id,omitempty"`
	Statement []Statement `json:"Statement"`
}

// NamedPolicy is a stored identity policy.
type NamedPolicy struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Builtin     bool           `json:"builtin"`
	Document    PolicyDocument `json:"document"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}

// ParsePolicy parses and validates an identity policy document.
// Errors wrap ErrInvalidPolicy with a human readable reason.
func ParsePolicy(data []byte) (*PolicyDocument, error) {
	doc, reason := decodePolicy(data)
	if reason == "" {
		reason = validateNewIdentityPolicy(doc)
	}
	if reason != "" {
		return nil, wrapf(ErrInvalidPolicy, reason)
	}
	return doc, nil
}

// ParseBucketPolicy parses and validates a bucket policy for bucket: every
// statement needs a Principal, and resources must refer to the bucket
// (arn:aws:s3:::bucket or arn:aws:s3:::bucket/...). Returns
// s3err.ErrMalformedPolicy (with a descriptive message) on failure.
func ParseBucketPolicy(data []byte, bucket string) (*PolicyDocument, error) {
	doc, reason := decodePolicy(data)
	if reason == "" {
		reason = validateBucketPolicy(doc, bucket)
	}
	if reason != "" {
		return nil, s3err.ErrMalformedPolicy.WithMessage("%s", reason)
	}
	return doc, nil
}

// AuthzRequest is an authorization question.
type AuthzRequest struct {
	Identity auth.Identity
	// Action is an IAM action such as "s3:GetObject", "s3:ListBucket" or
	// "admin:ListUsers".
	Action string
	// Bucket/Key identify the resource of s3 actions. Resource ARNs are
	// arn:aws:s3:::<bucket> (Key == "") and arn:aws:s3:::<bucket>/<key>.
	// Bucket == "" means a service-level action (resource "*"). Admin
	// actions ignore Bucket and Key (see Resource).
	Bucket string
	Key    string
	// Resource is the resource of admin actions: AdminUserARN,
	// AdminPolicyARN, AdminBucketARN or AdminBackingARN of the target, or "" for
	// service-level admin actions, which match only the policy resources
	// "*" and "arn:gos3:admin:::*". s3 actions ignore it. s3 ARNs never
	// match admin resources and admin ARNs never match s3 resources.
	Resource string
	// BucketPolicy is the parsed bucket policy (nil if none).
	BucketPolicy *PolicyDocument
	// ACL is the effective canned ACL ("public-read", "public-read-write",
	// "authenticated-read" or "" = private) of the object (for object reads)
	// or bucket (for bucket-level actions). Canned ACLs grant read (and for
	// public-read-write, write) access as in AWS.
	ACL string
	// Conditions is the request condition context, keys like
	// "aws:SourceIp", "aws:SecureTransport", "aws:CurrentTime",
	// "aws:username", "aws:Referer", "aws:UserAgent", "s3:prefix",
	// "s3:delimiter", "s3:max-keys", "s3:x-amz-acl",
	// "s3:x-amz-server-side-encryption", "s3:ExistingObjectTag/<key>",
	// "s3:RequestObjectTag/<key>", "s3:VersionId", "s3:signatureversion",
	// "s3:authType".
	Conditions map[string][]string
}

// Decision is an authorization outcome.
type Decision struct {
	Allowed bool
	// ExplicitDeny is true when a Deny statement matched.
	ExplicitDeny bool
	Reason       string // short explanation for logs / admin UI
}

// Options configures a Manager.
type Options struct {
	// Dir is where iam.json is persisted (0600, atomic writes). Empty = in-memory only.
	Dir string
	// Root credentials (from configuration; never stored in iam.json).
	RootAccessKey string
	RootSecretKey string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Manager is the IAM service. It implements auth.CredentialProvider.
//
// All methods are safe for concurrent use. State is kept in memory and, when
// Options.Dir is set, written synchronously to <Dir>/iam.json on every
// mutation. iam.json contains secret access keys (they are needed to verify
// SigV4 signatures) and must be treated as sensitive: it is created with mode
// 0600 and should be backed up and stored accordingly.
type Manager struct {
	opts Options
	path string // iam.json path; "" when in-memory

	writeMu sync.Mutex   // serializes mutations (see mutate)
	mu      sync.RWMutex // guards st
	st      *state       // copy-on-write: replaced wholesale by mutations

	warned sync.Map // ids already logged by warnOnce
}

var _ auth.CredentialProvider = (*Manager)(nil)

// Open loads (or initializes) IAM state.
func Open(opts Options) (*Manager, error) { return open(opts) }

// Close flushes state. Writes are synchronous, so Close only exists for
// symmetry; it is idempotent and always returns nil.
func (m *Manager) Close() error { return nil }

// LookupCredentials implements auth.CredentialProvider.
func (m *Manager) LookupCredentials(accessKey string) (string, auth.Identity, error) {
	return m.lookupCredentials(accessKey)
}

// Authorize evaluates req following AWS semantics: root is always allowed;
// an explicit Deny in the identity's policies or the bucket policy denies;
// otherwise an Allow from any identity policy, the bucket policy (principal
// match, incl. "*" for anonymous) or a canned ACL allows; default deny.
func (m *Manager) Authorize(ctx context.Context, req AuthzRequest) Decision {
	if err := ctx.Err(); err != nil {
		return Decision{ExplicitDeny: true, Reason: "authorization context canceled"}
	}
	if shared, ok := ctx.Value(requestBudgetKey{}).(*requestBudget); ok {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		return m.authorize(req, shared.b)
	}
	return m.authorize(req, nil)
}

// ListUsers returns all users sorted by name (secrets are never included).
func (m *Manager) ListUsers(ctx context.Context) ([]User, error) { return m.listUsers(ctx) }

// GetUser returns one user (secrets are never included).
func (m *Manager) GetUser(ctx context.Context, name string) (User, error) {
	return m.getUser(ctx, name)
}

// CreateUser creates a user. Names must match [A-Za-z0-9+=,.@_-]{1,64};
// "root" and "anonymous" are reserved, and a name may not be a 12-digit
// account ID or equal an existing access key ID. All policies must exist.
func (m *Manager) CreateUser(ctx context.Context, spec UserSpec) (User, error) {
	return m.createUser(ctx, spec)
}

// UpdateUser updates a user; nil fields of u are left unchanged.
func (m *Manager) UpdateUser(ctx context.Context, name string, u UserUpdate) (User, error) {
	return m.updateUser(ctx, name, u)
}

// DeleteUser deletes a user and all of its access keys.
func (m *Manager) DeleteUser(ctx context.Context, name string) error { return m.deleteUser(ctx, name) }

// CreateAccessKey creates an access key for user and returns it including
// the secret (the only time it is exposed). accessKey/secretKey may be
// supplied by the caller (empty = generate: 20-char uppercase alnum access
// key, 40-char URL-safe secret). Caller supplied access keys must be 3..128
// characters of [A-Za-z0-9._-] and must not equal a user name, a reserved
// name or a 12-digit account ID; caller supplied secrets must be 16..128
// printable ASCII characters. A user may have at most 10 keys.
func (m *Manager) CreateAccessKey(ctx context.Context, user, description, accessKey, secretKey string) (AccessKey, error) {
	return m.createAccessKey(ctx, user, description, accessKey, secretKey)
}

// UpdateAccessKey sets the status (KeyActive or KeyInactive) of a key.
func (m *Manager) UpdateAccessKey(ctx context.Context, accessKey, status string) error {
	return m.updateAccessKey(ctx, "", accessKey, status)
}

// AccessKeyOwner returns the name of the user owning an IAM access key,
// whatever its status (ErrKeyNotFound for unknown keys and the root key).
func (m *Manager) AccessKeyOwner(ctx context.Context, accessKey string) (string, error) {
	return m.accessKeyOwner(ctx, accessKey)
}

// DeleteAccessKey deletes an access key.
func (m *Manager) DeleteAccessKey(ctx context.Context, accessKey string) error {
	return m.deleteAccessKey(ctx, "", accessKey)
}

// ListPolicies returns the built-in policies (Builtin=true, read-only)
// followed by the custom policies sorted by name.
func (m *Manager) ListPolicies(ctx context.Context) ([]NamedPolicy, error) {
	return m.listPolicies(ctx)
}

// GetPolicy returns a built-in or custom policy.
func (m *Manager) GetPolicy(ctx context.Context, name string) (NamedPolicy, error) {
	return m.getPolicy(ctx, name)
}

// PutPolicy creates or replaces a custom identity policy. The document is
// validated like ParsePolicy (errors wrap ErrInvalidPolicy); built-in names
// yield ErrPolicyBuiltin.
func (m *Manager) PutPolicy(ctx context.Context, name, description string, doc PolicyDocument) (NamedPolicy, error) {
	return m.putPolicy(ctx, name, description, doc)
}

// DeletePolicy deletes a custom policy (ErrPolicyInUse if attached to a
// user, ErrPolicyBuiltin for built-ins).
func (m *Manager) DeletePolicy(ctx context.Context, name string) error {
	return m.deletePolicy(ctx, name)
}

// EffectivePolicies returns the identity policies that apply to id (root:
// [admin]; anonymous: none).
func (m *Manager) EffectivePolicies(ctx context.Context, id auth.Identity) ([]NamedPolicy, error) {
	return m.effectivePolicies(ctx, id)
}
