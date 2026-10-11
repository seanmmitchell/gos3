package s3

// CopyObject and DeleteObjects. Owned by the s3-object work item.

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"hash"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- CopyObject ---

// copyObjectResult is the CopyObject response body.
type copyObjectResult struct {
	XMLName           xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyObjectResult"`
	ETag              string   `xml:"ETag"`
	LastModified      string   `xml:"LastModified"`
	ChecksumType      string   `xml:"ChecksumType,omitempty"`
	ChecksumCRC32     string   `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string   `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string   `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string   `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string   `xml:"ChecksumSHA256,omitempty"`
}

// Directive values of x-amz-metadata-directive / x-amz-tagging-directive.
const (
	directiveCopy    = "COPY"
	directiveReplace = "REPLACE"
)

// parseDirective validates a COPY|REPLACE directive header (default COPY).
func parseDirective(h http.Header, name string) (string, error) {
	v := trimHeaderSpace(h.Get(name))
	switch strings.ToUpper(v) {
	case "", directiveCopy:
		return directiveCopy, nil
	case directiveReplace:
		return directiveReplace, nil
	}
	what := "metadata"
	if strings.Contains(strings.ToLower(name), "tagging") {
		what = "tagging"
	}
	return "", s3err.ErrInvalidArgument.WithMessage("Unknown %s directive.", what).
		With("ArgumentName", strings.ToLower(name)).With("ArgumentValue", truncateValue(v))
}

// sourceChecksumAlgorithm returns the algorithm of a source object's
// stored checksum (canonical order), or "".
func sourceChecksumAlgorithm(sums map[string]string) string {
	for _, a := range checksumAlgorithms {
		if sums[a] != "" {
			return a
		}
	}
	return ""
}

// copyHasher computes the MD5 and one checksum of copied data.
type copyHasher struct {
	md5  hash.Hash
	sum  hash.Hash
	algo string
	n    int64
}

func (c *copyHasher) Write(p []byte) (int, error) {
	c.md5.Write(p)
	c.sum.Write(p)
	c.n += int64(len(p))
	return len(p), nil
}

