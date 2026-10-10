package s3

// Object subresources: tagging, ACL, retention, legal hold and
// GetObjectAttributes. Owned by the s3-object work item.

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// maxSubresourceBody bounds the XML bodies of the object subresource
// operations (tagging, ACL, retention, legal hold).
const maxSubresourceBody = 64 << 10

// readValidatedBody reads a small request body of at most max bytes,
// validating Content-MD5 and x-amz-checksum-* headers or trailers when the
// client sent them (BadDigest / InvalidDigest on mismatch). Larger bodies
// yield MaxMessageLengthExceeded.
func (s *Server) readValidatedBody(r *http.Request, rc *reqContext, max int64) ([]byte, error) {
	if n := rc.contentLength(); n > max {
		return nil, s3err.ErrMaxMessageLengthExceeded
	}
	p, err := s.newPayloadReader(r, rc, payloadOptions{MaxSize: max + 1})
	if err != nil {
		return nil, err
	}
	return readBody(p, max)
}

// decodeValidatedXML reads a validated body (see readValidatedBody) and
// unmarshals it into v (MissingRequestBodyError / MalformedXML).
func (s *Server) decodeValidatedXML(r *http.Request, rc *reqContext, max int64, v any) error {
	data, err := s.readValidatedBody(r, rc, max)
	if err != nil {
		return err
	}
	return decodeXML(data, v)
}

// subresourceTarget resolves the version addressed by a subresource
// request (versionId validated, delete markers per S3 rules) and reports
// whether the bucket is versioned.
func (s *Server) subresourceTarget(w http.ResponseWriter, r *http.Request, rc *reqContext) (storage.ObjectInfo, bool, error) {
	ctx := r.Context()
	vid, err := objectVersionParam(rc.query)
	if err != nil {
		return storage.ObjectInfo{}, false, err
	}
	versioned, err := s.isVersioned(ctx, rc)
	if err != nil {
		return storage.ObjectInfo{}, false, err
	}
	info, _, err := s.objectForRead(ctx, w, r, rc, vid, false)
	return info, versioned, err
}

// updateObjectMeta applies fn to the stored metadata of info's version.
func (s *Server) updateObjectMeta(r *http.Request, rc *reqContext, info storage.ObjectInfo, fn func(*storage.ObjectMeta) error) (storage.ObjectInfo, error) {
	out, err := s.backend.UpdateObjectMeta(r.Context(), rc.bucket, rc.key, info.VersionID, fn)
	if err != nil {
		return out, objectLookupError(err, rc.bucket, rc.key, rc.versionID())
	}
	return out, nil
}

// setVersionHeader sets x-amz-version-id when the bucket is versioned.
func setVersionHeader(w http.ResponseWriter, versioned bool, vid string) {
	if versioned && vid != "" {
		w.Header().Set("X-Amz-Version-Id", vid)
	}
}

// --- Tagging ---

// handleGetObjectTagging implements GetObjectTagging.
func (s *Server) handleGetObjectTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	info, versioned, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	setVersionHeader(w, versioned, info.VersionID)
	return writeXML(w, http.StatusOK, taggingFromMap(info.Tags))
}

// handlePutObjectTagging implements PutObjectTagging (<= 10 tags). The op
// is authorized here, after the (small) body is decoded, because its
// s3:RequestObjectTag/<key> and s3:RequestObjectTagKeys condition keys come
// from the tag set in the body, as in AWS.
func (s *Server) handlePutObjectTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	var doc Tagging
	if err := s.decodeValidatedXML(r, rc, maxSubresourceBody, &doc); err != nil {
		return err
	}
	tags, err := validateObjectTags(doc.TagSet.Tags)
	if err != nil {
		return err
	}
	if err := s.authorizeOpWith(r.Context(), r, rc, requestTagConditions(tags)); err != nil {
		return err
	}
	info, versioned, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		tags = nil
	}
	if _, err := s.updateObjectMeta(r, rc, info, func(m *storage.ObjectMeta) error {
		m.Tags = tags
		return nil
	}); err != nil {
		return err
	}
	setVersionHeader(w, versioned, info.VersionID)
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleDeleteObjectTagging implements DeleteObjectTagging.
func (s *Server) handleDeleteObjectTagging(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	info, versioned, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	if _, err := s.updateObjectMeta(r, rc, info, func(m *storage.ObjectMeta) error {
		m.Tags = nil
		return nil
	}); err != nil {
		return err
	}
	setVersionHeader(w, versioned, info.VersionID)
	writeNoContent(w)
	return nil
}

