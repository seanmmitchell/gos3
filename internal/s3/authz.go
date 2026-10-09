package s3

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// authzTarget is one authorization question asked by the core or by a
// handler (for secondary resources such as a copy source or each key of
// DeleteObjects).
type authzTarget struct {
	// Action is the IAM action, e.g. "s3:GetObject".
	Action string
	// Bucket and Key name the resource (Key "" = the bucket itself, Bucket
	// "" = service level).
	Bucket string
	Key    string
	// VersionID is the addressed version (s3:VersionId condition and the
	// object ACL fallback).
	VersionID string
	// ObjectRead enables the object-ACL retry: when the first evaluation
	// denies without an explicit Deny, the object's canned ACL is consulted.
	ObjectRead bool
	// NoBucketLookup skips loading the bucket policy/ACL/public access block
	// (the bucket need not exist, e.g. CreateBucket).
	NoBucketLookup bool
	// Conditions are extra condition keys merged over the request context.
	Conditions map[string][]string
}

// authorizeOp authorizes the request's primary resource with the
// operation's IAM action.
func (s *Server) authorizeOp(ctx context.Context, r *http.Request, rc *reqContext) error {
	op := rc.op
	v := rc.versionID()
	t := authzTarget{
		Action:         op.action(v),
		VersionID:      v,
		ObjectRead:     op.ObjectRead,
		NoBucketLookup: op.NoBucketLookup,
	}
	switch op.Kind {
	case resBucket:
		t.Bucket = rc.bucket
	case resObject:
		t.Bucket, t.Key = rc.bucket, rc.key
	}
	return s.authorizeTarget(ctx, r, rc, t)
}

// authorize is the handler helper for secondary resources: it authorizes
// action on bucket/key for the caller of rc. Object reads (s3:GetObject,
// s3:GetObjectVersion) get the object-ACL fallback. versionID may be "".
func (s *Server) authorize(r *http.Request, rc *reqContext, action, bucket, key, versionID string) error {
	a := strings.ToLower(action)
	return s.authorizeTarget(r.Context(), r, rc, authzTarget{
		Action:     action,
		Bucket:     bucket,
		Key:        key,
		VersionID:  versionID,
		ObjectRead: key != "" && (a == "s3:getobject" || a == "s3:getobjectversion"),
	})
}

// objectReadAction returns s3:GetObjectVersion when versionID is set and
// s3:GetObject otherwise (for authorizing copy sources).
func objectReadAction(versionID string) string {
	if versionID != "" {
		return "s3:GetObjectVersion"
	}
	return "s3:GetObject"
}

// authorizeTarget evaluates t for the caller of rc and returns nil (allowed),
// AccessDenied, NoSuchBucket (bucket lookups) or an internal error.
//
// Evaluation: root is always allowed without any lookups. Otherwise the
// bucket's policy (parsed, cached), canned ACL (storage.ConfigACL) and
// PublicAccessBlock configuration are loaded; ObjectOwnership
// BucketOwnerEnforced disables ACL grants entirely; IgnorePublicAcls drops public
// canned ACL grants and RestrictPublicBuckets drops a public bucket
// policy's Allow statements for anonymous callers. Object reads that are
// denied (without an explicit Deny) are retried with the object's own
// canned ACL. The decision reason is logged at debug level.
func (s *Server) authorizeTarget(ctx context.Context, r *http.Request, rc *reqContext, t authzTarget) error {
	id := rc.identity
	if id.IsRoot && !id.IsAnonymous {
		rc.authzReason = "root user"
		return nil
	}
	if s.iam == nil {
		rc.authzReason = "no IAM manager configured"
		return s3err.ErrAccessDenied
	}
	req := iam.AuthzRequest{Identity: id, Action: t.Action, Bucket: t.Bucket, Key: t.Key}
	var bz bucketAuthz
	if t.Bucket != "" && !t.NoBucketLookup {
		var err error
		if bz, err = s.bucketAuthzConfig(ctx, t.Bucket); err != nil {
			return err
		}
		req.BucketPolicy = bz.policy
		if bz.pab.RestrictPublicBuckets && id.IsAnonymous && bz.policyPublic {
			req.BucketPolicy = bz.denyOnly
		}
		// The bucket ACL governs bucket-level actions and object writes; an
		// object's readability comes from the object's own ACL (AWS
		// semantics), consulted below.
		if !t.ObjectRead && !bz.aclsDisabled {
			req.ACL = effectiveACL(bz.acl, bz.pab)
		}
	}
	req.Conditions = s.conditionContext(r, rc, t)
	if t.Key != "" && s.needsExistingTags(ctx, id, bz) {
		if info, err := s.backend.GetObjectInfo(ctx, t.Bucket, t.Key, t.VersionID); err == nil && !info.DeleteMarker {
			for k, v := range info.Tags {
				req.Conditions["s3:ExistingObjectTag/"+k] = []string{v}
			}
		}
	}

	dec := s.iam.Authorize(ctx, req)
	if !dec.Allowed && !dec.ExplicitDeny && t.ObjectRead && t.Key != "" && !bz.aclsDisabled {
		info, err := s.backend.GetObjectInfo(ctx, t.Bucket, t.Key, t.VersionID)
		if err == nil && !info.DeleteMarker {
			if acl := effectiveACL(info.ACL, bz.pab); acl != "" {
				req.ACL = acl
				dec = s.iam.Authorize(ctx, req)
			}
		}
	}
	rc.authzReason = dec.Reason
	s.log.LogAttrs(ctx, slog.LevelDebug, "s3: authorization decision",
		slog.String("request_id", rc.requestID), slog.String("action", t.Action),
		slog.String("bucket", t.Bucket), slog.String("key", t.Key),
		slog.String("user", rc.userName()), slog.Bool("anonymous", id.IsAnonymous),
		slog.Bool("allowed", dec.Allowed), slog.Bool("explicit_deny", dec.ExplicitDeny),
		slog.String("reason", dec.Reason))
	if !dec.Allowed {
		return s3err.ErrAccessDenied
	}
	return nil
}