// handleCopyObject implements CopyObject: the source (authorized with
// s3:GetObject / s3:GetObjectVersion) is streamed into a new version of the
// destination while its MD5 and checksum are recomputed. Metadata and tags
// follow x-amz-metadata-directive / x-amz-tagging-directive; storage class,
// ACL, encryption, object lock and website redirect always come from the
// request (and bucket defaults), as in S3.
func (s *Server) handleCopyObject(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	h := r.Header
	if err := checkSSECHeaders(h); err != nil {
		return err
	}
	src, err := parseCopySource(h.Get("X-Amz-Copy-Source"))
	if err != nil {
		return err
	}
	if h.Get("X-Amz-Copy-Source-Range") != "" {
		return s3err.ErrInvalidArgument.WithMessage("The x-amz-copy-source-range header is only supported for UploadPartCopy.").
			With("ArgumentName", "x-amz-copy-source-range")
	}
	metaDir, err := parseDirective(h, "X-Amz-Metadata-Directive")
	if err != nil {
		return err
	}
	tagDir, err := parseDirective(h, "X-Amz-Tagging-Directive")
	if err != nil {
		return err
	}
	algo := ""
	if v := h.Get("X-Amz-Checksum-Algorithm"); v != "" {
		var ok bool
		if algo, ok = normalizeChecksumAlgorithm(v); !ok {
			return invalidChecksumValue("x-amz-checksum-algorithm")
		}
	}
	hdrMeta, err := s.objectMetaFromHeaders(h)
	if err != nil {
		return err
	}
	cond, err := parseWriteConditions(h)
	if err != nil {
		return err
	}
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	srcBucket, info, rd, err := s.openCopySource(r, rc, src)
	if err != nil {
		return err
	}
	defer rd.Close()
	if info.Size > s.maxObjectSize {
		return s3err.ErrInvalidRequest.WithMessage("The specified copy source is larger than the maximum allowable size for a copy source: %d", s.maxObjectSize)
	}
	if src.Bucket == rc.bucket && src.Key == rc.key && (src.VersionID == "" || info.IsLatest) &&
		metaDir != directiveReplace && tagDir != directiveReplace && algo == "" &&
		h.Get("X-Amz-Storage-Class") == "" && h.Get("X-Amz-Server-Side-Encryption") == "" &&
		h.Get("X-Amz-Website-Redirect-Location") == "" {
		return s3err.ErrInvalidRequest.WithMessage("This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
	}

	meta := hdrMeta
	if metaDir == directiveCopy {
		meta.ContentType = info.ContentType
		meta.ContentEncoding = info.ContentEncoding
		meta.ContentDisposition = info.ContentDisposition
		meta.ContentLanguage = info.ContentLanguage
		meta.CacheControl = info.CacheControl
		meta.Expires = info.Expires
		meta.UserMeta = maps.Clone(info.UserMeta)
	}
	if tagDir == directiveCopy {
		meta.Tags = maps.Clone(info.Tags)
	}
	opts := headerObjectOpts(h)
	// The new object's tags, whether given (REPLACE) or copied from the
	// source (COPY), need s3:PutObjectTagging with them as the request
	// tags; copying the source's tags also needs permission to read them
	// (s3:GetObjectTagging / s3:GetObjectVersionTagging), as on AWS.
	// Otherwise a principal limited to s3:GetObject and s3:PutObject could
	// read tags it may not read and plant tags that
	// s3:ExistingObjectTag conditions rely on.
	opts.tagsSet = len(meta.Tags) > 0
	if tagDir == directiveCopy && len(meta.Tags) > 0 {
		action := "s3:GetObjectTagging"
		if src.VersionID != "" {
			action = "s3:GetObjectVersionTagging"
		}
		if err := s.authorize(r, rc, action, src.Bucket, src.Key, src.VersionID); err != nil {
			return err
		}
	}
	if err := s.prepareNewObject(ctx, r, rc, bi, rc.key, &meta, opts); err != nil {
		return err
	}
	if algo == "" {
		algo = sourceChecksumAlgorithm(info.Checksums)
	}
	if algo == "" {
		algo = ChecksumCRC64NVME
	}
	// Like AWS, a copy that takes a while answers 200 early and keeps the
	// connection alive with whitespace (an error then comes in the 200
	// body), so clients do not time out and retry large copies. The early
	// headers are known now; the version ID and expiration of the new
	// object follow as trailers in that case.
	early := http.Header{}
	if srcBucket.Versioning != storage.VersioningUnversioned {
		early.Set("X-Amz-Copy-Source-Version-Id", info.VersionID)
	}
	setSSEHeaders(early, meta)
	versioned := bi.Versioning != storage.VersioningUnversioned
	var trailers []string
	if versioned {
		trailers = []string{"X-Amz-Version-Id"}
	}
	if s.bucketLifecycle(ctx, rc.bucket) != nil {
		trailers = append(trailers, "X-Amz-Expiration")
	}
	bucket, key := rc.bucket, rc.key
	return s.respondWithKeepAlive(w, r, rc, early, trailers, func() longResult {
		hasher := &copyHasher{md5: md5.New(), sum: newChecksumHash(algo), algo: algo}
		// ctxReader stops the copy when the client goes away.
		data := io.TeeReader(io.LimitReader(ctxReader{ctx, rd}, info.Size), hasher)
		out, err := s.storeObject(ctx, bucket, key, data, meta, cond, func(m *storage.ObjectMeta) error {
			if hasher.n != info.Size {
				return io.ErrUnexpectedEOF
			}
			m.ETag = hex.EncodeToString(hasher.md5.Sum(nil))
			m.Checksums = map[string]string{algo: base64.StdEncoding.EncodeToString(hasher.sum.Sum(nil))}
			m.ChecksumType = ChecksumTypeFullObject
			return nil
		})
		if err != nil {
			return longResult{err: err}
		}
		hdr := http.Header{}
		if versioned {
			hdr.Set("X-Amz-Version-Id", out.VersionID)
		}
		setSSEHeaders(hdr, out.ObjectMeta)
		s.setExpirationHeader(ctx, hdr, out)
		res := copyObjectResult{
			ETag:         quoteETag(out.ETag),
			LastModified: formatXMLTime(out.LastModified),
			ChecksumType: out.ChecksumType,
		}
		res.ChecksumCRC32 = out.Checksums[ChecksumCRC32]
		res.ChecksumCRC32C = out.Checksums[ChecksumCRC32C]
		res.ChecksumCRC64NVME = out.Checksums[ChecksumCRC64NVME]
		res.ChecksumSHA1 = out.Checksums[ChecksumSHA1]
		res.ChecksumSHA256 = out.Checksums[ChecksumSHA256]
		return longResult{doc: res, headers: hdr}
	})
}

// openCopySource opens the source of a copy (CopyObject, and usable by
// UploadPartCopy): it validates the source version ID, authorizes
// s3:GetObject / s3:GetObjectVersion on it, applies the S3 delete marker
// rules (NoSuchKey for a latest delete marker, InvalidRequest for one
// addressed by version ID) and the x-amz-copy-source-if-* preconditions
// (412). It returns the source bucket, the version's info and a reader
// over its content, which the caller must close.
func (s *Server) openCopySource(r *http.Request, rc *reqContext, src copySource) (storage.BucketInfo, storage.ObjectInfo, io.ReadSeekCloser, error) {
	ctx := r.Context()
	if src.VersionID != "" && !validVersionID(src.VersionID) {
		return storage.BucketInfo{}, storage.ObjectInfo{}, nil, s3err.ErrInvalidArgument.WithMessage("Invalid version id specified").
			With("ArgumentName", "x-amz-copy-source").With("ArgumentValue", truncateValue(r.Header.Get("X-Amz-Copy-Source")))
	}
	if err := s.authorize(r, rc, objectReadAction(src.VersionID), src.Bucket, src.Key, src.VersionID); err != nil {
		return storage.BucketInfo{}, storage.ObjectInfo{}, nil, err
	}
	bi, err := s.lookupBucket(ctx, src.Bucket)
	if err != nil {
		return storage.BucketInfo{}, storage.ObjectInfo{}, nil, err
	}
	info, rd, err := s.backend.OpenObject(ctx, src.Bucket, src.Key, src.VersionID)
	if err != nil {
		return bi, info, nil, objectLookupError(err, src.Bucket, src.Key, src.VersionID)
	}
	fail := func(err error) (storage.BucketInfo, storage.ObjectInfo, io.ReadSeekCloser, error) {
		if rd != nil {
			_ = rd.Close()
		}
		return bi, info, nil, err
	}
	switch {
	case info.DeleteMarker && src.VersionID != "":
		return fail(s3err.ErrInvalidRequest.WithMessage("The source of a copy request may not specifically refer to a delete marker by version id."))
	case info.DeleteMarker:
		return fail(s3err.ErrNoSuchKey.With("Key", src.Key))
	case rd == nil:
		return fail(errors.New("s3: backend returned no reader for an object"))
	}
	if err := checkCopySourceConditions(r.Header, info.ETag, info.LastModified); err != nil {
		return fail(err)
	}
	return bi, info, rd, nil
}

// --- DeleteObjects ---

// deleteRequest is the DeleteObjects request body.
type deleteRequest struct {
	XMLName xml.Name           `xml:"Delete"`
	Quiet   bool               `xml:"Quiet"`
	Objects []deleteIdentifier `xml:"Object"`
}

// deleteIdentifier is one <Object> of a DeleteObjects request.
type deleteIdentifier struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
	// ETag makes the deletion conditional on the current ETag.
	ETag string `xml:"ETag"`
}

