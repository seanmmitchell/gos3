package s3

// This file holds the bucket configuration operations; it is owned by the
// s3-bucketconfig work item. Related operations live in
// handlers_bucketconfig_access.go (ACL, ownership controls, public access
// block, object lock configuration) and handlers_bucketconfig_misc.go
// (website, notification, logging, replication, accelerate, request
// payment).
//
// Every configuration document is validated on PUT and stored in a
// canonical form (re-encoded with the S3 namespace) under its
// storage.Config* key; GET handlers decode the stored document and render
// it again, so responses are always canonical XML in the S3 namespace.
// Policies are the exception: they are JSON and returned byte for byte as
// stored.

import (
	"context"
	"encoding/xml"
	"errors"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/lifecycle"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Request body limits of the bucket configuration operations.
const (
	// maxBucketConfigBody bounds XML configuration bodies (except
	// lifecycle, which allows lifecycle.MaxConfigSize, and policies, which
	// allow iam.MaxPolicySize).
	maxBucketConfigBody = 1 << 20
)

// --- Shared helpers ---

// bucketStateLocks serialize the read-check-write sequences that keep
// related bucket settings consistent (versioning vs. object lock and
// replication, bucket ACL vs. object ownership, policy vs. public access
// block) within this process. Buckets hash onto a fixed set of mutexes.
var bucketStateLocks [64]sync.Mutex

// lockBucketState locks the state mutex of bucket and returns the unlock
// function.
func lockBucketState(bucket string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(bucket))
	m := &bucketStateLocks[h.Sum32()%uint32(len(bucketStateLocks))]
	m.Lock()
	return m.Unlock
}

// readConfigBody reads a configuration request body of at most max bytes,
// validating Content-MD5 and x-amz-checksum-* headers or trailers when the
// client sent them (InvalidDigest / BadDigest on mismatch). A larger body
// yields MaxMessageLengthExceeded.
//
// Every bucket configuration write reads its body here, after the core
// authorized it, so the request is re-validated once the body is in
// (revalidateWrite): a body that took long cannot change a bucket's
// policy, ACL or other settings after the caller's access was revoked.
func (s *Server) readConfigBody(r *http.Request, rc *reqContext, max int64) ([]byte, error) {
	if n := rc.contentLength(); n > max {
		return nil, s3err.ErrMaxMessageLengthExceeded
	}
	p, err := s.newPayloadReader(r, rc, payloadOptions{MaxSize: max + 1})
	if err != nil {
		return nil, err
	}
	data, err := readBody(p, max)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	if err := s.revalidateWrite(rc, func() error { return s.reauthorizeOp(ctx, r, rc, nil) }); err != nil {
		return nil, err
	}
	return data, nil
}

// decodeConfigBody reads a configuration body (see readConfigBody) and
// unmarshals it into v: MissingRequestBodyError when empty, MalformedXML
// when not well-formed or the root element does not match.
func (s *Server) decodeConfigBody(r *http.Request, rc *reqContext, max int64, v any) error {
	data, err := s.readConfigBody(r, rc, max)
	if err != nil {
		return err
	}
	return decodeXML(data, v)
}

// loadBucketConfig returns the stored document key of the addressed
// bucket: NoSuchBucket when the bucket does not exist, nil when the
// document is unset.
func (s *Server) loadBucketConfig(ctx context.Context, rc *reqContext, key string) ([]byte, error) {
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return nil, err
	}
	raw, err := s.bucketConfig(ctx, rc.bucket, key)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	return raw, nil
}

// requireBucketConfig is loadBucketConfig for GET handlers: an unset
// document yields notFound (with BucketName).
func (s *Server) requireBucketConfig(ctx context.Context, rc *reqContext, key string, notFound *s3err.Error) ([]byte, error) {
	raw, err := s.loadBucketConfig(ctx, rc, key)
	if err == nil && raw == nil {
		err = notFound.With("BucketName", rc.bucket)
	}
	return raw, err
}

