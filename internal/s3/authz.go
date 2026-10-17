package s3

import (
	"bytes"
	"container/list"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// ObjectAttrs, when non-nil, replaces every condition key describing
	// the object written (ACL, storage class, encryption, website redirect,
	// object lock and request tags; see objectAttrConditions) that would
	// otherwise be derived from the request headers. Handlers whose object attributes
	// come from a request body or form, or from a multipart upload, set it
	// so headers that are not applied cannot satisfy or dodge a condition.
	ObjectAttrs map[string][]string
}

// authorizeOp authorizes the request's primary resource with the
// operation's IAM action.
func (s *Server) authorizeOp(ctx context.Context, r *http.Request, rc *reqContext) error {
	return s.authorizeOpWith(ctx, r, rc, nil)
}

// authorizeOpWith is authorizeOp for handlers (AuthzByHandler) whose
// object attribute condition keys come from the request body: attrs
// replaces the header-derived ones (see authzTarget.ObjectAttrs).
func (s *Server) authorizeOpWith(ctx context.Context, r *http.Request, rc *reqContext, attrs map[string][]string) error {
	op := rc.op
	v := rc.versionID()
	// Only operations with a version-specific action address the supplied
	// version. A write creates the current object even with a dummy versionId.
	if op.VersionAction == "" {
		v = ""
	}
	t := authzTarget{
		Action:         op.action(v),
		VersionID:      v,
		ObjectRead:     op.ObjectRead,
		NoBucketLookup: op.NoBucketLookup,
		ObjectAttrs:    attrs,
	}
	if attrs == nil && !(rc.identity.IsRoot && !rc.identity.IsAnonymous) {
		// UploadPart, UploadPartCopy and CompleteMultipartUpload write the
		// object that CreateMultipartUpload described: its recorded
		// attributes (for example the encryption a "require SSE" policy
		// checks) are the ones that apply, not this request's headers.
		switch op.Name {
		case "UploadPart", "UploadPartCopy", "CompleteMultipartUpload":
			if up, err := s.backend.GetMultipartUpload(ctx, rc.bucket, rc.key, rc.query.Get("uploadId")); err == nil {
				t.ObjectAttrs = objectAttrConditions(up.Meta, up.Meta.ACL != "", up.Meta.LegalHold, len(up.Meta.Tags) > 0, s.now())
			} else {
				t.ObjectAttrs = map[string][]string{}
			}
		}
	}
	switch op.Kind {
	case resBucket:
		t.Bucket = rc.bucket
	case resObject:
		t.Bucket, t.Key = rc.bucket, rc.key
	}
	if err := s.authorizeTarget(ctx, r, rc, t); err != nil {
		return err
	}
	if err := s.authorizeObjectRead(ctx, r, rc, t); err != nil {
		return err
	}
	// An AuthzByHandler handler has authorized the caller: the rest of its
	// body (if any) is no longer read before authorization.
	rc.clearPreAuthLimit()
	return nil
}