// effectiveACL applies IgnorePublicAcls to a canned ACL.
func effectiveACL(acl string, pab PublicAccessBlockConfiguration) string {
	acl = strings.ToLower(strings.TrimSpace(acl))
	if pab.IgnorePublicAcls && isPublicCannedACL(acl) {
		return ""
	}
	return acl
}

// isPublicCannedACL reports whether a canned ACL grants anything to
// AllUsers or AuthenticatedUsers.
func isPublicCannedACL(acl string) bool {
	switch strings.ToLower(acl) {
	case iam.ACLPublicRead, iam.ACLPublicReadWrite, iam.ACLAuthenticatedRead:
		return true
	}
	return false
}

// needsExistingTags reports whether any policy that applies could test
// s3:ExistingObjectTag/* keys, which requires loading the object's tags.
func (s *Server) needsExistingTags(ctx context.Context, id auth.Identity, bz bucketAuthz) bool {
	if bz.policyUsesExistingTag {
		return true
	}
	if id.IsAnonymous || id.IsRoot {
		return false
	}
	pols, err := s.iam.EffectivePolicies(ctx, id)
	if err != nil {
		return false
	}
	for i := range pols {
		if policyUsesKeyPrefix(&pols[i].Document, "s3:existingobjecttag/") {
			return true
		}
	}
	return false
}

// policyUsesKeyPrefix reports whether any condition key of doc starts with
// prefix (lowercase).
func policyUsesKeyPrefix(doc *iam.PolicyDocument, prefix string) bool {
	if doc == nil {
		return false
	}
	for _, st := range doc.Statement {
		for _, kv := range st.Condition {
			for k := range kv {
				if strings.HasPrefix(strings.ToLower(k), prefix) {
					return true
				}
			}
		}
	}
	return false
}

