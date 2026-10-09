package s3

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// userMetaPrefix is the lowercase prefix of user metadata headers.
const userMetaPrefix = "x-amz-meta-"

// objectMetaFromHeaders builds the ObjectMeta of a new object (PutObject,
// CopyObject with REPLACE, CreateMultipartUpload, PostObject fields) from
// request headers, validating them:
//
//   - Content-Type, Content-Encoding (aws-chunked already stripped by the
//     core), Content-Disposition, Content-Language, Cache-Control, Expires
//     are stored verbatim;
//   - x-amz-meta-* become UserMeta (lowercased names without the prefix,
//     repeated headers joined with ","); names+values above 2 KiB =>
//     MetadataTooLarge;
//   - x-amz-storage-class is validated (InvalidStorageClass), STANDARD => "";
//   - x-amz-tagging is parsed as URL query and validated (InvalidArgument /
//     InvalidTag);
//   - x-amz-acl is validated as an object canned ACL, private => ""; grant
//     headers (x-amz-grant-*) => NotImplemented;
//   - x-amz-server-side-encryption AES256 / aws:kms / aws:kms:dsse and the
//     KMS key ID are recorded; SSE-C headers => NotImplemented;
//   - x-amz-object-lock-mode / -retain-until-date (both or neither, date in
//     the future) / -legal-hold (ON|OFF) => InvalidArgument when invalid;
//     whether the bucket has object lock is checked by requireObjectLock;
//   - x-amz-website-redirect-location must start with "/", "http://" or
//     "https://".
//
// ETag, checksums and part information are set by the handler.
func (s *Server) objectMetaFromHeaders(h http.Header) (storage.ObjectMeta, error) {
	var m storage.ObjectMeta
	m.ContentType = h.Get("Content-Type")
	m.ContentEncoding = h.Get("Content-Encoding")
	m.ContentDisposition = h.Get("Content-Disposition")
	m.ContentLanguage = h.Get("Content-Language")
	m.CacheControl = h.Get("Cache-Control")
	m.Expires = h.Get("Expires")

	meta, err := userMetaFromHeaders(h)
	if err != nil {
		return m, err
	}
	m.UserMeta = meta

	if m.StorageClass, err = normalizeStorageClass(h.Get("X-Amz-Storage-Class")); err != nil {
		return m, err
	}
	if v := h.Get("X-Amz-Tagging"); v != "" {
		if m.Tags, err = parseTaggingHeader(v); err != nil {
			return m, err
		}
	}
	if err := checkGrantHeaders(h); err != nil {
		return m, err
	}
	if m.ACL, err = validateCannedACL(h.Get("X-Amz-Acl"), false); err != nil {
		return m, err
	}
	if m.SSE, m.SSEKMSKeyID, err = sseFromHeaders(h); err != nil {
		return m, err
	}
	if err := s.objectLockFromHeaders(h, &m); err != nil {
		return m, err
	}
	if v := h.Get("X-Amz-Website-Redirect-Location"); v != "" {
		if len(v) > maxRedirectLength || !(strings.HasPrefix(v, "/") || strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://")) {
			return m, s3err.ErrInvalidArgument.WithMessage("The website redirect location must have a prefix of 'http://' or 'https://' or '/'").
				With("ArgumentName", "x-amz-website-redirect-location").With("ArgumentValue", v)
		}
		m.WebsiteRedirect = v
	}
	return m, nil
}

// userMetaFromHeaders extracts x-amz-meta-* headers (nil when none).
func userMetaFromHeaders(h http.Header) (map[string]string, error) {
	var meta map[string]string
	size := 0
	for k, vs := range h {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, userMetaPrefix) || len(lk) == len(userMetaPrefix) {
			continue
		}
		name := lk[len(userMetaPrefix):]
		v := strings.Join(vs, ",")
		size += len(name) + len(v)
		if size > maxUserMetaSize {
			return nil, s3err.ErrMetadataTooLarge.With("MaxSizeAllowed", strconv.Itoa(maxUserMetaSize))
		}
		if meta == nil {
			meta = make(map[string]string)
		}
		if old, ok := meta[name]; ok { // same name in different case
			v = old + "," + v
		}
		meta[name] = v
	}
	return meta, nil
}

