package s3

// This file holds the core object operations (PutObject, GetObject,
// HeadObject, DeleteObject) and the helpers shared by the other s3-object
// files (handlers_object_*.go). It is owned by the s3-object work item.

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Object-operation errors that have no entry in the shared s3err table.
var (
	// errInvalidPartNumber is returned by GetObject/HeadObject for a
	// partNumber beyond the object's part count.
	errInvalidPartNumber = &s3err.Error{Code: "InvalidPartNumber", HTTPStatus: http.StatusRequestedRangeNotSatisfiable,
		Message: "The requested partnumber is not satisfiable"}
	// errACLNotSupported is returned when ACLs are disabled by the
	// BucketOwnerEnforced object ownership setting.
	errACLNotSupported = &s3err.Error{Code: "AccessControlListNotSupported", HTTPStatus: http.StatusBadRequest,
		Message: "The bucket does not allow ACLs"}
	// errObjectLocked is returned when object lock protects a version.
	errObjectLocked = s3err.ErrAccessDenied.WithMessage("Access Denied because object protected by object lock.")
)

// maxVersionIDLength bounds versionId values accepted from clients; gos3
// version IDs are much shorter, so longer values can never exist.
const maxVersionIDLength = 1024

// objectVersionParam returns the versionId query parameter ("" when
// absent), rejecting malformed values with InvalidArgument as S3 does.
func objectVersionParam(q url.Values) (string, error) {
	if !q.Has("versionId") {
		return "", nil
	}
	v := q.Get("versionId")
	if v == "" {
		return "", s3err.ErrInvalidArgument.WithMessage("Version id cannot be the empty string").
			With("ArgumentName", "versionId").With("ArgumentValue", v)
	}
	if !validVersionID(v) {
		return "", s3err.ErrInvalidArgument.WithMessage("Invalid version id specified").
			With("ArgumentName", "versionId").With("ArgumentValue", truncateValue(v))
	}
	return v, nil
}

// validVersionID reports whether v could be a version ID: 1..1024
// printable, non-space ASCII characters.
func validVersionID(v string) bool {
	if v == "" || len(v) > maxVersionIDLength {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= ' ' || v[i] > '~' {
			return false
		}
	}
	return true
}

// truncateValue shortens client-supplied values echoed in error elements.
func truncateValue(v string) string {
	const max = 256
	if len(v) <= max {
		return v
	}
	return v[:max] + "..."
}

// objectLookupError maps a storage error from GetObjectInfo/OpenObject of
// bucket/key (version vid) to the S3 error AWS returns: a missing key with
// an explicit version is NoSuchVersion.
func objectLookupError(err error, bucket, key, vid string) error {
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		return s3err.ErrNoSuchBucket.With("BucketName", bucket)
	case errors.Is(err, storage.ErrVersionNotFound), vid != "" && errors.Is(err, storage.ErrObjectNotFound):
		return s3err.ErrNoSuchVersion.With("Key", key).With("VersionId", vid)
	case errors.Is(err, storage.ErrObjectNotFound):
		return s3err.ErrNoSuchKey.With("Key", key)
	}
	return mapStorageError(err)
}

// deleteMarkerError applies S3's delete marker rules to a read of info
// (a delete marker): it sets x-amz-delete-marker and x-amz-version-id and
// returns NoSuchKey (404) when the latest version was addressed implicitly,
// or MethodNotAllowed (405) when the marker was addressed by version ID.
func deleteMarkerError(w http.ResponseWriter, method string, info storage.ObjectInfo, vid string) error {
	h := w.Header()
	h.Set("X-Amz-Delete-Marker", "true")
	h.Set("X-Amz-Version-Id", info.VersionID)
	if vid != "" {
		return s3err.ErrMethodNotAllowed.With("Method", method).With("ResourceType", "DeleteMarker")
	}
	return s3err.ErrNoSuchKey.With("Key", info.Key)
}

// objectForRead returns the addressed object version (vid "" = latest) of
// the request's bucket/key, applying the delete marker rules of
// deleteMarkerError. With open the content reader is returned too (the
// caller closes it). When authorization looked at the object, the version
// it evaluated is returned (see openAuthorizedObject).
func (s *Server) objectForRead(ctx context.Context, w http.ResponseWriter, r *http.Request, rc *reqContext, vid string, open bool) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	info, rd, err := s.openAuthorizedObject(ctx, rc, rc.bucket, rc.key, vid, open)
	if err != nil {
		return info, nil, objectLookupError(err, rc.bucket, rc.key, vid)
	}
	if info.DeleteMarker {
		if rd != nil {
			_ = rd.Close()
		}
		return info, nil, deleteMarkerError(w, r.Method, info, vid)
	}
	if open && rd == nil {
		return info, nil, errors.New("s3: backend returned no reader for an object")
	}
	return info, rd, nil
}

// --- Copy buffers ---

// copyBufferSize is the buffer size used to stream object data.
const copyBufferSize = 256 << 10

var copyBufPool = sync.Pool{New: func() any {
	b := make([]byte, copyBufferSize)
	return &b
}}