// --- ACL ---

// handleGetObjectAcl implements GetObjectAcl: the object's canned ACL is
// rendered as an AccessControlPolicy owned by the bucket owner. When ACLs
// are disabled (BucketOwnerEnforced) only the owner grant is reported.
func (s *Server) handleGetObjectAcl(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	info, _, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	raw, err := s.bucketConfig(ctx, rc.bucket, storage.ConfigOwnership)
	if err != nil {
		return err
	}
	canned := info.ACL
	if aclsDisabledByOwnership(raw) {
		canned = ""
	}
	setVersionHeader(w, bi.Versioning != storage.VersioningUnversioned, info.VersionID)
	return writeXML(w, http.StatusOK, aclPolicyForCanned(canned, ownerFor(bi.Owner)))
}

// handlePutObjectAcl implements PutObjectAcl with a canned ACL
// (x-amz-acl) or an AccessControlPolicy body equivalent to one of the
// canned ACLs private, public-read, public-read-write or
// authenticated-read; other grant sets are NotImplemented.
func (s *Server) handlePutObjectAcl(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	h := r.Header
	if err := checkGrantHeaders(h); err != nil {
		return err
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	info, _, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	var acl string
	if v := h.Get("X-Amz-Acl"); v != "" {
		if acl, err = validateCannedACL(v, false); err != nil {
			return err
		}
		data, err := s.readValidatedBody(r, rc, maxSubresourceBody)
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			return s3err.ErrUnexpectedContent.WithMessage("This request does not support content when x-amz-acl is specified.")
		}
	} else {
		data, err := s.readValidatedBody(r, rc, maxSubresourceBody)
		if err != nil {
			return err
		}
		var p AccessControlPolicy
		if err := decodeXML(data, &p); err != nil {
			if err == s3err.ErrMalformedXML {
				return s3err.ErrMalformedACL
			}
			return err
		}
		owner := ownerFor(bi.Owner)
		if acl, err = cannedFromGrants(&p, owner.ID, ownerFor(rc.userName()).ID); err != nil {
			return err
		}
	}
	if err := s.checkObjectACLAllowed(ctx, rc.bucket, acl); err != nil {
		return err
	}
	if _, err := s.updateObjectMeta(r, rc, info, func(m *storage.ObjectMeta) error {
		m.ACL = acl
		return nil
	}); err != nil {
		return err
	}
	setVersionHeader(w, bi.Versioning != storage.VersioningUnversioned, info.VersionID)
	writeEmpty(w, http.StatusOK)
	return nil
}

// cannedFromGrants maps the grants of an AccessControlPolicy to the
// canned ACL granting exactly the same permissions ("" = private).
// Grants to the owner (any of ownerIDs) are implied and accepted; any
// other grantee or permission combination is NotImplemented, since gos3
// only stores canned ACLs.
func cannedFromGrants(p *AccessControlPolicy, ownerIDs ...string) (string, error) {
	notImpl := s3err.ErrNotImplemented.WithMessage("Only ACLs equivalent to the private, public-read, public-read-write or authenticated-read canned ACLs are supported.")
	var allRead, allWrite, authRead bool
	for _, g := range p.Grants {
		perm := strings.ToUpper(strings.TrimSpace(g.Permission))
		switch perm {
		case "FULL_CONTROL", "READ", "WRITE", "READ_ACP", "WRITE_ACP":
		default:
			return "", s3err.ErrMalformedACL
		}
		gt := g.Grantee
		typ := gt.Type
		if typ == "" {
			switch {
			case gt.URI != "":
				typ = "Group"
			case gt.ID != "":
				typ = "CanonicalUser"
			case gt.EmailAddress != "":
				typ = "AmazonCustomerByEmail"
			}
		}
		switch typ {
		case "CanonicalUser":
			isOwner := false
			for _, id := range ownerIDs {
				if strings.TrimSpace(gt.ID) == id {
					isOwner = true
				}
			}
			if !isOwner {
				return "", notImpl
			}
		case "Group":
			switch uri := strings.TrimSpace(gt.URI); {
			case uri == groupAllUsers && perm == "READ":
				allRead = true
			case uri == groupAllUsers && perm == "WRITE":
				allWrite = true
			case uri == groupAuthenticatedUsers && perm == "READ":
				authRead = true
			default:
				return "", notImpl
			}
		case "AmazonCustomerByEmail":
			return "", notImpl
		default:
			return "", s3err.ErrMalformedACL
		}
	}
	switch {
	case allRead && allWrite && !authRead:
		return aclPublicReadWrite, nil
	case allRead && !allWrite && !authRead:
		return aclPublicRead, nil
	case authRead && !allRead && !allWrite:
		return aclAuthenticatedRead, nil
	case !allRead && !allWrite && !authRead:
		return "", nil
	}
	return "", notImpl
}