// deleteResult is the DeleteObjects response body.
type deleteResult struct {
	XMLName xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ DeleteResult"`
	Deleted []deletedEntry `xml:"Deleted"`
	Errors  []deleteError  `xml:"Error"`
}

// deletedEntry reports one successful deletion.
type deletedEntry struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId,omitempty"`
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

// deleteError reports one failed deletion.
type deleteError struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// maxDeleteObjectsBody bounds the DeleteObjects request body. It is the
// largest request a client can legitimately send, 1000 entries of:
//
//	<Object></Object>                          17
//	<Key></Key>                                11
//	key: 1024 bytes, each escaped as at most
//	  6 bytes ("&quot;", "&apos;", "&#x0D;")  6144
//	<VersionId></VersionId>                    23
//	version ID (gos3: < 32, AWS: 32)          128
//	<ETag></ETag>                              13
//	&quot; + 32 hex + "-10000" + &quot;        50
//	white space (pretty-printed documents)     64
//	                                         ----
//	                                         6450
//
// 1000 * 6450 = 6,450,000 bytes, plus 4 KiB for the XML declaration, the
// <Delete xmlns="..."> root and <Quiet>: 6,454,096 bytes (about 6.2 MiB).
// The body is parsed as it arrives (see parseDeleteRequest), so its size
// bounds the bytes read and parsed, not the memory kept.
const maxDeleteObjectsBody = maxDeleteObjects*6450 + 4096

// maxDeleteObjectsElements bounds the XML elements of a DeleteObjects
// body: an <Object> and its three fields are four elements; six per entry
// (the 1001st included, which is rejected) leave room for others.
const maxDeleteObjectsElements = (maxDeleteObjects+1)*6 + 64

