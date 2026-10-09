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
	PolicyDiagnostics = "diagnostics" // admin:ServerInfo, admin:GetStats, admin:GetDiscovery
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
// ARNs of the form arn:aws:iam::<anything>:user/<name> or :root.
type Principal struct {
	Any bool          // "*" or {"AWS":"*"}
	AWS StringOrSlice // user names / ARNs
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
//
// CONTRACT STUB.
func ParsePolicy(data []byte) (*PolicyDocument, error) {
	return nil, ErrInvalidPolicy
}

// ParseBucketPolicy parses and validates a bucket policy for bucket: every
// statement needs a Principal, and resources must refer to the bucket
// (arn:aws:s3:::bucket or arn:aws:s3:::bucket/...). Returns
// s3err.ErrMalformedPolicy (with a descriptive message) on failure.
//
// CONTRACT STUB.
func ParseBucketPolicy(data []byte, bucket string) (*PolicyDocument, error) {
	return nil, s3err.ErrMalformedPolicy
}

// AuthzRequest is an authorization question.
type AuthzRequest struct {
	Identity auth.Identity
	// Action is an IAM action such as "s3:GetObject", "s3:ListBucket" or
	// "admin:ListUsers".
	Action string
	// Bucket/Key identify the resource. Resource ARNs are
	// arn:aws:s3:::<bucket> (Key == "") and arn:aws:s3:::<bucket>/<key>.
	// Bucket == "" means a service-level action (resource "*"); admin actions
	// use Bucket == "" and match Resource "*" or "arn:gos3:admin:::*".
	Bucket string
	Key    string
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
type Manager struct {
	opts Options
}

var _ auth.CredentialProvider = (*Manager)(nil)

// Open loads (or initializes) IAM state.
//
// CONTRACT STUB.
func Open(opts Options) (*Manager, error) { return &Manager{opts: opts}, nil }

// Close flushes state.
func (m *Manager) Close() error { return nil }

// LookupCredentials implements auth.CredentialProvider. CONTRACT STUB.
func (m *Manager) LookupCredentials(accessKey string) (string, auth.Identity, error) {
	return "", auth.Identity{}, s3err.ErrInvalidAccessKeyID
}

// Authorize evaluates req following AWS semantics: root is always allowed;
// an explicit Deny in the identity's policies or the bucket policy denies;
// otherwise an Allow from any identity policy, the bucket policy (principal
// match, incl. "*" for anonymous) or a canned ACL allows; default deny.
//
// CONTRACT STUB.
func (m *Manager) Authorize(ctx context.Context, req AuthzRequest) Decision {
	return Decision{Reason: "not implemented"}
}

// Users. CONTRACT STUBS.
func (m *Manager) ListUsers(ctx context.Context) ([]User, error) { return nil, nil }
func (m *Manager) GetUser(ctx context.Context, name string) (User, error) {
	return User{}, ErrUserNotFound
}
func (m *Manager) CreateUser(ctx context.Context, spec UserSpec) (User, error) {
	return User{}, ErrInvalidName
}
func (m *Manager) UpdateUser(ctx context.Context, name string, u UserUpdate) (User, error) {
	return User{}, ErrUserNotFound
}
func (m *Manager) DeleteUser(ctx context.Context, name string) error { return ErrUserNotFound }

// Access keys. CreateAccessKey returns the secret (only time it is exposed).
// accessKey/secretKey may be supplied by the caller (empty = generate:
// 20-char uppercase alnum access key, 40-char base64url-ish secret). CONTRACT STUBS.
func (m *Manager) CreateAccessKey(ctx context.Context, user, description, accessKey, secretKey string) (AccessKey, error) {
	return AccessKey{}, ErrUserNotFound
}
func (m *Manager) UpdateAccessKey(ctx context.Context, accessKey, status string) error {
	return ErrKeyNotFound
}
func (m *Manager) DeleteAccessKey(ctx context.Context, accessKey string) error { return ErrKeyNotFound }

// Policies. Built-ins are listed with Builtin=true and are read-only. CONTRACT STUBS.
func (m *Manager) ListPolicies(ctx context.Context) ([]NamedPolicy, error) { return nil, nil }
func (m *Manager) GetPolicy(ctx context.Context, name string) (NamedPolicy, error) {
	return NamedPolicy{}, ErrPolicyNotFound
}
func (m *Manager) PutPolicy(ctx context.Context, name, description string, doc PolicyDocument) (NamedPolicy, error) {
	return NamedPolicy{}, ErrInvalidPolicy
}
func (m *Manager) DeletePolicy(ctx context.Context, name string) error { return ErrPolicyNotFound }

// EffectivePolicies returns the identity policies that apply to id (root:
// [admin]; anonymous: none). CONTRACT STUB.
func (m *Manager) EffectivePolicies(ctx context.Context, id auth.Identity) ([]NamedPolicy, error) {
	return nil, nil
}