// decodeStoredConfig unmarshals a stored configuration document into v.
// Documents are validated before they are stored, so a failure means the
// stored state is corrupt: it is logged and reported as InternalError.
func (s *Server) decodeStoredConfig(ctx context.Context, rc *reqContext, key string, raw []byte, v any) error {
	if err := xml.Unmarshal(raw, v); err != nil {
		s.log.LogAttrs(ctx, slog.LevelError, "s3: stored bucket configuration is invalid",
			slog.String("request_id", rc.requestID), slog.String("bucket", rc.bucket),
			slog.String("config", key), slog.String("error", err.Error()))
		return s3err.ErrInternalError
	}
	return nil
}

// storeBucketConfig stores the canonical XML encoding of v (or data when v
// is a []byte) under key and drops cached parsed configurations of the
// bucket.
func (s *Server) storeBucketConfig(ctx context.Context, rc *reqContext, key string, v any) error {
	data, ok := v.([]byte)
	if !ok {
		var err error
		if data, err = xml.Marshal(v); err != nil {
			return err
		}
	}
	err := s.backend.PutBucketConfig(ctx, rc.bucket, key, data)
	s.invalidateBucketConfig(rc.bucket)
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	return nil
}

// removeBucketConfig deletes the document key of the addressed bucket
// (idempotent) after checking that the bucket exists.
func (s *Server) removeBucketConfig(ctx context.Context, rc *reqContext, key string) error {
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	err := s.backend.DeleteBucketConfig(ctx, rc.bucket, key)
	s.invalidateBucketConfig(rc.bucket)
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	return nil
}

// deleteBucketConfigResponse deletes the document key of the addressed
// bucket and answers 204 No Content.
func (s *Server) deleteBucketConfigResponse(w http.ResponseWriter, r *http.Request, rc *reqContext, key string) error {
	if err := s.removeBucketConfig(r.Context(), rc, key); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// enabledOrDisabled reports whether v is "Enabled" or "Disabled".
func enabledOrDisabled(v string) bool { return v == "Enabled" || v == "Disabled" }

// --- Versioning ---

// versioningConfiguration is the <VersioningConfiguration> document of
// Get/PutBucketVersioning.
type versioningConfiguration struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	XMLNS     string   `xml:"xmlns,attr,omitempty"`
	Status    string   `xml:"Status,omitempty"`
	MfaDelete string   `xml:"MfaDelete,omitempty"`
}

// handleGetBucketVersioning implements GetBucketVersioning: Status is
// omitted while versioning has never been enabled; MfaDelete is never
// reported (MFA delete is not supported).
func (s *Server) handleGetBucketVersioning(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	bi, err := s.bucketInfo(r.Context(), rc)
	if err != nil {
		return err
	}
	return writeXML(w, http.StatusOK, versioningConfiguration{XMLNS: s3Namespace, Status: string(bi.Versioning)})
}

// handlePutBucketVersioning implements PutBucketVersioning. Status must be
// Enabled or Suspended (a missing Status is
// IllegalVersioningConfigurationException, any other value MalformedXML).
// MfaDelete Enabled is NotImplemented. Versioning cannot be suspended on a
// bucket with object lock or a replication configuration
// (InvalidBucketState).
func (s *Server) handlePutBucketVersioning(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var cfg versioningConfiguration
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &cfg); err != nil {
		return err
	}
	status := storage.VersioningStatus(strings.TrimSpace(cfg.Status))
	switch status {
	case storage.VersioningEnabled, storage.VersioningSuspended:
	case "":
		return s3err.ErrIllegalVersioningConfiguration
	default:
		return s3err.ErrMalformedXML
	}
	switch strings.TrimSpace(cfg.MfaDelete) {
	case "", "Disabled":
	case "Enabled":
		return s3err.ErrNotImplemented.WithMessage("MFA Delete is not supported.")
	default:
		return s3err.ErrMalformedXML
	}

	unlock := lockBucketState(rc.bucket)
	defer unlock()
	bi, err := s.lookupBucket(ctx, rc.bucket)
	if err != nil {
		return err
	}
	if status == storage.VersioningSuspended && bi.Versioning != storage.VersioningSuspended {
		if bi.ObjectLockEnabled {
			return s3err.ErrInvalidBucketState.WithMessage("An Object Lock configuration is present on this bucket, so the versioning state cannot be changed.")
		}
		raw, err := s.bucketConfig(ctx, rc.bucket, storage.ConfigReplication)
		if err != nil {
			return err
		}
		if len(raw) > 0 {
			return s3err.ErrInvalidBucketState.WithMessage("A replication configuration is present on this bucket, so the versioning state cannot be changed.")
		}
	}
	if bi.Versioning != status {
		if err := s.backend.SetBucketVersioning(ctx, rc.bucket, status); err != nil {
			return bucketError(err, rc.bucket)
		}
	}
	rc.forgetBucket()
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- Bucket tagging ---