// copyBuffered copies src to dst with a pooled buffer.
func copyBuffered(dst io.Writer, src io.Reader) (int64, error) {
	bp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bp)
	return io.CopyBuffer(dst, src, *bp)
}

// writerOnly hides any io.ReaderFrom of a writer so that io.CopyBuffer
// always uses the pooled buffer.
type writerOnly struct{ io.Writer }

// readerOnly hides any io.WriterTo of a reader for the same reason.
type readerOnly struct{ io.Reader }

// --- Writing new objects ---

// parseWriteConditions parses the conditional write headers of PutObject /
// CopyObject: "If-None-Match: *" (fail when the key has a current object)
// and "If-Match: <etag>" (fail unless the current object has that ETag).
// Other forms are NotImplemented, as in S3.
func parseWriteConditions(h http.Header) (storage.CommitOptions, error) {
	var o storage.CommitOptions
	inm := strings.TrimSpace(h.Get("If-None-Match"))
	im := strings.TrimSpace(h.Get("If-Match"))
	if inm != "" && im != "" {
		return o, s3err.ErrNotImplemented.WithMessage("Multiple conditional request headers present in the request")
	}
	if inm != "" {
		if inm != "*" {
			return o, s3err.ErrNotImplemented.WithMessage("A header you provided implies functionality that is not implemented").
				With("Header", "If-None-Match")
		}
		o.IfNoneMatch = true
	}
	if im != "" {
		if im == "*" || strings.Contains(im, ",") {
			return o, s3err.ErrNotImplemented.WithMessage("A header you provided implies functionality that is not implemented").
				With("Header", "If-Match")
		}
		o.IfMatch = trimETag(im)
		if o.IfMatch == "" {
			return o, s3err.ErrInvalidArgument.WithMessage("Invalid If-Match value").With("ArgumentName", "If-Match")
		}
	}
	return o, nil
}

// commitError converts a failed conditional commit: If-Match on a key
// without a current object is NoSuchKey (as in S3), other precondition
// failures are 412 with the failed condition.
func (s *Server) commitError(ctx context.Context, err error, bucket, key string, cond storage.CommitOptions) error {
	if !errors.Is(err, storage.ErrPreconditionFailed) {
		return bucketError(err, bucket)
	}
	if cond.IfMatch != "" {
		info, gerr := s.backend.GetObjectInfo(ctx, bucket, key, "")
		if errors.Is(gerr, storage.ErrObjectNotFound) || (gerr == nil && info.DeleteMarker) {
			return s3err.ErrNoSuchKey.With("Key", key)
		}
		return s3err.ErrPreconditionFailed.With("Condition", "If-Match")
	}
	return s3err.ErrPreconditionFailed.With("Condition", "If-None-Match")
}

// storeObject streams src (size bytes, -1 = unknown) into a new version of
// bucket/key, compressed as the compression policy says, and commits it
// with meta and cond. finish runs after src reached EOF and before the
// commit (to set ETag and checksums computed while streaming). The writer
// is aborted on any error.
func (s *Server) storeObject(ctx context.Context, bucket, key string, src io.Reader, size int64, meta storage.ObjectMeta,
	cond storage.CommitOptions, finish func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	ow, err := s.newObjectWriter(ctx, bucket, key, s.objectCompression(ctx, bucket, meta, size))
	if err != nil {
		return storage.ObjectInfo{}, bucketError(err, bucket)
	}
	committed := false
	defer func() {
		if !committed {
			_ = ow.Abort()
		}
	}()
	if _, err := copyBuffered(writerOnly{ow}, readerOnly{src}); err != nil {
		return storage.ObjectInfo{}, mapStorageError(err)
	}
	if finish != nil {
		if err := finish(&meta); err != nil {
			return storage.ObjectInfo{}, err
		}
	}
	info, err := ow.Commit(ctx, meta, cond)
	if err != nil {
		return storage.ObjectInfo{}, s.commitError(ctx, err, bucket, key, cond)
	}
	committed = true
	return info, nil
}

// newObjectPayload is newPayloadReader for object data: when the client
// asked for no checksum, a CRC64NVME checksum is computed and stored by
// default, as S3 does for new objects.
func (s *Server) newObjectPayload(r *http.Request, rc *reqContext, opts payloadOptions) (*payloadReader, error) {
	p, err := s.newPayloadReader(r, rc, opts)
	if err != nil || p.Algorithm() != "" || len(opts.Algorithms) > 0 {
		return p, err
	}
	opts.Algorithms = []string{ChecksumCRC64NVME}
	return s.newPayloadReader(r, rc, opts)
}