// checkGrantHeaders rejects explicit ACL grant headers (only canned ACLs
// are supported).
func checkGrantHeaders(h http.Header) error {
	for _, k := range []string{"X-Amz-Grant-Read", "X-Amz-Grant-Write", "X-Amz-Grant-Read-Acp", "X-Amz-Grant-Write-Acp", "X-Amz-Grant-Full-Control"} {
		if h.Get(k) != "" {
			return s3err.ErrNotImplemented.WithMessage("ACL grant headers are not supported; use a canned ACL (x-amz-acl).")
		}
	}
	return nil
}

// sseFromHeaders validates the server-side encryption headers.
func sseFromHeaders(h http.Header) (sse, kmsKeyID string, err error) {
	if err := checkSSECHeaders(h); err != nil {
		return "", "", err
	}
	sse = strings.TrimSpace(h.Get("X-Amz-Server-Side-Encryption"))
	switch sse {
	case "", "AES256", "aws:kms", "aws:kms:dsse":
	default:
		return "", "", s3err.ErrInvalidArgument.WithMessage("The encryption method specified is not supported").
			With("ArgumentName", "x-amz-server-side-encryption").With("ArgumentValue", sse)
	}
	kmsKeyID = strings.TrimSpace(h.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"))
	if kmsKeyID != "" && !strings.HasPrefix(sse, "aws:kms") {
		return "", "", s3err.ErrInvalidArgument.WithMessage("Server Side Encryption with AWS KMS managed key requires HTTP header x-amz-server-side-encryption : aws:kms").
			With("ArgumentName", "x-amz-server-side-encryption")
	}
	return sse, kmsKeyID, nil
}

// checkSSECHeaders rejects SSE-C (customer-provided key) headers, for the
// request and for a copy source; gos3 does not implement SSE-C. GetObject,
// HeadObject, UploadPart and the copy handlers call it too.
func checkSSECHeaders(h http.Header) error {
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-server-side-encryption-customer-") ||
			strings.HasPrefix(lk, "x-amz-copy-source-server-side-encryption-customer-") {
			return s3err.ErrNotImplemented.WithMessage("Server-side encryption with customer-provided keys (SSE-C) is not supported.")
		}
	}
	return nil
}

// objectLockFromHeaders validates the object lock headers into m.
func (s *Server) objectLockFromHeaders(h http.Header, m *storage.ObjectMeta) error {
	mode := strings.TrimSpace(h.Get("X-Amz-Object-Lock-Mode"))
	until := strings.TrimSpace(h.Get("X-Amz-Object-Lock-Retain-Until-Date"))
	if (mode == "") != (until == "") {
		return s3err.ErrInvalidArgument.WithMessage("x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied")
	}
	if mode != "" {
		if mode != "GOVERNANCE" && mode != "COMPLIANCE" {
			return s3err.ErrInvalidArgument.WithMessage("Unknown wormMode directive.").
				With("ArgumentName", "x-amz-object-lock-mode").With("ArgumentValue", mode)
		}
		t, ok := parseISOTime(until)
		if !ok {
			return s3err.ErrInvalidArgument.WithMessage("The retain until date must be provided in ISO 8601 format").
				With("ArgumentName", "x-amz-object-lock-retain-until-date").With("ArgumentValue", until)
		}
		if !t.After(s.now()) {
			return s3err.ErrInvalidArgument.WithMessage("The retain until date must be in the future!").
				With("ArgumentName", "x-amz-object-lock-retain-until-date").With("ArgumentValue", until)
		}
		m.RetentionMode, m.RetainUntil = mode, t
	}
	switch v := strings.TrimSpace(h.Get("X-Amz-Object-Lock-Legal-Hold")); v {
	case "":
	case "ON":
		m.LegalHold = true
	case "OFF":
		m.LegalHold = false
	default:
		return s3err.ErrInvalidArgument.WithMessage("Legal Hold must be either of 'ON' or 'OFF'").
			With("ArgumentName", "x-amz-object-lock-legal-hold").With("ArgumentValue", v)
	}
	return nil
}