// handleGetBucketTagging implements GetBucketTagging (NoSuchTagSet when the
// bucket has no tags).
func (s *Server) handleGetBucketTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigTagging, s3err.ErrNoSuchTagSet)
	if err != nil {
		return err
	}
	var t Tagging
	if err := s.decodeStoredConfig(ctx, rc, storage.ConfigTagging, raw, &t); err != nil {
		return err
	}
	t.XMLNS = s3Namespace
	if t.TagSet.Tags == nil {
		t.TagSet.Tags = []Tag{}
	}
	return writeXML(w, http.StatusOK, t)
}

// handlePutBucketTagging implements PutBucketTagging: at most 50 tags with
// unique keys of 1..128 characters, values of at most 256 characters and
// no "aws:" key prefix (InvalidTag). An empty tag set removes the tags.
// Responds 204 No Content, as AWS does.
func (s *Server) handlePutBucketTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var t Tagging
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &t); err != nil {
		return err
	}
	if _, err := validateBucketTags(t.TagSet.Tags); err != nil {
		return err
	}
	if len(t.TagSet.Tags) == 0 {
		return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigTagging)
	}
	t.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigTagging, t); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// handleDeleteBucketTagging implements DeleteBucketTagging (204).
func (s *Server) handleDeleteBucketTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigTagging)
}

// --- CORS ---

// handleGetBucketCors implements GetBucketCors (NoSuchCORSConfiguration
// when unset).
func (s *Server) handleGetBucketCors(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigCORS, s3err.ErrNoSuchCORSConfiguration)
	if err != nil {
		return err
	}
	var c CORSConfiguration
	if err := s.decodeStoredConfig(ctx, rc, storage.ConfigCORS, raw, &c); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutBucketCors implements PutBucketCors. The document is validated
// with parseCORSConfiguration (1..100 rules, each with AllowedMethod in
// GET/PUT/POST/DELETE/HEAD and AllowedOrigin, at most one '*' per origin
// and header, no wildcard ExposeHeader, MaxAgeSeconds >= 0) and takes
// effect immediately for preflight and actual requests.
func (s *Server) handlePutBucketCors(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	data, err := s.readConfigBody(r, rc, maxBucketConfigBody)
	if err != nil {
		return err
	}
	c, err := parseCORSConfiguration(data)
	if err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigCORS, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeleteBucketCors implements DeleteBucketCors (204).
func (s *Server) handleDeleteBucketCors(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigCORS)
}

// --- Policy ---

// policyStatus is the GetBucketPolicyStatus response.
type policyStatus struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ PolicyStatus"`
	IsPublic bool     `xml:"IsPublic"`
}

// handleGetBucketPolicy implements GetBucketPolicy: the JSON document is
// returned exactly as it was stored (NoSuchBucketPolicy when unset).
func (s *Server) handleGetBucketPolicy(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	raw, err := s.requireBucketConfig(r.Context(), rc, storage.ConfigPolicy, s3err.ErrNoSuchBucketPolicy)
	if err != nil {
		return err
	}
	writeBody(w, http.StatusOK, "application/json", raw)
	return nil
}