// authorizeObjectRead completes the authorization of an operation with
// RequiresObjectRead: t, already allowed for the operation's own action, is
// asked again for reading the object (s3:GetObject, or s3:GetObjectVersion
// for a versionId), with the object-ACL fallback. Both decisions must
// allow, so an explicit Deny of either action, or no Allow for either,
// yields AccessDenied. Other operations return nil.
func (s *Server) authorizeObjectRead(ctx context.Context, r *http.Request, rc *reqContext, t authzTarget) error {
	if !rc.op.RequiresObjectRead || t.Key == "" {
		return nil
	}
	t.Action = objectReadAction(t.VersionID)
	t.ObjectRead = true
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
	if id.IsAnonymous {
		if err := s.checkAnonymousRequest(ctx, r, rc); err != nil {
			return err
		}
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
		o := s.authzObject(ctx, rc, t.Bucket, t.Key, t.VersionID)
		if o.err != nil {
			return mapStorageError(o.err)
		}
		if o.found && !o.info.DeleteMarker {
			for k, v := range o.info.Tags {
				req.Conditions["s3:ExistingObjectTag/"+k] = []string{v}
			}
		}
	}

	dec := s.iam.Authorize(ctx, req)
	if !dec.Allowed && !dec.ExplicitDeny && t.ObjectRead && t.Key != "" && !bz.aclsDisabled {
		o := s.authzObject(ctx, rc, t.Bucket, t.Key, t.VersionID)
		if o.err != nil {
			return mapStorageError(o.err)
		}
		if info := o.info; o.found && !info.DeleteMarker {
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

// objectRef names an object version as a request addresses it (VersionID
// "" = the latest).
type objectRef struct{ Bucket, Key, VersionID string }

// authzObject is the result of an object lookup made for an authorization
// decision.
type authzObject struct {
	info  storage.ObjectInfo
	found bool  // false: the lookup failed (no such object or version, or an error)
	err   error // unexpected storage failure; never treat it as absent tags
}

// authzObject returns the version of bucket/key (vid "" = latest) that
// authorization evaluates (its tags for s3:ExistingObjectTag conditions,
// its ACL for object reads). The first lookup in a request is remembered:
// later decisions in the request see the same version, and the handlers
// that read the object open exactly that version (openAuthorizedObject),
// so a concurrent overwrite cannot get a version served that was never
// authorized.
func (s *Server) authzObject(ctx context.Context, rc *reqContext, bucket, key, vid string) authzObject {
	ref := objectRef{bucket, key, vid}
	if o, ok := rc.authzObjects[ref]; ok {
		return o
	}
	info, err := s.backend.GetObjectInfo(ctx, bucket, key, vid)
	o := authzObject{info: info, found: err == nil}
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) && !errors.Is(err, storage.ErrVersionNotFound) {
		o.err = err
	}
	if rc.authzObjects == nil {
		rc.authzObjects = map[objectRef]authzObject{}
	}
	rc.authzObjects[ref] = o
	return o
}

// openAuthorizedObject looks up (open: and opens) bucket/key at version vid
// ("" = latest) for a read that authorization may have decided from the
// object itself (authzObject). When it did, the version it evaluated is
// the one returned: it is opened by its version ID, and if it no longer
// matches what was evaluated (an unversioned object overwritten in place,
// its ACL or tags changed, or an object created where none was found)
// the read fails closed with AccessDenied. Storage errors are returned
// unconverted (see objectLookupError); the caller closes the reader.
func (s *Server) openAuthorizedObject(ctx context.Context, rc *reqContext, bucket, key, vid string, open bool) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	get := func(v string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
		if open {
			return s.backend.OpenObject(ctx, bucket, key, v)
		}
		info, err := s.backend.GetObjectInfo(ctx, bucket, key, v)
		return info, nil, err
	}
	o, ok := rc.authzObjects[objectRef{bucket, key, vid}]
	if !ok {
		return get(vid)
	}
	changed := func(info storage.ObjectInfo, rd io.ReadSeekCloser) (storage.ObjectInfo, io.ReadSeekCloser, error) {
		if rd != nil {
			_ = rd.Close()
		}
		rc.authzReason = "object changed after authorization"
		return info, nil, s3err.ErrAccessDenied
	}
	if !o.found {
		info, rd, err := get(vid)
		if err == nil && !info.DeleteMarker {
			return changed(info, rd)
		}
		return info, rd, err
	}
	info, rd, err := get(o.info.VersionID)
	if err != nil {
		if vid == "" && errors.Is(err, storage.ErrVersionNotFound) {
			// The version evaluated was the latest; it is gone now.
			err = storage.ErrObjectNotFound
		}
		return info, nil, err
	}
	if !sameObjectVersion(o.info, info) {
		return changed(info, rd)
	}
	return info, rd, nil
}

// sameObjectVersion reports whether b is the object version a describes,
// with the same content and authorization-relevant metadata.
func sameObjectVersion(a, b storage.ObjectInfo) bool {
	return a.VersionID == b.VersionID && a.DeleteMarker == b.DeleteMarker &&
		a.ETag == b.ETag && a.Size == b.Size && a.LastModified.Equal(b.LastModified) &&
		a.ACL == b.ACL && maps.Equal(a.Tags, b.Tags)
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
	if r.TLS != nil {
		// s3:TlsVersion of a connection terminated here ("1.2", "1.3").
		switch r.TLS.Version {
		case tls.VersionTLS10:
			c["s3:TlsVersion"] = []string{"1.0"}
		case tls.VersionTLS11:
			c["s3:TlsVersion"] = []string{"1.1"}
		case tls.VersionTLS12:
			c["s3:TlsVersion"] = []string{"1.2"}
		case tls.VersionTLS13:
			c["s3:TlsVersion"] = []string{"1.3"}
		}
	}
	// Client-supplied free-form values are truncated: policies glob-match
	// and substitute them.
	set("aws:UserAgent", iam.TruncateRequestValue(r.UserAgent()))
	set("aws:Referer", iam.TruncateRequestValue(r.Referer()))
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
		if !rc.auth.SignedAt.IsZero() {
			// Milliseconds since the request (or presigned URL, or POST
			// policy form) was signed.
			c["s3:signatureAge"] = []string{strconv.FormatInt(max(now.Sub(rc.auth.SignedAt).Milliseconds(), 0), 10)}
		}
	}
	q := rc.query
	for _, p := range [...]string{"prefix", "delimiter", "max-keys"} {
		if q.Has(p) {
			c["s3:"+p] = []string{iam.TruncateRequestValue(q.Get(p))}
		}
	}
	set("s3:VersionId", t.VersionID)
	h := r.Header
	switch rc.opName {
	case "CopyObject", "UploadPartCopy":
		// Only the copy operations read x-amz-copy-source: on any other
		// operation (PostObject in particular) the header is ignored and
		// must not satisfy a copy-source condition.
		if src, err := parseCopySource(h.Get("X-Amz-Copy-Source")); err == nil {
			c["s3:x-amz-copy-source"] = copySourceConditionValues(src)
		}
		if rc.opName == "CopyObject" {
			if v := h.Get("x-amz-metadata-directive"); v != "" {
				// The directive as the copy handler reads it (any case).
				if d, err := parseDirective(h, "x-amz-metadata-directive"); err == nil {
					v = d
				}
				set("s3:x-amz-metadata-directive", v)
			}
		}
	}
	switch rc.opName {
	case "PutObject", "CopyObject", "CompleteMultipartUpload":
		// The conditional write headers, as parseWriteConditions reads
		// them (so AWS's "require If-None-Match" policies work).
		set("s3:if-none-match", strings.TrimSpace(h.Get("If-None-Match")))
		set("s3:if-match", strings.TrimSpace(h.Get("If-Match")))
	}
	switch rc.opName {
	case "PutObject", "CopyObject", "PostObject", "CompleteMultipartUpload":
		c["s3:ObjectCreationOperation"] = []string{"true"}
	case "CreateMultipartUpload", "UploadPart", "UploadPartCopy":
		c["s3:ObjectCreationOperation"] = []string{"false"}
	}
	if t.ObjectAttrs != nil {
		for k, v := range t.ObjectAttrs {
			if len(v) > 0 || k == "s3:RequestObjectTagKeys" {
				c[k] = v
			}
		}
	} else {
		// Header values are trimmed exactly as the handlers trim them when
		// storing the attributes (trimHeaderSpace), so a value the policy
		// did not see is never stored.
		for _, name := range [...]string{
			"x-amz-acl", "x-amz-server-side-encryption", "x-amz-server-side-encryption-aws-kms-key-id",
			"x-amz-storage-class",
		} {
			set("s3:"+name, trimHeaderSpace(h.Get(name)))
		}
		set("s3:x-amz-website-redirect-location", h.Get("x-amz-website-redirect-location"))
		set("s3:object-lock-mode", trimHeaderSpace(h.Get("x-amz-object-lock-mode")))
		if until := trimHeaderSpace(h.Get("x-amz-object-lock-retain-until-date")); until != "" {
			// Any accepted format (e.g. the compact 20060102T150405Z) is
			// reported in the RFC 3339 form Date operators understand.
			if ut, ok := parseISOTime(until); ok {
				until = ut.UTC().Format(time.RFC3339)
				set("s3:object-lock-remaining-retention-days", remainingRetentionDays(ut, now))
			}
			set("s3:object-lock-retain-until-date", until)
		}
		set("s3:object-lock-legal-hold", trimHeaderSpace(h.Get("x-amz-object-lock-legal-hold")))
		// Request tags come from x-amz-tagging only where that header is
		// what gets stored: not for CopyObject with the (default) COPY
		// tagging directive, which keeps the source's tags.
		tags := h.Get("x-amz-tagging")
		if d, _ := parseDirective(h, "X-Amz-Tagging-Directive"); rc.opName == "CopyObject" && d != directiveReplace {
			tags = ""
		}
		if tags != "" {
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
	}
	for k, v := range t.Conditions {
		c[k] = v
	}
	return c
}

// copySourceConditionValues is s3:x-amz-copy-source for a parsed copy
// source: the decoded "bucket/key" (with "?versionId=<id>" when a version
// is named) in both the slash-less and the leading-slash spelling AWS
// examples use, so a StringLike Deny written either way matches whatever
// encoding or leading slash the request used, and a negated operator is
// satisfied by either spelling.
func copySourceConditionValues(src copySource) []string {
	v := src.Bucket + "/" + src.Key
	if src.VersionID != "" {
		v += "?versionId=" + src.VersionID
	}
	return []string{v, "/" + v}
}

// objectAttrConditions returns the condition keys describing the
// attributes of an object being written, from its parsed metadata (see
// authzTarget.ObjectAttrs): s3:x-amz-acl (when aclSet; "private" for the
// default), s3:x-amz-storage-class, s3:x-amz-server-side-encryption(-aws-
// kms-key-id), s3:x-amz-website-redirect-location, s3:object-lock-mode,
// s3:object-lock-retain-until-date, s3:object-lock-remaining-retention-days,
// s3:object-lock-legal-hold (when legalHoldSet) and, when tagsSet, the
// request tag keys. Values are the normalised ones that are stored.
func objectAttrConditions(m storage.ObjectMeta, aclSet, legalHoldSet, tagsSet bool, now time.Time) map[string][]string {
	c := map[string][]string{}
	set := func(k, v string) {
		if v != "" {
			c[k] = []string{v}
		}
	}
	if aclSet {
		acl := m.ACL
		if acl == "" {
			acl = aclPrivate
		}
		set("s3:x-amz-acl", acl)
	}
	set("s3:x-amz-storage-class", m.StorageClass)
	set("s3:x-amz-server-side-encryption", m.SSE)
	set("s3:x-amz-server-side-encryption-aws-kms-key-id", m.SSEKMSKeyID)
	set("s3:x-amz-website-redirect-location", m.WebsiteRedirect)
	if m.RetentionMode != "" {
		maps.Copy(c, retentionConditions(m.RetentionMode, m.RetainUntil, now))
	}
	if legalHoldSet {
		c["s3:object-lock-legal-hold"] = []string{legalHoldStatus(m.LegalHold)}
	}
	if tagsSet {
		maps.Copy(c, requestTagConditions(m.Tags))
	}
	return c
}

// retentionConditions returns the object lock retention condition keys for
// a retention mode and retain-until date.
func retentionConditions(mode string, until, now time.Time) map[string][]string {
	return map[string][]string{
		"s3:object-lock-mode":                     {mode},
		"s3:object-lock-retain-until-date":        {until.UTC().Format(time.RFC3339)},
		"s3:object-lock-remaining-retention-days": {remainingRetentionDays(until, now)},
	}
}

// remainingRetentionDays is s3:object-lock-remaining-retention-days: the
// whole days from now until the retain-until date, rounded up.
func remainingRetentionDays(until, now time.Time) string {
	d := until.Sub(now)
	if d <= 0 {
		return "0"
	}
	return strconv.FormatInt(int64((d+24*time.Hour-1)/(24*time.Hour)), 10)
}

// legalHoldStatus is "ON" or "OFF".
func legalHoldStatus(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
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