// requireObjectLock returns InvalidRequest when meta asks for object lock
// (retention or legal hold) but the bucket does not have it enabled.
func requireObjectLock(meta storage.ObjectMeta, bi storage.BucketInfo) error {
	if (meta.RetentionMode != "" || meta.LegalHold) && !bi.ObjectLockEnabled {
		return s3err.ErrInvalidRequest.WithMessage("Bucket is missing Object Lock Configuration")
	}
	return nil
}

// checksumModeEnabled reports whether x-amz-checksum-mode: ENABLED is set.
func checksumModeEnabled(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get("X-Amz-Checksum-Mode")), "ENABLED")
}

// writeObjectHeaders sets the response headers describing an object
// version (GetObject, HeadObject): Content-Length (full size; ranged
// responses override it), ETag, Last-Modified, Accept-Ranges, Content-Type
// (binary/octet-stream when unset), Content-Encoding/-Disposition/-Language,
// Cache-Control, Expires, x-amz-meta-*, x-amz-version-id (when versioned),
// x-amz-tagging-count, x-amz-storage-class (omitted for STANDARD),
// x-amz-server-side-encryption (+ KMS key ID), x-amz-object-lock-*,
// x-amz-mp-parts-count (multipart objects), x-amz-website-redirect-location
// and, when checksumMode, x-amz-checksum-<algo> + x-amz-checksum-type.
func writeObjectHeaders(w http.ResponseWriter, info storage.ObjectInfo, versioned, checksumMode bool) {
	h := w.Header()
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if info.ETag != "" {
		h.Set("ETag", quoteETag(info.ETag))
	}
	h.Set("Last-Modified", formatHTTPTime(info.LastModified))
	h.Set("Accept-Ranges", "bytes")
	ct := info.ContentType
	if ct == "" {
		ct = "binary/octet-stream"
	}
	h.Set("Content-Type", ct)
	set := func(k, v string) {
		if v != "" {
			h.Set(k, v)
		}
	}
	set("Content-Encoding", info.ContentEncoding)
	set("Content-Disposition", info.ContentDisposition)
	set("Content-Language", info.ContentLanguage)
	set("Cache-Control", info.CacheControl)
	set("Expires", info.Expires)
	names := make([]string, 0, len(info.UserMeta))
	for k := range info.UserMeta {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		h.Set(userMetaPrefix+k, info.UserMeta[k])
	}
	if versioned {
		set("X-Amz-Version-Id", info.VersionID)
	}
	if n := len(info.Tags); n > 0 {
		h.Set("X-Amz-Tagging-Count", strconv.Itoa(n))
	}
	if info.StorageClass != "" && info.StorageClass != "STANDARD" {
		h.Set("X-Amz-Storage-Class", info.StorageClass)
	}
	set("X-Amz-Server-Side-Encryption", info.SSE)
	if strings.HasPrefix(info.SSE, "aws:kms") {
		set("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id", info.SSEKMSKeyID)
	}
	if info.RetentionMode != "" && !info.RetainUntil.IsZero() {
		h.Set("X-Amz-Object-Lock-Mode", info.RetentionMode)
		h.Set("X-Amz-Object-Lock-Retain-Until-Date", formatXMLTime(info.RetainUntil))
	}
	if info.LegalHold {
		h.Set("X-Amz-Object-Lock-Legal-Hold", "ON")
	}
	if n := len(info.PartSizes); n > 0 {
		h.Set("X-Amz-Mp-Parts-Count", strconv.Itoa(n))
	}
	set("X-Amz-Website-Redirect-Location", info.WebsiteRedirect)
	if checksumMode && len(info.Checksums) > 0 {
		for _, algo := range checksumAlgorithms {
			if v := info.Checksums[algo]; v != "" {
				h.Set(checksumHeader(algo), v)
			}
		}
		set("X-Amz-Checksum-Type", info.ChecksumType)
	}
}