// handlePutBucketPolicy implements PutBucketPolicy: the body (at most
// 20 KB) must be a valid bucket policy for this bucket
// (iam.ParseBucketPolicy, MalformedPolicy otherwise). A public policy is
// rejected with AccessDenied while the bucket's PublicAccessBlock sets
// BlockPublicPolicy. The policy is stored verbatim; responds 204.
func (s *Server) handlePutBucketPolicy(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	data, err := s.readConfigBody(r, rc, iam.MaxPolicySize)
	if errors.Is(err, s3err.ErrMaxMessageLengthExceeded) {
		return s3err.ErrMalformedPolicy.WithMessage("Policy exceeds the maximum allowed document size of %d bytes.", iam.MaxPolicySize)
	}
	if err != nil {
		return err
	}
	doc, err := iam.ParseBucketPolicy(data, rc.bucket)
	if err != nil {
		return err
	}
	unlock := lockBucketState(rc.bucket)
	defer unlock()
	pab, err := s.publicAccessBlock(ctx, rc.bucket)
	if err != nil {
		return err
	}
	if pab.BlockPublicPolicy && iam.IsPublicPolicy(doc) {
		return s3err.ErrAccessDenied.WithMessage("Access Denied: the policy is public and the bucket's public access block settings block public policies (BlockPublicPolicy).")
	}
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigPolicy, data); err != nil {
		return err
	}
	writeNoContent(w)
	return nil
}

// handleDeleteBucketPolicy implements DeleteBucketPolicy (204, also when
// no policy is set).
func (s *Server) handleDeleteBucketPolicy(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigPolicy)
}

// handleGetBucketPolicyStatus implements GetBucketPolicyStatus: IsPublic
// is true when some Allow statement applies to everyone (Principal "*")
// without a restricting condition such as aws:SourceIp or aws:SourceVpc
// (iam.IsPublicPolicy). NoSuchBucketPolicy when the bucket has no policy.
func (s *Server) handleGetBucketPolicyStatus(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.requireBucketConfig(ctx, rc, storage.ConfigPolicy, s3err.ErrNoSuchBucketPolicy); err != nil {
		return err
	}
	bz, err := s.bucketAuthzConfig(ctx, rc.bucket)
	if err != nil {
		return err
	}
	if bz.policy == nil { // deleted concurrently
		return s3err.ErrNoSuchBucketPolicy.With("BucketName", rc.bucket)
	}
	return writeXML(w, http.StatusOK, policyStatus{IsPublic: bz.policyPublic})
}

// --- Lifecycle ---

// handleGetBucketLifecycle implements GetBucketLifecycleConfiguration (and
// the legacy GetBucketLifecycle): the canonical document stored by PUT
// (NoSuchLifecycleConfiguration when unset).
func (s *Server) handleGetBucketLifecycle(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigLifecycle, s3err.ErrNoSuchLifecycleConfiguration)
	if err != nil {
		return err
	}
	cfg, err := lifecycle.Parse(raw)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelError, "s3: stored lifecycle configuration is invalid",
			slog.String("request_id", rc.requestID), slog.String("bucket", rc.bucket), slog.String("error", err.Error()))
		return s3err.ErrInternalError
	}
	out, err := cfg.Marshal()
	if err != nil {
		return err
	}
	writeBody(w, http.StatusOK, "application/xml", out)
	return nil
}

// handlePutBucketLifecycle implements PutBucketLifecycleConfiguration (and
// the legacy PutBucketLifecycle form with a top-level Prefix per rule).
// The document is validated by lifecycle.Parse; rules without an ID get a
// generated one, and the canonical form (lifecycle Marshal) is stored.
func (s *Server) handlePutBucketLifecycle(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	data, err := s.readConfigBody(r, rc, lifecycle.MaxConfigSize)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return s3err.ErrMissingRequestBody
	}
	cfg, err := lifecycle.Parse(data)
	if err != nil {
		return err
	}
	out, err := cfg.Marshal()
	if err != nil {
		return err
	}
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigLifecycle, out); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeleteBucketLifecycle implements DeleteBucketLifecycle (204).
func (s *Server) handleDeleteBucketLifecycle(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigLifecycle)
}