// conditionContext builds the IAM condition context for a request.
func (s *Server) conditionContext(r *http.Request, rc *reqContext, t authzTarget) map[string][]string {
	now := s.now().UTC()
	c := map[string][]string{
		"aws:CurrentTime":     {now.Format("2006-01-02T15:04:05Z")},
		"aws:EpochTime":       {strconv.FormatInt(now.Unix(), 10)},
		"aws:SecureTransport": {strconv.FormatBool(rc.secure)},
	}
	if rc.sourceIP != "" {
		c["aws:SourceIp"] = []string{rc.sourceIP}
	}
	set := func(k, v string) {
		if v != "" {
			c[k] = []string{v}
		}
	}
	set("aws:UserAgent", r.UserAgent())
	set("aws:Referer", r.Referer())
	id := rc.identity
	switch {
	case id.IsAnonymous:
		c["aws:PrincipalType"] = []string{"Anonymous"}
	case id.IsRoot:
		c["aws:PrincipalType"] = []string{"Account"}
		set("aws:username", auth.RootUser)
	default:
		c["aws:PrincipalType"] = []string{"User"}
		set("aws:username", id.User)
		set("aws:userid", id.User)
	}
	if rc.auth != nil {
		switch rc.auth.Method {
		case auth.MethodHeaderV4:
			c["s3:authType"] = []string{"REST-HEADER"}
		case auth.MethodPresignedV4:
			c["s3:authType"] = []string{"REST-QUERY-STRING"}
		case auth.MethodPostPolicy:
			c["s3:authType"] = []string{"POST"}
		}
		if rc.auth.Method != auth.MethodAnonymous {
			c["s3:signatureversion"] = []string{"AWS4-HMAC-SHA256"}
		}
		set("s3:x-amz-content-sha256", rc.auth.PayloadMode)
	}
	q := rc.query
	for _, p := range [...]string{"prefix", "delimiter", "max-keys"} {
		if q.Has(p) {
			c["s3:"+p] = []string{q.Get(p)}
		}
	}
	set("s3:VersionId", t.VersionID)
	h := r.Header
	for _, name := range [...]string{
		"x-amz-acl", "x-amz-server-side-encryption", "x-amz-server-side-encryption-aws-kms-key-id",
		"x-amz-storage-class", "x-amz-copy-source", "x-amz-metadata-directive",
		"x-amz-website-redirect-location",
	} {
		set("s3:"+name, h.Get(name))
	}
	set("s3:object-lock-mode", h.Get("x-amz-object-lock-mode"))
	set("s3:object-lock-retain-until-date", h.Get("x-amz-object-lock-retain-until-date"))
	set("s3:object-lock-legal-hold", h.Get("x-amz-object-lock-legal-hold"))
	if tags := h.Get("x-amz-tagging"); tags != "" {
		if vals, err := url.ParseQuery(tags); err == nil {
			keys := make([]string, 0, len(vals))
			for k, v := range vals {
				if len(v) > 0 {
					c["s3:RequestObjectTag/"+k] = []string{v[0]}
				}
				keys = append(keys, k)
			}
			c["s3:RequestObjectTagKeys"] = keys
		}
	}
	for k, v := range t.Conditions {
		c[k] = v
	}
	return c
}

// bucketAuthz is the authorization-relevant configuration of a bucket.
type bucketAuthz struct {
	policy                *iam.PolicyDocument
	denyOnly              *iam.PolicyDocument // policy without Allow statements
	policyPublic          bool
	policyUsesExistingTag bool
	acl                   string
	aclsDisabled          bool // ObjectOwnership BucketOwnerEnforced
	pab                   PublicAccessBlockConfiguration
}

// parsedPolicy is the cached form of a bucket policy.
type parsedPolicy struct {
	doc              *iam.PolicyDocument
	denyOnly         *iam.PolicyDocument
	public           bool
	usesExistingTags bool
}

// bucketAuthzConfig loads (through the parsed-config cache) the policy,
// canned ACL and public access block of bucket. A missing bucket yields
// NoSuchBucket.
func (s *Server) bucketAuthzConfig(ctx context.Context, bucket string) (bucketAuthz, error) {
	var bz bucketAuthz
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigPolicy)
	if err != nil {
		return bz, err
	}
	if raw != nil {
		v, err := s.configs.get(bucket, storage.ConfigPolicy, raw, func(b []byte) (any, error) {
			doc, err := iam.ParseBucketPolicy(b, bucket)
			if err != nil {
				return nil, err
			}
			p := &parsedPolicy{
				doc:              doc,
				public:           iam.IsPublicPolicy(doc),
				usesExistingTags: policyUsesKeyPrefix(doc, "s3:existingobjecttag/"),
			}
			deny := &iam.PolicyDocument{Version: doc.Version, ID: doc.ID}
			for _, st := range doc.Statement {
				if st.Effect == "Deny" {
					deny.Statement = append(deny.Statement, st)
				}
			}
			p.denyOnly = deny
			return p, nil
		})
		if err != nil {
			// A stored policy was validated on PUT; failing to parse it now
			// must not open access, so fail closed.
			s.log.LogAttrs(ctx, slog.LevelError, "s3: stored bucket policy is invalid",
				slog.String("bucket", bucket), slog.String("error", err.Error()))
			return bz, s3err.ErrInternalError
		}
		p := v.(*parsedPolicy)
		bz.policy, bz.denyOnly, bz.policyPublic, bz.policyUsesExistingTag = p.doc, p.denyOnly, p.public, p.usesExistingTags
	}

	raw, err = s.bucketConfig(ctx, bucket, storage.ConfigACL)
	if err != nil {
		return bz, err
	}
	bz.acl = cannedACLFromConfig(raw)

	raw, err = s.bucketConfig(ctx, bucket, storage.ConfigOwnership)
	if err != nil {
		return bz, err
	}
	bz.aclsDisabled = aclsDisabledByOwnership(raw)

	pab, err := s.publicAccessBlock(ctx, bucket)
	if err != nil {
		return bz, err
	}
	bz.pab = pab
	return bz, nil
}