// prepareNewObject completes and checks the metadata of a new object
// (PutObject, CopyObject, PostObject) written to the bucket bi:
//
//   - object lock headers require object lock on the bucket and the
//     s3:PutObjectRetention / s3:PutObjectLegalHold permissions
//     (o.legalHoldSet: a legal hold header or field was given);
//   - without explicit retention, the bucket's default retention applies;
//   - without explicit SSE, the bucket's default encryption is recorded;
//   - a canned ACL (o.aclSet: given explicitly) requires s3:PutObjectAcl
//     and must be allowed by the bucket's ownership controls and public
//     access block;
//   - tags given with the request or copied from a copy source (o.tagsSet;
//     meta.Tags holds them) require s3:PutObjectTagging, evaluated with
//     the s3:RequestObjectTag/<key> and s3:RequestObjectTagKeys condition
//     keys.
//
// AWS requires the same extra permissions, so a principal limited to
// s3:PutObject can neither publish objects through an ACL nor plant tags
// that s3:ExistingObjectTag conditions rely on.
func (s *Server) prepareNewObject(ctx context.Context, r *http.Request, rc *reqContext, bi storage.BucketInfo,
	key string, meta *storage.ObjectMeta, o newObjectOpts) error {
	if err := requireObjectLock(*meta, bi); err != nil {
		return err
	}
	if o.legalHoldSet && !bi.ObjectLockEnabled {
		return s3err.ErrInvalidRequest.WithMessage("Bucket is missing Object Lock Configuration")
	}
	o.retentionSet = o.retentionSet || meta.RetentionMode != ""
	if err := s.authorizeNewObject(ctx, r, rc, bi.Name, key, meta.Tags, o, nil); err != nil {
		return err
	}
	if bi.ObjectLockEnabled && meta.RetentionMode == "" {
		mode, until, err := s.defaultRetention(ctx, bi.Name)
		if err != nil {
			return err
		}
		if mode != "" {
			meta.RetentionMode, meta.RetainUntil = mode, until
		}
	}
	if meta.SSE == "" {
		sse, kmsKey, err := s.defaultEncryption(ctx, bi.Name)
		if err != nil {
			return err
		}
		meta.SSE, meta.SSEKMSKeyID = sse, kmsKey
	}
	return nil
}

// authorizeNewObject makes the authorization checks of prepareNewObject
// for the object bucket/key with the request tags tags: the permissions
// the explicitly given attributes of o need, and the canned ACL against
// the bucket's ownership controls and public access block. Handlers call
// it again, through revalidateWrite, before a long-running write commits;
// conds (nil on the first call) then holds condition keys to evaluate
// instead of the request's (see requestTimeConditions).
func (s *Server) authorizeNewObject(ctx context.Context, r *http.Request, rc *reqContext, bucket, key string,
	tags map[string]string, o newObjectOpts, conds map[string][]string) error {
	if o.retentionSet {
		if err := s.authorizeTarget(ctx, r, rc, authzTarget{Action: "s3:PutObjectRetention", Bucket: bucket, Key: key,
			Conditions: conds, ObjectAttrs: o.attrs}); err != nil {
			return err
		}
	}
	if o.legalHoldSet {
		if err := s.authorizeTarget(ctx, r, rc, authzTarget{Action: "s3:PutObjectLegalHold", Bucket: bucket, Key: key,
			Conditions: conds, ObjectAttrs: o.attrs}); err != nil {
			return err
		}
	}
	if o.aclSet {
		if err := s.authorizeTarget(ctx, r, rc, authzTarget{Action: "s3:PutObjectAcl", Bucket: bucket, Key: key,
			Conditions: conds, ObjectAttrs: o.attrs}); err != nil {
			return err
		}
	}
	if o.tagsSet {
		tc := requestTagConditions(tags)
		maps.Copy(tc, conds)
		if err := s.authorizeTarget(ctx, r, rc, authzTarget{Action: "s3:PutObjectTagging", Bucket: bucket, Key: key,
			Conditions: tc, ObjectAttrs: o.attrs}); err != nil {
			return err
		}
	}
	if o.aclSet {
		if err := s.checkObjectACLAllowed(ctx, bucket, o.acl); err != nil {
			return err
		}
	}
	return nil
}

// newObjectOpts describes the request-supplied attributes of a new object
// that need permissions beyond s3:PutObject (see prepareNewObject).
type newObjectOpts struct {
	legalHoldSet bool   // an object lock legal hold header or field was given
	retentionSet bool   // object lock retention was given explicitly
	acl          string // the canned ACL
	aclSet       bool   // a canned ACL was given explicitly
	tagsSet      bool   // tags were given or copied (they are in meta.Tags)
	// attrs, when non-nil, are the object attribute condition keys for the
	// extra checks (authzTarget.ObjectAttrs): PostObject's form fields,
	// which are not request headers. Nil uses the request headers.
	attrs map[string][]string
}

// headerObjectOpts returns the newObjectOpts of a request whose object
// attributes come from headers (PutObject, CreateMultipartUpload; CopyObject
// overrides tagsSet for its tagging directive).
func headerObjectOpts(h http.Header) newObjectOpts {
	acl := h.Get("X-Amz-Acl")
	return newObjectOpts{
		legalHoldSet: h.Get("X-Amz-Object-Lock-Legal-Hold") != "",
		retentionSet: trimHeaderSpace(h.Get("X-Amz-Object-Lock-Mode")) != "",
		acl:          acl,
		aclSet:       acl != "",
		tagsSet:      h.Get("X-Amz-Tagging") != "",
	}
}