// --- Default encryption ---

// bucketEncryptionConfig is the <ServerSideEncryptionConfiguration>
// document of Get/PutBucketEncryption (storage.ConfigEncryption).
type bucketEncryptionConfig struct {
	XMLName xml.Name            `xml:"ServerSideEncryptionConfiguration"`
	XMLNS   string              `xml:"xmlns,attr,omitempty"`
	Rules   []bucketEncryptRule `xml:"Rule"`
}

// bucketEncryptRule is one <Rule> of a ServerSideEncryptionConfiguration.
type bucketEncryptRule struct {
	Default          *bucketEncryptDefault `xml:"ApplyServerSideEncryptionByDefault"`
	BucketKeyEnabled *bool                 `xml:"BucketKeyEnabled,omitempty"`
}

// bucketEncryptDefault is <ApplyServerSideEncryptionByDefault>.
type bucketEncryptDefault struct {
	SSEAlgorithm   string `xml:"SSEAlgorithm"`
	KMSMasterKeyID string `xml:"KMSMasterKeyID,omitempty"`
}

// validate checks the configuration: exactly one rule with
// ApplyServerSideEncryptionByDefault (MalformedXML otherwise) and
// SSEAlgorithm AES256 without a KMSMasterKeyID (InvalidArgument). The KMS
// algorithms aws:kms and aws:kms:dsse are NotImplemented, other algorithms
// MalformedXML.
func (c *bucketEncryptionConfig) validate() error {
	if len(c.Rules) != 1 {
		return s3err.ErrMalformedXML
	}
	d := c.Rules[0].Default
	if d == nil {
		return s3err.ErrMalformedXML
	}
	d.SSEAlgorithm = strings.TrimSpace(d.SSEAlgorithm)
	d.KMSMasterKeyID = strings.TrimSpace(d.KMSMasterKeyID)
	switch d.SSEAlgorithm {
	case "AES256":
		if d.KMSMasterKeyID != "" {
			return s3err.ErrInvalidArgument.WithMessage("a KMSMasterKeyID is not applicable if the default sse algorithm is not aws:kms or aws:kms:dsse").
				With("ArgumentName", "ApplyServerSideEncryptionByDefault")
		}
	case "aws:kms", "aws:kms:dsse":
		return errKMSNotImplemented()
	default:
		return s3err.ErrMalformedXML
	}
	return nil
}

// handleGetBucketEncryption implements GetBucketEncryption
// (ServerSideEncryptionConfigurationNotFoundError when unset).
func (s *Server) handleGetBucketEncryption(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	raw, err := s.requireBucketConfig(ctx, rc, storage.ConfigEncryption, s3err.ErrServerSideEncryptionConfigNotFound)
	if err != nil {
		return err
	}
	var c bucketEncryptionConfig
	if err := s.decodeStoredConfig(ctx, rc, storage.ConfigEncryption, raw, &c); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	return writeXML(w, http.StatusOK, c)
}

// handlePutBucketEncryption implements PutBucketEncryption (see
// bucketEncryptionConfig.validate). Verify the selected provider before
// enabling encryption for new objects; existing versions remain unchanged.
func (s *Server) handlePutBucketEncryption(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	if _, err := s.bucketInfo(ctx, rc); err != nil {
		return err
	}
	var c bucketEncryptionConfig
	if err := s.decodeConfigBody(r, rc, maxBucketConfigBody, &c); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	c.XMLNS = s3Namespace
	if err := s.prepareEncryption(ctx, rc.bucket, &storage.ObjectMeta{SSE: "AES256"}, ""); err != nil {
		return err
	}
	if err := s.storeBucketConfig(ctx, rc, storage.ConfigEncryption, c); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeleteBucketEncryption implements DeleteBucketEncryption (204).
func (s *Server) handleDeleteBucketEncryption(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.deleteBucketConfigResponse(w, r, rc, storage.ConfigEncryption)
}