// bucketConfig returns a raw bucket configuration document, nil when unset,
// or NoSuchBucket.
func (s *Server) bucketConfig(ctx context.Context, bucket, key string) ([]byte, error) {
	raw, err := s.backend.GetBucketConfig(ctx, bucket, key)
	switch {
	case err == nil:
		return raw, nil
	case errors.Is(err, storage.ErrConfigNotFound):
		return nil, nil
	default:
		return nil, bucketError(err, bucket)
	}
}

// publicAccessBlock returns the bucket's PublicAccessBlock configuration
// (all false when unset). Invalid stored documents are treated as the most
// restrictive setting.
func (s *Server) publicAccessBlock(ctx context.Context, bucket string) (PublicAccessBlockConfiguration, error) {
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigPublicAccessBlock)
	if err != nil || raw == nil {
		return PublicAccessBlockConfiguration{}, err
	}
	v, err := s.configs.get(bucket, storage.ConfigPublicAccessBlock, raw, func(b []byte) (any, error) {
		return parsePublicAccessBlock(b)
	})
	if err != nil {
		return PublicAccessBlockConfiguration{BlockPublicAcls: true, IgnorePublicAcls: true, BlockPublicPolicy: true, RestrictPublicBuckets: true}, nil
	}
	return *v.(*PublicAccessBlockConfiguration), nil
}

// invalidateBucketConfig drops every cached parsed configuration of bucket.
// Handlers call it after changing or deleting a bucket configuration
// document (policy, CORS, public access block) or the bucket itself. The
// cache also revalidates against the stored bytes on every use, so this is
// an optimization, not a correctness requirement.
func (s *Server) invalidateBucketConfig(bucket string) { s.configs.invalidate(bucket) }

// defaultConfigCacheSize bounds the number of cached parsed documents.
const defaultConfigCacheSize = 4096

// configCache is a bounded LRU of parsed bucket configuration documents.
// Entries are keyed by (bucket, kind) and remember the raw bytes they were
// parsed from; a lookup with different bytes reparses, so stale entries are
// never served.
type configCache struct {
	mu      sync.Mutex
	max     int
	lru     *list.List // of *cacheEntry, most recent first
	entries map[cacheKey]*list.Element
}

type cacheKey struct{ bucket, kind string }

type cacheEntry struct {
	key cacheKey
	raw []byte
	val any
	err error
}

func newConfigCache(max int) *configCache {
	return &configCache{max: max, lru: list.New(), entries: make(map[cacheKey]*list.Element)}
}

// get returns the parsed form of raw for (bucket, kind), parsing (and
// caching the result, errors included) when absent or stale.
func (c *configCache) get(bucket, kind string, raw []byte, parse func([]byte) (any, error)) (any, error) {
	k := cacheKey{bucket, kind}
	c.mu.Lock()
	if el, ok := c.entries[k]; ok {
		e := el.Value.(*cacheEntry)
		if bytes.Equal(e.raw, raw) {
			c.lru.MoveToFront(el)
			c.mu.Unlock()
			return e.val, e.err
		}
	}
	c.mu.Unlock()

	val, err := parse(raw)
	e := &cacheEntry{key: k, raw: bytes.Clone(raw), val: val, err: err}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		el.Value = e
		c.lru.MoveToFront(el)
	} else {
		c.entries[k] = c.lru.PushFront(e)
	}
	for c.lru.Len() > c.max {
		old := c.lru.Back()
		c.lru.Remove(old)
		delete(c.entries, old.Value.(*cacheEntry).key)
	}
	return val, err
}

// invalidate removes all entries of bucket.
func (c *configCache) invalidate(bucket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, el := range c.entries {
		if k.bucket == bucket {
			c.lru.Remove(el)
			delete(c.entries, k)
		}
	}
}

// len returns the number of cached entries.
func (c *configCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}