// requestTagConditions returns the s3:RequestObjectTag/<key> and
// s3:RequestObjectTagKeys condition keys for the tags of a request (an
// empty tag set yields an empty s3:RequestObjectTagKeys).
func requestTagConditions(tags map[string]string) map[string][]string {
	c := make(map[string][]string, len(tags)+1)
	keys := make([]string, 0, len(tags))
	for k, v := range tags {
		c["s3:RequestObjectTag/"+k] = []string{v}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c["s3:RequestObjectTagKeys"] = keys
	return c
}

// checkObjectACLAllowed checks that a canned ACL may be set on an object of
// bucket: BucketOwnerEnforced ownership only accepts
// bucket-owner-full-control (AccessControlListNotSupported otherwise) and
// BlockPublicAcls rejects public ACLs (AccessDenied).
func (s *Server) checkObjectACLAllowed(ctx context.Context, bucket, acl string) error {
	acl = strings.ToLower(strings.TrimSpace(acl))
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigOwnership)
	if err != nil {
		return err
	}
	if aclsDisabledByOwnership(raw) && acl != aclBucketOwnerFullControl {
		return errACLNotSupported
	}
	if isPublicCannedACL(acl) {
		pab, err := s.publicAccessBlock(ctx, bucket)
		if err != nil {
			return err
		}
		if pab.BlockPublicAcls {
			return s3err.ErrAccessDenied
		}
	}
	return nil
}

// objectLockConfigDoc is the stored ObjectLockConfiguration document (only
// the parts needed to apply default retention).
type objectLockConfigDoc struct {
	XMLName           xml.Name `xml:"ObjectLockConfiguration"`
	ObjectLockEnabled string   `xml:"ObjectLockEnabled"`
	Rule              *struct {
		DefaultRetention *struct {
			Mode  string `xml:"Mode"`
			Days  int    `xml:"Days"`
			Years int    `xml:"Years"`
		} `xml:"DefaultRetention"`
	} `xml:"Rule"`
}

// Parsed-config cache kinds used by this file (distinct from the storage
// config keys so other parsers of the same documents never collide).
const (
	cacheKindObjectLockDefaults = "s3-object/object-lock-defaults"
	cacheKindSSEDefaults        = "s3-object/sse-defaults"
)

// lockDefaults is the parsed default retention of a bucket.
type lockDefaults struct {
	mode        string
	days, years int
}

func parseLockDefaults(raw []byte) (*lockDefaults, error) {
	var doc objectLockConfigDoc
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	d := &lockDefaults{}
	if doc.Rule != nil && doc.Rule.DefaultRetention != nil {
		dr := doc.Rule.DefaultRetention
		mode := strings.ToUpper(strings.TrimSpace(dr.Mode))
		if (mode == "GOVERNANCE" || mode == "COMPLIANCE") && dr.Days >= 0 && dr.Years >= 0 &&
			dr.Days <= maxRetentionYears*366 && dr.Years <= maxRetentionYears && dr.Days+dr.Years > 0 {
			d.mode, d.days, d.years = mode, dr.Days, dr.Years
		}
	}
	return d, nil
}

func (d *lockDefaults) retentionAt(created time.Time) (string, time.Time) {
	if d.mode == "" {
		return "", time.Time{}
	}
	return d.mode, created.UTC().AddDate(d.years, 0, d.days)
}

// maxRetentionYears bounds default retention periods (as S3: 100 years).
const maxRetentionYears = 100

// defaultRetention returns the bucket's default object lock retention
// (mode and retain-until date for an object created now), or "" when none
// is configured. An unparsable stored document is logged and ignored.
func (s *Server) defaultRetention(ctx context.Context, bucket string) (string, time.Time, error) {
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigObjectLock)
	if err != nil || raw == nil {
		return "", time.Time{}, err
	}
	v, err := s.configs.get(bucket, cacheKindObjectLockDefaults, raw, func(b []byte) (any, error) {
		return parseLockDefaults(b)
	})
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "s3: ignoring invalid stored object lock configuration",
			slog.String("bucket", bucket), slog.String("error", err.Error()))
		return "", time.Time{}, nil
	}
	mode, until := v.(*lockDefaults).retentionAt(s.now())
	return mode, until, nil
}

// publishObjectRetention uses the bucket's current default and the new
// version's creation time. The backend calls this under its commit lock with
// configurations from the same atomic snapshot; it must not read the backend.
func publishObjectRetention(meta *storage.ObjectMeta, created time.Time, config func(string) []byte, explicit bool) error {
	if explicit {
		if !meta.RetainUntil.After(created) {
			return s3err.ErrInvalidArgument.WithMessage("The retain until date must be in the future!")
		}
		return nil
	}
	meta.RetentionMode, meta.RetainUntil = "", time.Time{}
	if raw := config(storage.ConfigObjectLock); raw != nil {
		d, err := parseLockDefaults(raw)
		if err != nil {
			return s3err.ErrInternalError
		}
		meta.RetentionMode, meta.RetainUntil = d.retentionAt(created)
	}
	return nil
}

// sseConfigDoc is the stored ServerSideEncryptionConfiguration document.
type sseConfigDoc struct {
	XMLName xml.Name `xml:"ServerSideEncryptionConfiguration"`
	Rules   []struct {
		Default *struct {
			SSEAlgorithm   string `xml:"SSEAlgorithm"`
			KMSMasterKeyID string `xml:"KMSMasterKeyID"`
		} `xml:"ApplyServerSideEncryptionByDefault"`
	} `xml:"Rule"`
}

