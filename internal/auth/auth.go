// Package auth authenticates S3 and admin API requests using AWS Signature
// Version 4 (Authorization header, presigned query string, aws-chunked
// streaming payloads with chunk signatures and/or trailing checksums) and
// verifies browser POST policy uploads.
//
// Authentication only establishes WHO is calling; authorization (what they
// may do) lives in internal/iam.
//
// CONTRACT: exported names/signatures in this file are fixed (other packages
// code against them concurrently). The implementation is owned by the auth
// work item; it may add files and unexported helpers freely.
package auth

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// Identity is an authenticated principal.
type Identity struct {
	AccessKey   string
	User        string // IAM user name; RootUser for the root credentials
	IsRoot      bool
	IsAnonymous bool
}

// RootUser is the user name reported for the root credentials.
const RootUser = "root"

// Anonymous is the identity of unauthenticated requests.
var Anonymous = Identity{IsAnonymous: true}

// CredentialProvider resolves access keys. internal/iam implements it.
type CredentialProvider interface {
	// LookupCredentials returns the secret key and identity for an access key.
	// It returns s3err.ErrInvalidAccessKeyID if the key is unknown, inactive
	// or its user is disabled.
	LookupCredentials(accessKey string) (secretKey string, id Identity, err error)
}

// Method is how a request was authenticated.
type Method string

const (
	MethodAnonymous   Method = "anonymous"
	MethodHeaderV4    Method = "header-v4"
	MethodPresignedV4 Method = "presigned-v4"
	MethodPostPolicy  Method = "post-policy"
)