// parseDeleteRequest reads a DeleteObjects document from x. It stops with
// MalformedXML at the 1001st <Object>, before reading the rest of the
// body, and otherwise accepts what encoding/xml accepted for deleteRequest
// (unknown elements ignored, the last of repeated fields wins). The body
// is read to its end, so its Content-MD5 / checksum is verified, before it
// returns successfully.
func parseDeleteRequest(x *xmlStream) (deleteRequest, error) {
	var req deleteRequest
	if err := x.root("Delete"); err != nil {
		return req, err
	}
	for {
		el, ok, err := x.next()
		if err != nil {
			return req, err
		}
		if !ok {
			break
		}
		switch el.Name.Local {
		case "Quiet":
			v, err := x.text()
			if err != nil {
				return req, err
			}
			if req.Quiet, err = parseXMLBool(v); err != nil {
				return req, x.fail(err)
			}
		case "Object":
			if len(req.Objects) == maxDeleteObjects {
				return req, s3err.ErrMalformedXML
			}
			var o deleteIdentifier
			if err := x.fields(func(name, v string) {
				switch name {
				case "Key":
					o.Key = v
				case "VersionId":
					o.VersionID = v
				case "ETag":
					o.ETag = v
				}
			}); err != nil {
				return req, err
			}
			req.Objects = append(req.Objects, o)
		default:
			if err := x.skip(); err != nil {
				return req, err
			}
		}
	}
	return req, x.finish()
}

// handleDeleteObjects implements DeleteObjects: up to 1000 keys, each
// authorized (s3:DeleteObject / s3:DeleteObjectVersion) and deleted on its
// own; failures are reported per key in the DeleteResult. Quiet mode
// omits successful deletions.
//
// The body is read before anything is authorized (the keys to authorize
// are in it), possibly from an anonymous caller: it is streamed with
// bounded size and work (parseDeleteRequest) and within the absolute
// pre-authorization deadline the core set (see Server.limitPreAuthBody).
func (s *Server) handleDeleteObjects(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	x, err := s.validatedXMLStream(r, rc, maxDeleteObjectsBody, maxDeleteObjectsElements)
	if err != nil {
		return err
	}
	req, err := parseDeleteRequest(x)
	if err != nil {
		return err
	}
	// The whole body has been read: the per-key authorizations below
	// decide what happens, the pre-authorization deadline is done.
	rc.clearPreAuthLimit()
	if len(req.Objects) == 0 || len(req.Objects) > maxDeleteObjects {
		return s3err.ErrMalformedXML
	}
	for _, o := range req.Objects {
		if o.Key == "" {
			return s3err.ErrMalformedXML
		}
	}
	res := deleteResult{}
	for _, o := range req.Objects {
		d, err := s.deleteObjectsEntry(r, rc, bi, o)
		if err != nil {
			e := s.toS3Error(ctx, rc, err)
			res.Errors = append(res.Errors, deleteError{Key: o.Key, VersionID: o.VersionID, Code: e.Code, Message: e.Message})
			continue
		}
		if !req.Quiet {
			res.Deleted = append(res.Deleted, d)
		}
	}
	rc.addLog(slog.Int("delete_keys", len(req.Objects)))
	return writeXML(w, http.StatusOK, res)
}

// deleteObjectsEntry deletes one key of a DeleteObjects request.
func (s *Server) deleteObjectsEntry(r *http.Request, rc *reqContext, bi storage.BucketInfo, o deleteIdentifier) (deletedEntry, error) {
	if err := validateObjectKey(o.Key); err != nil {
		return deletedEntry{}, err
	}
	if o.VersionID != "" && !validVersionID(o.VersionID) {
		return deletedEntry{}, s3err.ErrInvalidArgument.WithMessage("Invalid version id specified")
	}
	action := "s3:DeleteObject"
	if o.VersionID != "" {
		action = "s3:DeleteObjectVersion"
	}
	if err := s.authorize(r, rc, action, bi.Name, o.Key, o.VersionID); err != nil {
		return deletedEntry{}, err
	}
	res, err := s.deleteObjectVersion(r.Context(), r, rc, bi, o.Key, o.VersionID, strings.TrimSpace(o.ETag))
	if err != nil {
		return deletedEntry{}, err
	}
	d := deletedEntry{Key: o.Key}
	switch {
	case o.VersionID != "":
		d.VersionID = o.VersionID
		if res.DeleteMarker {
			d.DeleteMarker, d.DeleteMarkerVersionID = true, o.VersionID
		}
	case res.DeleteMarker:
		d.DeleteMarker, d.DeleteMarkerVersionID = true, res.VersionID
	}
	return d, nil
}