// sseDefaults is the parsed default encryption of a bucket.
type sseDefaults struct{ algorithm, kmsKeyID string }

// defaultEncryption returns the bucket's default server-side encryption
// (from storage.ConfigEncryption) to record on new objects, or "" when no
// default encryption is configured.
func (s *Server) defaultEncryption(ctx context.Context, bucket string) (string, string, error) {
	raw, err := s.bucketConfig(ctx, bucket, storage.ConfigEncryption)
	if err != nil || raw == nil {
		return "", "", err
	}
	v, err := s.configs.get(bucket, cacheKindSSEDefaults, raw, func(b []byte) (any, error) {
		var doc sseConfigDoc
		if err := xml.Unmarshal(b, &doc); err != nil {
			return nil, err
		}
		d := &sseDefaults{}
		for _, rule := range doc.Rules {
			if rule.Default == nil {
				continue
			}
			switch algo := strings.TrimSpace(rule.Default.SSEAlgorithm); algo {
			case "AES256", "aws:kms", "aws:kms:dsse":
				d.algorithm = algo
				if algo != "AES256" {
					d.kmsKeyID = strings.TrimSpace(rule.Default.KMSMasterKeyID)
				}
				return d, nil
			}
		}
		return d, nil
	})
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "s3: ignoring invalid stored encryption configuration",
			slog.String("bucket", bucket), slog.String("error", err.Error()))
		return "", "", nil
	}
	d := v.(*sseDefaults)
	return d.algorithm, d.kmsKeyID, nil
}

// setChecksumHeaders sets x-amz-checksum-<algo> for every checksum plus
// x-amz-checksum-type (when typ is set).
func setChecksumHeaders(h http.Header, sums map[string]string, typ string) {
	if len(sums) == 0 {
		return
	}
	for _, algo := range checksumAlgorithms {
		if v := sums[algo]; v != "" {
			h.Set(checksumHeader(algo), v)
		}
	}
	if typ != "" {
		h.Set("X-Amz-Checksum-Type", typ)
	}
}

// setSSEHeaders reports the server-side encryption of an object.
func setSSEHeaders(h http.Header, meta storage.ObjectMeta) {
	if meta.SSE == "" {
		return
	}
	h.Set("X-Amz-Server-Side-Encryption", meta.SSE)
	if strings.HasPrefix(meta.SSE, "aws:kms") && meta.SSEKMSKeyID != "" {
		h.Set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", meta.SSEKMSKeyID)
	}
}

// --- PutObject ---

// handlePutObject implements PutObject: the payload is streamed into the
// backend while its MD5 (ETag) and checksums are computed and validated
// (Content-MD5, x-amz-checksum-* headers and trailers); conditional writes
// (If-None-Match: *, If-Match) are evaluated atomically at commit.
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	h := r.Header
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	meta, err := s.objectMetaFromHeaders(h)
	if err != nil {
		return err
	}
	cond, err := parseWriteConditions(h)
	if err != nil {
		return err
	}
	opts := headerObjectOpts(h)
	if err := s.prepareNewObject(ctx, r, rc, bi, rc.key, &meta, opts); err != nil {
		return err
	}
	cond.BeforePublish = func(m, _ *storage.ObjectMeta, created time.Time, config func(string) []byte) error {
		return publishObjectRetention(m, created, config, opts.retentionSet)
	}
	p, err := s.newObjectPayload(r, rc, payloadOptions{RequireLength: true})
	if err != nil {
		return err
	}
	info, err := s.storeObject(ctx, rc.bucket, rc.key, p, p.declaredSize(), meta, cond, func(m *storage.ObjectMeta) error {
		m.ETag = p.ETag()
		m.Checksums = p.Checksums()
		m.ChecksumType = p.ChecksumType()
		// The body may have taken long: the caller must still be allowed.
		return s.revalidateWrite(rc, func() error {
			if err := s.reauthorizeOp(ctx, r, rc, nil); err != nil {
				return err
			}
			return s.authorizeNewObject(ctx, r, rc, rc.bucket, rc.key, m.Tags, opts, s.requestTimeConditions(r, rc, nil))
		})
	})
	if err != nil {
		return err
	}
	hdr := w.Header()
	hdr.Set("ETag", quoteETag(info.ETag))
	if bi.Versioning != storage.VersioningUnversioned {
		hdr.Set("X-Amz-Version-Id", info.VersionID)
	}
	setChecksumHeaders(hdr, info.Checksums, info.ChecksumType)
	setSSEHeaders(hdr, info.ObjectMeta)
	s.setExpirationHeader(ctx, hdr, info)
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- GetObject / HeadObject ---

// handleGetObject implements GetObject.
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.serveObject(w, r, rc, true)
}

// handleHeadObject implements HeadObject.
func (s *Server) handleHeadObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s.serveObject(w, r, rc, false)
}

// responseOverrides maps the GetObject response-* query parameters to the
// headers they override.
var responseOverrides = []struct{ param, header string }{
	{"response-content-type", "Content-Type"},
	{"response-content-language", "Content-Language"},
	{"response-expires", "Expires"},
	{"response-cache-control", "Cache-Control"},
	{"response-content-disposition", "Content-Disposition"},
	{"response-content-encoding", "Content-Encoding"},
}