// --- Retention and legal hold ---

// objectRetention is the <Retention> document.
type objectRetention struct {
	XMLName         xml.Name `xml:"Retention"`
	XMLNS           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

// objectLegalHold is the <LegalHold> document.
type objectLegalHold struct {
	XMLName xml.Name `xml:"LegalHold"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

// errNoObjectRetention is returned when an object has no retention.
var errNoObjectRetention = s3err.ErrNoSuchObjectLegalHold.WithMessage("The specified object does not have a ObjectLock configuration")

// requireBucketObjectLock returns InvalidRequest unless object lock is
// enabled on the addressed bucket.
func (s *Server) requireBucketObjectLock(r *http.Request, rc *reqContext) error {
	bi, err := s.bucketInfo(r.Context(), rc)
	if err != nil {
		return err
	}
	if !bi.ObjectLockEnabled {
		return s3err.ErrInvalidRequest.WithMessage("Bucket is missing Object Lock Configuration")
	}
	return nil
}

// handleGetObjectRetention implements GetObjectRetention.
func (s *Server) handleGetObjectRetention(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	if err := s.requireBucketObjectLock(r, rc); err != nil {
		return err
	}
	info, _, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	if info.RetentionMode == "" || info.RetainUntil.IsZero() {
		return errNoObjectRetention
	}
	return writeXML(w, http.StatusOK, objectRetention{
		XMLNS: s3Namespace, Mode: info.RetentionMode, RetainUntilDate: formatXMLTime(info.RetainUntil),
	})
}

// handlePutObjectRetention implements PutObjectRetention. Active
// COMPLIANCE retention can only be extended; active GOVERNANCE retention
// can only be extended unless the caller bypasses governance
// (x-amz-bypass-governance-retention: true + s3:BypassGovernanceRetention).
// An empty <Retention/> removes the retention (same rules).
//
// The op is authorized here, after the (small) body is decoded, because
// its s3:object-lock-mode, s3:object-lock-retain-until-date and
// s3:object-lock-remaining-retention-days condition keys come from the
// body, as in AWS; request headers do not count.
func (s *Server) handlePutObjectRetention(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	var doc objectRetention
	if err := s.decodeValidatedXML(r, rc, maxSubresourceBody, &doc); err != nil {
		return err
	}
	mode := strings.TrimSpace(doc.Mode)
	dateStr := strings.TrimSpace(doc.RetainUntilDate)
	var until time.Time
	switch {
	case mode == "" && dateStr == "":
	case mode == "" || dateStr == "":
		return s3err.ErrMalformedXML
	case mode != "GOVERNANCE" && mode != "COMPLIANCE":
		return s3err.ErrMalformedXML
	default:
		t, ok := parseISOTime(dateStr)
		if !ok {
			return s3err.ErrInvalidArgument.WithMessage("The retain until date must be provided in ISO 8601 format").
				With("ArgumentName", "RetainUntilDate").With("ArgumentValue", truncateValue(dateStr))
		}
		if !t.After(s.now()) {
			return s3err.ErrInvalidArgument.WithMessage("The retain until date must be in the future!").
				With("ArgumentName", "RetainUntilDate").With("ArgumentValue", dateStr)
		}
		until = t
	}
	attrs := map[string][]string{}
	if mode != "" {
		attrs = retentionConditions(mode, until, s.now())
	}
	if err := s.authorizeOpWith(r.Context(), r, rc, attrs); err != nil {
		return err
	}
	if err := s.requireBucketObjectLock(r, rc); err != nil {
		return err
	}
	info, _, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	bypass := s.canBypassGovernance(r, rc, rc.bucket, rc.key, info.VersionID)
	now := s.now()
	if _, err := s.updateObjectMeta(r, rc, info, func(m *storage.ObjectMeta) error {
		if m.RetentionMode != "" && m.RetainUntil.After(now) {
			extends := mode == m.RetentionMode && !until.Before(m.RetainUntil)
			switch {
			case extends:
			case m.RetentionMode == "GOVERNANCE" && bypass:
			default:
				return errObjectLocked
			}
		}
		m.RetentionMode, m.RetainUntil = mode, until
		return nil
	}); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// handleGetObjectLegalHold implements GetObjectLegalHold.
func (s *Server) handleGetObjectLegalHold(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	if err := s.requireBucketObjectLock(r, rc); err != nil {
		return err
	}
	info, _, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	status := "OFF"
	if info.LegalHold {
		status = "ON"
	}
	return writeXML(w, http.StatusOK, objectLegalHold{XMLNS: s3Namespace, Status: status})
}

// handlePutObjectLegalHold implements PutObjectLegalHold (ON / OFF).
// The op is authorized after the body is decoded: its
// s3:object-lock-legal-hold condition key is the status in the body.
func (s *Server) handlePutObjectLegalHold(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	var doc objectLegalHold
	if err := s.decodeValidatedXML(r, rc, maxSubresourceBody, &doc); err != nil {
		return err
	}
	var on bool
	switch strings.TrimSpace(doc.Status) {
	case "ON":
		on = true
	case "OFF":
	default:
		return s3err.ErrMalformedXML
	}
	attrs := map[string][]string{"s3:object-lock-legal-hold": {legalHoldStatus(on)}}
	if err := s.authorizeOpWith(r.Context(), r, rc, attrs); err != nil {
		return err
	}
	if err := s.requireBucketObjectLock(r, rc); err != nil {
		return err
	}
	info, _, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	if _, err := s.updateObjectMeta(r, rc, info, func(m *storage.ObjectMeta) error {
		m.LegalHold = on
		return nil
	}); err != nil {
		return err
	}
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- GetObjectAttributes ---

// objectAttributesResponse is the GetObjectAttributes response body.
type objectAttributesResponse struct {
	XMLName      xml.Name               `xml:"http://s3.amazonaws.com/doc/2006-03-01/ GetObjectAttributesResponse"`
	ETag         string                 `xml:"ETag,omitempty"`
	Checksum     *attributesChecksum    `xml:"Checksum,omitempty"`
	ObjectParts  *attributesObjectParts `xml:"ObjectParts,omitempty"`
	StorageClass string                 `xml:"StorageClass,omitempty"`
	ObjectSize   *int64                 `xml:"ObjectSize,omitempty"`
}

// attributesChecksum holds an object's or part's checksums.
type attributesChecksum struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
	ChecksumType      string `xml:"ChecksumType,omitempty"`
}

// set fills the checksum fields from an algorithm -> value map.
func (c *attributesChecksum) set(sums map[string]string) {
	c.ChecksumCRC32 = sums[ChecksumCRC32]
	c.ChecksumCRC32C = sums[ChecksumCRC32C]
	c.ChecksumCRC64NVME = sums[ChecksumCRC64NVME]
	c.ChecksumSHA1 = sums[ChecksumSHA1]
	c.ChecksumSHA256 = sums[ChecksumSHA256]
}

// attributesObjectParts is the <ObjectParts> element.
type attributesObjectParts struct {
	TotalPartsCount      int              `xml:"PartsCount"`
	PartNumberMarker     int              `xml:"PartNumberMarker"`
	NextPartNumberMarker int              `xml:"NextPartNumberMarker"`
	MaxParts             int              `xml:"MaxParts"`
	IsTruncated          bool             `xml:"IsTruncated"`
	Parts                []attributesPart `xml:"Part"`
}

// attributesPart is one <Part> of <ObjectParts>.
type attributesPart struct {
	PartNumber int   `xml:"PartNumber"`
	Size       int64 `xml:"Size"`
	attributesChecksumFields
}

// attributesChecksumFields are the per-part checksum elements.
type attributesChecksumFields struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
}

// objectAttributeNames are the valid x-amz-object-attributes values.
var objectAttributeNames = map[string]string{
	"etag": "ETag", "checksum": "Checksum", "objectparts": "ObjectParts",
	"storageclass": "StorageClass", "objectsize": "ObjectSize",
}

// parseObjectAttributes parses the x-amz-object-attributes header(s).
func parseObjectAttributes(h http.Header) (map[string]bool, error) {
	want := map[string]bool{}
	for _, v := range h.Values("X-Amz-Object-Attributes") {
		for _, a := range strings.Split(v, ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			name, ok := objectAttributeNames[strings.ToLower(a)]
			if !ok {
				return nil, s3err.ErrInvalidArgument.WithMessage("Invalid attribute name specified.").
					With("ArgumentName", "x-amz-object-attributes").With("ArgumentValue", truncateValue(a))
			}
			want[name] = true
		}
	}
	if len(want) == 0 {
		return nil, s3err.ErrInvalidArgument.WithMessage("Missing required header for this request: x-amz-object-attributes").
			With("ArgumentName", "x-amz-object-attributes")
	}
	return want, nil
}

// handleGetObjectAttributes implements GetObjectAttributes (ETag,
// Checksum, ObjectParts with x-amz-max-parts / x-amz-part-number-marker,
// StorageClass, ObjectSize).
func (s *Server) handleGetObjectAttributes(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	h := r.Header
	if err := checkSSECHeaders(h); err != nil {
		return err
	}
	want, err := parseObjectAttributes(h)
	if err != nil {
		return err
	}
	hv := map[string][]string{}
	for _, name := range []string{"X-Amz-Max-Parts", "X-Amz-Part-Number-Marker"} {
		if v := h.Get(name); v != "" {
			hv[strings.ToLower(name)] = []string{v}
		}
	}
	maxParts, err := parseMaxKeys(hv, "x-amz-max-parts", 1000, 1000)
	if err != nil {
		return err
	}
	marker, err := parseMaxKeys(hv, "x-amz-part-number-marker", 0, maxPartNumber)
	if err != nil {
		return err
	}
	info, versioned, err := s.subresourceTarget(w, r, rc)
	if err != nil {
		return err
	}
	if err := checkConditions(h, info.ETag, info.LastModified); err != nil {
		if errors.Is(err, s3err.ErrNotModified) {
			w.Header().Set("ETag", quoteETag(info.ETag))
			w.Header().Set("Last-Modified", formatHTTPTime(info.LastModified))
		}
		return err
	}

	resp := objectAttributesResponse{}
	if want["ETag"] {
		resp.ETag = trimETag(info.ETag)
	}
	if want["Checksum"] && len(info.Checksums) > 0 {
		c := &attributesChecksum{ChecksumType: info.ChecksumType}
		c.set(info.Checksums)
		resp.Checksum = c
	}
	if want["ObjectParts"] && len(info.PartSizes) > 0 {
		op := &attributesObjectParts{TotalPartsCount: len(info.PartSizes), PartNumberMarker: marker, MaxParts: maxParts}
		for i := marker; i < len(info.PartSizes); i++ {
			if len(op.Parts) >= maxParts {
				op.IsTruncated = true
				break
			}
			p := attributesPart{PartNumber: i + 1, Size: info.PartSizes[i]}
			if i < len(info.PartChecksums) {
				sums := info.PartChecksums[i]
				p.ChecksumCRC32, p.ChecksumCRC32C, p.ChecksumCRC64NVME = sums[ChecksumCRC32], sums[ChecksumCRC32C], sums[ChecksumCRC64NVME]
				p.ChecksumSHA1, p.ChecksumSHA256 = sums[ChecksumSHA1], sums[ChecksumSHA256]
			}
			op.Parts = append(op.Parts, p)
			op.NextPartNumberMarker = i + 1
		}
		resp.ObjectParts = op
	}
	if want["StorageClass"] {
		resp.StorageClass = storageClassOrStandard(info.StorageClass)
	}
	if want["ObjectSize"] {
		size := info.Size
		resp.ObjectSize = &size
	}
	hdr := w.Header()
	hdr.Set("Last-Modified", formatHTTPTime(info.LastModified))
	setVersionHeader(w, versioned, info.VersionID)
	return writeXML(w, http.StatusOK, resp)
}