// Payload modes (value of x-amz-content-sha256).
const (
	PayloadUnsigned                 = "UNSIGNED-PAYLOAD"
	PayloadStreamingSigned          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	PayloadStreamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	PayloadStreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	PayloadEmptySHA256              = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Result is the outcome of a successful Authenticate.
type Result struct {
	Identity Identity
	Method   Method
	// Region and Service from the credential scope ("" for anonymous).
	Region  string
	Service string
	// Body must be used by handlers instead of r.Body. It transparently
	// decodes aws-chunked encoding, verifies chunk signatures and/or the
	// x-amz-content-sha256 payload hash. A verification failure is returned
	// from Read as an *s3err.Error (e.g. ErrXAmzContentSHA256Mismatch,
	// ErrSignatureDoesNotMatch, ErrIncompleteBody) no later than at EOF, so
	// storage writers must Abort on read errors.
	Body io.ReadCloser
	// DecodedContentLength is the length of the decoded payload
	// (x-amz-decoded-content-length for aws-chunked, else r.ContentLength);
	// -1 if unknown.
	DecodedContentLength int64
	// Trailer returns trailing headers sent with aws-chunked trailer payloads
	// (e.g. "X-Amz-Checksum-Crc32"), available once Body returned io.EOF.
	// Returns an empty header if there were none. Never nil.
	Trailer func() http.Header
	// PayloadMode is the x-amz-content-sha256 value (or "" for anonymous /
	// presigned requests, which are UNSIGNED-PAYLOAD).
	PayloadMode string
}

// Options configures a Verifier.
type Options struct {
	// Region is the server region. When StrictRegion is true, requests whose
	// credential scope region differs are rejected with
	// AuthorizationHeaderMalformed; otherwise any region is accepted.
	Region       string
	StrictRegion bool
	// MaxClockSkew bounds |now - x-amz-date| for header auth (default 15m).
	MaxClockSkew time.Duration
	// Now overrides the clock (tests). Defaults to time.Now.
	Now func() time.Time
}

// Verifier authenticates requests.
type Verifier struct {
	creds  CredentialProvider
	opts   Options
	region atomic.Pointer[string] // current server region (see SetRegion)
}

// NewVerifier returns a Verifier backed by creds.
func NewVerifier(creds CredentialProvider, opts Options) *Verifier {
	if opts.MaxClockSkew <= 0 {
		opts.MaxClockSkew = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	v := &Verifier{creds: creds, opts: opts}
	v.SetRegion(opts.Region)
	return v
}

// SetRegion changes the server region checked with StrictRegion (the
// region can be changed at runtime through the settings). Safe for
// concurrent use with Authenticate.
func (v *Verifier) SetRegion(region string) { v.region.Store(&region) }

// currentRegion returns the region set by NewVerifier or SetRegion.
func (v *Verifier) currentRegion() string { return *v.region.Load() }

// Authenticate authenticates r. Requests without any authentication
// information yield Result{Identity: Anonymous, Method: MethodAnonymous}.
// Errors are *s3err.Error values.
//
// Supported: SigV4 Authorization header, SigV4 presigned query strings,
// and payloads signed with a hex SHA-256, UNSIGNED-PAYLOAD,
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD(-TRAILER) and
// STREAMING-UNSIGNED-PAYLOAD-TRAILER (aws-chunked). SigV2 is rejected with
// AccessDenied. Anonymous requests still get a verifying Body when they
// declare x-amz-content-sha256 or use unsigned aws-chunked encoding.
func (v *Verifier) Authenticate(r *http.Request) (*Result, error) {
	if r == nil {
		return nil, s3err.ErrInvalidRequest
	}
	return v.authenticate(r)
}

// PostPolicyForm is the parsed multipart/form-data fields of a browser POST
// upload (field names lowercased), excluding the file.
type PostPolicyForm map[string]string

// PostPolicyConditions is the decoded policy document, used by the object
// handler to check the actual upload (key, content-length-range, ...).
type PostPolicyConditions struct {
	Expiration time.Time
	// Conditions are the raw conditions of the policy document.
	Conditions []any
}

// VerifyPostPolicy verifies the signature of a browser POST upload form
// (x-amz-algorithm=AWS4-HMAC-SHA256, x-amz-credential, x-amz-date, policy,
// x-amz-signature). It returns the identity and decoded policy; forms with no
// policy/signature yield Anonymous and a nil policy. An expired policy is
// rejected with AccessDenied ("Policy expired").
func (v *Verifier) VerifyPostPolicy(form PostPolicyForm) (Identity, *PostPolicyConditions, error) {
	return v.verifyPostPolicy(form)
}

// CheckPostPolicy validates that the uploaded form fields and file size
// satisfy every condition in the policy (eq, starts-with,
// content-length-range, bucket, key, ...). Returns
// s3err.ErrAccessDenied/ErrInvalidPolicyDocument/ErrEntityTooLarge/...
//
// form must include every submitted field except the file (lowercased
// names) plus "bucket" set to the target bucket. Every field other than
// x-amz-signature, file, policy, bucket and x-ignore-* must be covered by a
// condition. A nil p (anonymous upload) always passes. fileSize < 0 skips
// content-length-range checks, so the handler may check the fields before
// streaming the file and the size afterwards.
func CheckPostPolicy(p *PostPolicyConditions, form PostPolicyForm, fileSize int64) error {
	return checkPostPolicy(p, form, fileSize)
}

// Credentials is an access/secret key pair (client side).
type Credentials struct {
	AccessKey string
	SecretKey string
}

// SignRequest signs r in place with SigV4 (Authorization header) — a client
// helper for tests, discovery probes and peer calls. payloadHash is the hex
// SHA-256 of the body or PayloadUnsigned ("" picks the empty-body hash for
// bodiless requests, else PayloadUnsigned). It sets X-Amz-Date,
// X-Amz-Content-Sha256 and Authorization, signs host, every x-amz-* header,
// content-type and content-md5, and pins r.URL.RawPath to the AWS-encoded
// path so the path on the wire is exactly the signed one. Empty region and
// service default to DefaultRegion and "s3".
func SignRequest(r *http.Request, c Credentials, region, service, payloadHash string, t time.Time) error {
	return signRequest(r, c, region, service, payloadHash, t)
}

// PresignURL returns a SigV4 presigned URL for method + rawURL valid for expires.
// Only host is signed (payload UNSIGNED-PAYLOAD, service "s3"); expires must
// be within 1s..7d. Existing query parameters are kept (re-encoded and
// sorted canonically) and stale X-Amz-* auth parameters are replaced.
func PresignURL(method, rawURL string, c Credentials, region string, expires time.Duration, t time.Time) (string, error) {
	return presignURL(method, rawURL, c, region, expires, t)
}