// parseResponseOverrides returns the requested header overrides. As in
// S3, they are only honoured for authenticated (signed or presigned)
// requests.
func parseResponseOverrides(rc *reqContext) (map[string]string, error) {
	var out map[string]string
	for _, o := range responseOverrides {
		if !rc.query.Has(o.param) {
			continue
		}
		if rc.identity.IsAnonymous {
			return nil, s3err.ErrInvalidRequest.WithMessage("Request specific response headers cannot be used for anonymous GET requests.")
		}
		if out == nil {
			out = make(map[string]string, len(responseOverrides))
		}
		out[o.header] = rc.query.Get(o.param)
	}
	return out, nil
}

// serveObject implements GetObject (withBody) and HeadObject: version
// selection, delete markers, preconditions, Range / partNumber, response
// header overrides and checksum mode.
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, rc *reqContext, withBody bool) error {
	ctx := r.Context()
	h := r.Header
	if err := checkSSECHeaders(h); err != nil {
		return err
	}
	vid, err := objectVersionParam(rc.query)
	if err != nil {
		return err
	}
	partNumber := 0
	if rc.query.Has("partNumber") {
		if partNumber, err = parsePartNumber(rc.query.Get("partNumber")); err != nil {
			return err
		}
		if strings.TrimSpace(h.Get("Range")) != "" {
			return s3err.ErrInvalidRequest.WithMessage("Cannot specify both Range header and partNumber query parameter")
		}
	}
	overrides, err := parseResponseOverrides(rc)
	if err != nil {
		return err
	}
	versioned, err := s.isVersioned(ctx, rc)
	if err != nil {
		return err
	}
	info, body, err := s.objectForRead(ctx, w, r, rc, vid, withBody)
	if err != nil {
		return err
	}
	if body != nil {
		defer body.Close()
	}
	if err := checkConditions(h, info.ETag, info.LastModified); err != nil {
		if errors.Is(err, s3err.ErrNotModified) {
			hdr := w.Header()
			hdr.Set("ETag", quoteETag(info.ETag))
			hdr.Set("Last-Modified", formatHTTPTime(info.LastModified))
			if versioned {
				hdr.Set("X-Amz-Version-Id", info.VersionID)
			}
		}
		return err
	}

	status := http.StatusOK
	var (
		rng       *byteRange
		partSums  map[string]string
		emptyPart bool
	)
	multipart := len(info.PartSizes) > 0
	switch {
	case partNumber > 0 && multipart:
		if partNumber > len(info.PartSizes) {
			return errInvalidPartNumber.With("PartNumberRequested", strconv.Itoa(partNumber)).
				With("ActualPartCount", strconv.Itoa(len(info.PartSizes)))
		}
		var off int64
		for _, sz := range info.PartSizes[:partNumber-1] {
			off += sz
		}
		size := info.PartSizes[partNumber-1]
		if off < 0 || size < 0 || off+size > info.Size {
			return errors.New("s3: stored part sizes are inconsistent with the object size")
		}
		// A part is served like a ranged GET (206 + Content-Range); an
		// empty part (possible as the last one) has no satisfiable range,
		// so it is a plain empty 200 rather than a 206 without
		// Content-Range.
		if size > 0 {
			rng = &byteRange{Start: off, Length: size}
		} else {
			emptyPart = true
		}
		if partNumber-1 < len(info.PartChecksums) {
			partSums = info.PartChecksums[partNumber-1]
		}
	case partNumber > 1:
		return errInvalidPartNumber.With("PartNumberRequested", strconv.Itoa(partNumber)).With("ActualPartCount", "1")
	case partNumber == 1:
		// The whole object is its only part; S3 answers like a ranged GET
		// of it (part-based downloaders read the size from Content-Range).
		if info.Size > 0 {
			rng = &byteRange{Start: 0, Length: info.Size}
		}
	case partNumber == 0:
		if !ifRangeAllows(h.Get("If-Range"), info.ETag, info.LastModified) {
			break // the representation changed: serve all of it
		}
		if rng, err = parseRange(h.Get("Range"), info.Size); err != nil {
			return err
		}
	}

	checksumMode := checksumModeEnabled(h)
	// The full-object checksum applies when the whole object is served,
	// which includes part 1 of a single-part object.
	whole := (partNumber == 0 && rng == nil) || (partNumber > 0 && !multipart)
	writeObjectHeaders(w, info, versioned, checksumMode && whole)
	hdr := w.Header()
	s.setExpirationHeader(r.Context(), hdr, info)
	if checksumMode && partNumber > 0 && multipart {
		setChecksumHeaders(hdr, partSums, "")
	}
	length := info.Size
	switch {
	case rng != nil:
		status = http.StatusPartialContent
		length = rng.Length
		hdr.Set("Content-Range", rng.ContentRange(info.Size))
		hdr.Set("Content-Length", strconv.FormatInt(length, 10))
	case emptyPart:
		length = 0
		hdr.Set("Content-Length", "0")
	}
	for k, v := range overrides {
		hdr.Set(k, v)
	}
	setActiveContentHeaders(hdr)
	if !withBody || length == 0 {
		w.WriteHeader(status)
		return nil
	}
	if rng != nil && rng.Start > 0 {
		if _, err := body.Seek(rng.Start, io.SeekStart); err != nil {
			return err
		}
	}
	w.WriteHeader(status)
	n, err := copyBuffered(writerOnly{w}, io.LimitReader(body, length))
	if err == nil && n < length {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		// The status line is out: the only way to signal the failure is to
		// abort the connection so the client sees a truncated response.
		s.log.LogAttrs(ctx, slog.LevelWarn, "s3: object stream interrupted",
			slog.String("request_id", rc.requestID), slog.String("bucket", rc.bucket),
			slog.String("key", rc.key), slog.Int64("sent", n), slog.String("error", err.Error()))
		panic(http.ErrAbortHandler)
	}
	return nil
}

// setActiveContentHeaders neutralises active content in object responses.
// Objects are served from the same origin (scheme://host:port) as the
// embedded web UI, so an uploaded text/html or image/svg+xml object opened
// in a browser would otherwise run script with the UI's origin and could
// read its stored connections and credentials. "Content-Security-Policy:
// sandbox" gives such a document a unique opaque origin with scripts,
// forms and plugins disabled; it does not affect images, media or
// downloads. PDFs are exempt (browsers refuse to render them in a
// sandbox), so they are always served as downloads instead:
// "Content-Disposition: attachment" replaces the stored disposition and any
// response-content-disposition override (a file name given there is kept),
// so a PDF uploaded by anyone never renders inline, with its viewer's
// scripting and forms, on the UI's origin. Only a Content-Type that parses
// as exactly application/pdf is exempt, and it is then rewritten to the
// bare canonical type: browsers split the header on commas and use the
// last MIME type of the list (so "application/pdf; x=y, text/html" renders
// as HTML), which a lenient check would let through.
// X-Content-Type-Options: nosniff stops browsers from executing objects
// whose stored type is not a script or style type.
func setActiveContentHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	if isPlainPDF(h.Values("Content-Type")) {
		h.Set("Content-Type", "application/pdf")
		h.Set("Content-Disposition", attachmentDisposition(h.Get("Content-Disposition")))
		return
	}
	h.Set("Content-Security-Policy", "sandbox")
}

// attachmentDisposition returns "attachment", with the file name of the
// Content-Disposition value cd when it has a valid one.
func attachmentDisposition(cd string) string {
	if _, params, err := mime.ParseMediaType(cd); err == nil && params["filename"] != "" {
		if v := mime.FormatMediaType("attachment", map[string]string{"filename": params["filename"]}); v != "" {
			return v
		}
	}
	return "attachment"
}

// isPlainPDF reports whether a Content-Type header (all its values) is a
// single application/pdf media type without any comma.
func isPlainPDF(vals []string) bool {
	if len(vals) != 1 || strings.Contains(vals[0], ",") {
		return false
	}
	mt, _, err := mime.ParseMediaType(vals[0])
	return err == nil && mt == "application/pdf"
}

// --- DeleteObject ---

// bypassGovernanceRequested reports whether
// x-amz-bypass-governance-retention: true was sent.
func bypassGovernanceRequested(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get("X-Amz-Bypass-Governance-Retention")), "true")
}

// lockedAgainstDelete reports whether object lock forbids permanently
// deleting (or shortening the retention of) the version info. GOVERNANCE
// retention can be bypassed with x-amz-bypass-governance-retention: true
// by callers allowed s3:BypassGovernanceRetention (bypass, see
// canBypassGovernance). It does no I/O, so it can run inside a backend
// transaction (storage.ConditionalDeleter).
func (s *Server) lockedAgainstDelete(info storage.ObjectInfo, bypass bool) error {
	if info.DeleteMarker {
		return nil
	}
	if info.LegalHold {
		return errObjectLocked
	}
	if info.RetentionMode == "" || !info.RetainUntil.After(s.now()) {
		return nil
	}
	if info.RetentionMode == "GOVERNANCE" && bypass {
		return nil
	}
	return errObjectLocked
}

// canBypassGovernance reports whether the request asks for and is allowed
// to bypass GOVERNANCE retention on bucket/key.
func (s *Server) canBypassGovernance(r *http.Request, rc *reqContext, bucket, key, vid string) bool {
	return bypassGovernanceRequested(r.Header) &&
		s.authorize(r, rc, "s3:BypassGovernanceRetention", bucket, key, vid) == nil
}

// deleteObjectVersion deletes bucket/key (version vid, "" = per versioning
// semantics) after the If-Match precondition (ifMatch, "" = none) and the
// object lock checks, atomically with the delete when the backend is a
// storage.ConditionalDeleter (both built-in backends are); deleting a version that does not exist succeeds, as
// in S3.
//
// When authorization decided from the object itself (its tags, for
// s3:ExistingObjectTag conditions; see authzObject), the delete is tied to
// the version that was evaluated: if the addressed version is no longer
// that one (a protected object was written in its place, or created where
// none was found), the delete fails with AccessDenied instead of removing
// an object the decision never saw.
func (s *Server) deleteObjectVersion(ctx context.Context, r *http.Request, rc *reqContext, bi storage.BucketInfo, key, vid, ifMatch string) (storage.DeleteResult, error) {
	var res storage.DeleteResult
	var err error
	authz, pinned := rc.authzObjects[objectRef{bi.Name, key, vid}]
	if ifMatch != "" || (vid != "" && bi.ObjectLockEnabled) || pinned {
		// Authorization does I/O, so it is decided before the check runs.
		bypass := vid != "" && s.canBypassGovernance(r, rc, bi.Name, key, vid)
		var checkErr error
		check := func(info storage.ObjectInfo) error {
			checkErr = nil
			if pinned && !authorizedVersion(authz, info) {
				rc.authzReason = "object changed after authorization"
				checkErr = s3err.ErrAccessDenied
				return checkErr
			}
			if ifMatch != "" {
				if info.DeleteMarker {
					checkErr = s3err.ErrNoSuchKey.With("Key", key)
				} else if !etagListMatches(ifMatch, info.ETag) {
					checkErr = s3err.ErrPreconditionFailed.With("Condition", "If-Match")
				}
			}
			if checkErr == nil && vid != "" {
				checkErr = s.lockedAgainstDelete(info, bypass)
			}
			return checkErr
		}
		cd, isCD := s.backend.(storage.ConditionalDeleter)
		if chk, ok := s.backend.(storage.CheckedDeleter); ok && pinned && vid == "" && ifMatch == "" {
			// A key without any version still gets its delete marker in a
			// versioned bucket, atomically with the check that no object
			// appeared there (the check refuses a version that is not the
			// one authorization evaluated).
			res, err = chk.DeleteObjectChecked(ctx, bi.Name, key, vid, func(info storage.ObjectInfo, found bool) error {
				if !found {
					checkErr = nil
					return nil
				}
				return check(info)
			})
		} else if isCD {
			res, err = cd.DeleteObjectIf(ctx, bi.Name, key, vid, check)
			if pinned && vid == "" && ifMatch == "" && bi.Versioning != storage.VersioningUnversioned && errors.Is(err, storage.ErrObjectNotFound) {
				// Without storage.CheckedDeleter: the key has no versions,
				// so add the delete marker S3 creates (not atomically).
				res, err = s.backend.DeleteObject(ctx, bi.Name, key, vid)
			}
		} else {
			var info storage.ObjectInfo
			if info, err = s.backend.GetObjectInfo(ctx, bi.Name, key, vid); err == nil {
				if err = check(info); err == nil {
					res, err = s.backend.DeleteObject(ctx, bi.Name, key, vid)
				}
			}
		}
		if checkErr != nil {
			return storage.DeleteResult{}, checkErr
		}
		if ifMatch != "" && (errors.Is(err, storage.ErrObjectNotFound) || errors.Is(err, storage.ErrVersionNotFound)) {
			return storage.DeleteResult{}, s3err.ErrNoSuchKey.With("Key", key)
		}
	} else {
		res, err = s.backend.DeleteObject(ctx, bi.Name, key, vid)
	}
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) || errors.Is(err, storage.ErrVersionNotFound) {
			return storage.DeleteResult{VersionID: vid}, nil
		}
		return storage.DeleteResult{}, bucketError(err, bi.Name)
	}
	if res.VersionID == "" {
		res.VersionID = vid
	}
	return res, nil
}

// authorizedVersion reports whether cur, the stored version a mutation is
// about to apply to, is the one authorization evaluated (o, see
// authzObject): the same version with the same content and
// authorization-relevant metadata or, when authorization found no object
// (or a delete marker), still no object (a delete marker). It does no I/O,
// so it can run inside a backend transaction.
func authorizedVersion(o authzObject, cur storage.ObjectInfo) bool {
	if !o.found || o.info.DeleteMarker {
		return cur.DeleteMarker
	}
	return sameObjectVersion(o.info, cur)
}

// handleDeleteObject implements DeleteObject, including object lock
// protection of versions and conditional deletes (If-Match).
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	vid, err := objectVersionParam(rc.query)
	if err != nil {
		return err
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
	res, err := s.deleteObjectVersion(ctx, r, rc, bi, rc.key, vid, ifMatch)
	if err != nil {
		return err
	}
	hdr := w.Header()
	if res.DeleteMarker {
		hdr.Set("X-Amz-Delete-Marker", "true")
	}
	if res.VersionID != "" && (bi.Versioning != storage.VersioningUnversioned || vid != "") {
		hdr.Set("X-Amz-Version-Id", res.VersionID)
	}
	writeNoContent(w)
	return nil
}

// --- Not implemented ---

// handleRestoreObject implements RestoreObject: gos3 has no archive
// storage classes, so restores are not implemented.
func (s *Server) handleRestoreObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented.WithMessage("RestoreObject is not implemented.")
}

// handleSelectObjectContent implements SelectObjectContent (not
// implemented).
func (s *Server) handleSelectObjectContent(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	return s3err.ErrNotImplemented.WithMessage("SelectObjectContent is not implemented.")
}
