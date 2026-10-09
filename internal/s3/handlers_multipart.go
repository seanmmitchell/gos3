package s3

// This file holds the multipart upload operations (CreateMultipartUpload,
// UploadPart, UploadPartCopy, CompleteMultipartUpload,
// AbortMultipartUpload); the listings live in handlers_multipart_list.go.
// It is owned by the s3-multipart work item.
//
// Checksums: CreateMultipartUpload may declare a checksum algorithm
// (x-amz-checksum-algorithm) and type (x-amz-checksum-type: COMPOSITE, the
// default except for CRC64NVME, or FULL_OBJECT for the CRC algorithms).
// The declaration is kept in the upload's ObjectMeta as
// Checksums = {algo: ""} and ChecksumType = type. Every part then gets that
// algorithm computed (and validated when the client sent it). Uploads
// without a declared algorithm get a CRC64NVME computed per part, and the
// completed object a FULL_OBJECT CRC64NVME checksum, as S3 does by
// default for new objects.

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Multipart limits.
const (
	// maxPartSize bounds one part (UploadPart, UploadPartCopy): 5 GiB.
	maxPartSize int64 = 5 << 30
	// maxMultipartObjectSize bounds a completed multipart object: 5 TiB.
	maxMultipartObjectSize int64 = 5 << 40
	// maxCompleteBodySize bounds the CompleteMultipartUpload XML body
	// (10000 parts with ETags and checksums fit comfortably).
	maxCompleteBodySize = 8 << 20
	// anonymousInitiator is the initiator recorded for uploads started by
	// anonymous callers (allowed by a bucket policy).
	anonymousInitiator = "anonymous"
)

// multipartMinPartSize is the minimum size of every part but the last
// (EntityTooSmall below it). It is a variable so tests can lower it.
var multipartMinPartSize = newAtomicInt64(minPartSize)

// initiatorName returns the storage initiator for the caller: "" for root
// (as storage.UploadInfo documents), the IAM user name, or
// anonymousInitiator.
func initiatorName(rc *reqContext) string {
	switch {
	case rc.identity.IsAnonymous:
		return anonymousInitiator
	case rc.identity.IsRoot:
		return ""
	}
	return rc.identity.User
}

// uploadChecksum returns the checksum algorithm and type declared when the
// upload was created ("", "" when none).
func uploadChecksum(m storage.ObjectMeta) (algo, typ string) {
	for _, a := range checksumAlgorithms {
		if _, ok := m.Checksums[a]; ok {
			return a, m.ChecksumType
		}
	}
	return "", ""
}

// uploadIDParam returns the uploadId query parameter; an empty value can
// never name an upload (NoSuchUpload).
func uploadIDParam(rc *reqContext) (string, error) {
	id := rc.query.Get("uploadId")
	if id == "" {
		return "", s3err.ErrNoSuchUpload
	}
	return id, nil
}

// getUpload returns the addressed in-progress upload of the request's
// bucket and key.
func (s *Server) getUpload(ctx context.Context, rc *reqContext) (storage.UploadInfo, error) {
	id, err := uploadIDParam(rc)
	if err != nil {
		return storage.UploadInfo{}, err
	}
	up, err := s.backend.GetMultipartUpload(ctx, rc.bucket, rc.key, id)
	if err != nil {
		return storage.UploadInfo{}, bucketError(err, rc.bucket)
	}
	return up, nil
}

// partAlgorithms returns the checksum algorithms computed for every part
// of an upload: the declared one, or CRC64NVME by default.
func partAlgorithms(algo string) []string {
	if algo == "" {
		return []string{ChecksumCRC64NVME}
	}
	return []string{algo}
}

// checksumMismatch is returned when a part carries a checksum of another
// algorithm than the one the upload was created with.
func checksumMismatch(want, got string) error {
	return s3err.ErrInvalidRequest.WithMessage("Checksum Type mismatch occurred, expected checksum Type: %s, actual checksum Type: %s",
		strings.ToLower(want), strings.ToLower(got))
}

// --- CreateMultipartUpload ---

// initiateMultipartUploadResult is the CreateMultipartUpload response body.
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// parseUploadChecksum validates x-amz-checksum-algorithm and
// x-amz-checksum-type of CreateMultipartUpload and returns the effective
// algorithm and type.
func parseUploadChecksum(h http.Header) (algo, typ string, err error) {
	if v := h.Get("X-Amz-Checksum-Algorithm"); v != "" {
		var ok bool
		if algo, ok = normalizeChecksumAlgorithm(v); !ok {
			return "", "", invalidChecksumValue("x-amz-checksum-algorithm")
		}
	}
	if typ, err = parseChecksumType(h.Get("X-Amz-Checksum-Type")); err != nil {
		return "", "", err
	}
	typ, err = validateChecksumType(algo, typ)
	return algo, typ, err
}

// handleCreateMultipartUpload implements CreateMultipartUpload: the new
// object's metadata (content headers, x-amz-meta-*, tagging, canned ACL,
// storage class, encryption, object lock) is validated and stored with
// the upload together with the declared checksum algorithm and type.
func (s *Server) handleCreateMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
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
	algo, typ, err := parseUploadChecksum(h)
	if err != nil {
		return err
	}
	acl := h.Get("X-Amz-Acl")
	if err := s.prepareNewObject(ctx, r, rc, bi, rc.key, &meta,
		h.Get("X-Amz-Object-Lock-Legal-Hold") != "", acl, acl != ""); err != nil {
		return err
	}
	meta.Checksums, meta.ChecksumType = nil, ""
	if algo != "" {
		meta.Checksums = map[string]string{algo: ""}
		meta.ChecksumType = typ
	}
	up, err := s.backend.CreateMultipartUpload(ctx, rc.bucket, rc.key, meta, initiatorName(rc))
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	hdr := w.Header()
	setSSEHeaders(hdr, meta)
	if algo != "" {
		hdr.Set("X-Amz-Checksum-Algorithm", algo)
		hdr.Set("X-Amz-Checksum-Type", typ)
	}
	return writeXML(w, http.StatusOK, initiateMultipartUploadResult{Bucket: rc.bucket, Key: rc.key, UploadID: up.UploadID})
}

// --- UploadPart ---

// handleUploadPart implements UploadPart: the part is streamed into the
// backend while its MD5 (the part ETag) and checksums are computed and
// validated (Content-MD5, x-amz-checksum-* headers and trailers). Parts
// are at most 5 GiB; uploading an existing part number replaces it.
func (s *Server) handleUploadPart(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	partNumber, err := parsePartNumber(rc.query.Get("partNumber"))
	if err != nil {
		return err
	}
	if err := checkSSECHeaders(r.Header); err != nil {
		return err
	}
	up, err := s.getUpload(ctx, rc)
	if err != nil {
		return err
	}
	algo, _ := uploadChecksum(up.Meta)
	p, err := s.newPayloadReader(r, rc, payloadOptions{RequireLength: true, MaxSize: maxPartSize, Algorithms: partAlgorithms(algo)})
	if err != nil {
		return err
	}
	if algo != "" && p.Algorithm() != "" && p.Algorithm() != algo {
		return checksumMismatch(algo, p.Algorithm())
	}
	pw, err := s.backend.NewPartWriter(ctx, rc.bucket, rc.key, up.UploadID, partNumber)
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	committed := false
	defer func() {
		if !committed {
			_ = pw.Abort()
		}
	}()
	if _, err := copyBuffered(writerOnly{pw}, readerOnly{p}); err != nil {
		return mapStorageError(err)
	}
	part, err := pw.Commit(ctx, storage.PartMeta{ETag: p.ETag(), Checksums: p.Checksums()})
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	committed = true

	hdr := w.Header()
	hdr.Set("ETag", quoteETag(part.ETag))
	report := algo
	if report == "" {
		report = p.Algorithm()
	}
	if v := part.Checksums[report]; report != "" && v != "" {
		hdr.Set(checksumHeader(report), v)
	}
	setSSEHeaders(hdr, up.Meta)
	writeEmpty(w, http.StatusOK)
	return nil
}

// --- UploadPartCopy ---

// mpChecksums holds the Checksum<algo> elements of multipart response
// documents (embedded, so the elements appear inline).
type mpChecksums struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
}

// set sets the element of algo (unknown algorithms are ignored).
func (c *mpChecksums) set(algo, v string) {
	switch algo {
	case ChecksumCRC32:
		c.ChecksumCRC32 = v
	case ChecksumCRC32C:
		c.ChecksumCRC32C = v
	case ChecksumCRC64NVME:
		c.ChecksumCRC64NVME = v
	case ChecksumSHA1:
		c.ChecksumSHA1 = v
	case ChecksumSHA256:
		c.ChecksumSHA256 = v
	}
}

// setAll sets the elements of every checksum in sums, or only that of
// algo when algo is not "".
func (c *mpChecksums) setAll(sums map[string]string, algo string) {
	for a, v := range sums {
		if algo == "" || a == algo {
			c.set(a, v)
		}
	}
}

// empty reports whether no checksum is set.
func (c *mpChecksums) empty() bool { return *c == mpChecksums{} }

// copyPartResult is the UploadPartCopy response body.
type copyPartResult struct {
	XMLName      xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyPartResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
	mpChecksums
}

// ctxReader stops reading once ctx is done (client gone).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// handleUploadPartCopy implements UploadPartCopy: a byte range
// (x-amz-copy-source-range, default the whole object) of the copy source,
// authorized with s3:GetObject / s3:GetObjectVersion and subject to the
// x-amz-copy-source-if-* conditions, becomes a part of the upload. The
// copy runs with keep-alive (see respondWithKeepAlive).
func (s *Server) handleUploadPartCopy(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	h := r.Header
	partNumber, err := parsePartNumber(rc.query.Get("partNumber"))
	if err != nil {
		return err
	}
	if err := checkSSECHeaders(h); err != nil {
		return err
	}
	src, err := parseCopySource(h.Get("X-Amz-Copy-Source"))
	if err != nil {
		return err
	}
	if src.VersionID != "" && !validVersionID(src.VersionID) {
		return s3err.ErrInvalidArgument.WithMessage("Invalid version id specified").
			With("ArgumentName", "x-amz-copy-source").With("ArgumentValue", truncateValue(h.Get("X-Amz-Copy-Source")))
	}
	up, err := s.getUpload(ctx, rc)
	if err != nil {
		return err
	}
	if err := s.authorize(r, rc, objectReadAction(src.VersionID), src.Bucket, src.Key, src.VersionID); err != nil {
		return err
	}
	srcBucket, err := s.lookupBucket(ctx, src.Bucket)
	if err != nil {
		return err
	}
	info, rd, err := s.backend.OpenObject(ctx, src.Bucket, src.Key, src.VersionID)
	if err != nil {
		return objectLookupError(err, src.Bucket, src.Key, src.VersionID)
	}
	if rd != nil {
		defer rd.Close()
	}
	if info.DeleteMarker {
		if src.VersionID != "" {
			return s3err.ErrInvalidRequest.WithMessage("The source of a copy request may not specifically refer to a delete marker by version id.")
		}
		return s3err.ErrNoSuchKey.With("Key", src.Key)
	}
	if rd == nil {
		return errors.New("s3: backend returned no reader for an object")
	}
	if err := checkCopySourceConditions(h, info.ETag, info.LastModified); err != nil {
		return err
	}
	rng := byteRange{Start: 0, Length: info.Size}
	if v := h.Get("X-Amz-Copy-Source-Range"); v != "" {
		if rng, err = parseCopySourceRange(v, info.Size); err != nil {
			return err
		}
	}
	if rng.Length > maxPartSize {
		return s3err.ErrInvalidRequest.WithMessage("The specified copy source is larger than the maximum allowable size for a copy source: %d", maxPartSize)
	}
	if rng.Start > 0 {
		if _, err := rd.Seek(rng.Start, io.SeekStart); err != nil {
			return err
		}
	}

	algo, _ := uploadChecksum(up.Meta)
	sumAlgo := partAlgorithms(algo)[0]
	early := http.Header{}
	if srcBucket.Versioning != storage.VersioningUnversioned {
		early.Set("X-Amz-Copy-Source-Version-Id", info.VersionID)
	}
	setSSEHeaders(early, up.Meta)
	bucket, key, uploadID := rc.bucket, rc.key, up.UploadID
	return s.respondWithKeepAlive(w, r, rc, early, nil, func() longResult {
		pw, err := s.backend.NewPartWriter(ctx, bucket, key, uploadID, partNumber)
		if err != nil {
			return longResult{err: bucketError(err, bucket)}
		}
		hasher := &copyHasher{md5: md5.New(), sum: newChecksumHash(sumAlgo), algo: sumAlgo}
		data := io.TeeReader(io.LimitReader(ctxReader{ctx, rd}, rng.Length), hasher)
		n, err := copyBuffered(writerOnly{pw}, readerOnly{data})
		if err == nil && n != rng.Length {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			_ = pw.Abort()
			return longResult{err: mapStorageError(err)}
		}
		sums := map[string]string{sumAlgo: base64.StdEncoding.EncodeToString(hasher.sum.Sum(nil))}
		part, err := pw.Commit(ctx, storage.PartMeta{ETag: hex.EncodeToString(hasher.md5.Sum(nil)), Checksums: sums})
		if err != nil {
			_ = pw.Abort()
			return longResult{err: bucketError(err, bucket)}
		}
		res := copyPartResult{ETag: quoteETag(part.ETag), LastModified: formatXMLTime(part.LastModified)}
		if algo != "" {
			res.set(algo, part.Checksums[algo])
		}
		return longResult{doc: res}
	})
}

// --- CompleteMultipartUpload ---

// completeMultipartUpload is the CompleteMultipartUpload request body.
type completeMultipartUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

// completePart is one <Part> of a CompleteMultipartUpload request.
type completePart struct {
	PartNumber        string `xml:"PartNumber"`
	ETag              string `xml:"ETag"`
	ChecksumCRC32     string `xml:"ChecksumCRC32"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME"`
	ChecksumSHA1      string `xml:"ChecksumSHA1"`
	ChecksumSHA256    string `xml:"ChecksumSHA256"`
}

// checksums returns the part checksums given in the request by algorithm.
func (p completePart) checksums() map[string]string {
	var m map[string]string
	for algo, v := range map[string]string{
		ChecksumCRC32: p.ChecksumCRC32, ChecksumCRC32C: p.ChecksumCRC32C, ChecksumCRC64NVME: p.ChecksumCRC64NVME,
		ChecksumSHA1: p.ChecksumSHA1, ChecksumSHA256: p.ChecksumSHA256,
	} {
		if v = strings.TrimSpace(v); v != "" {
			if m == nil {
				m = map[string]string{}
			}
			m[algo] = v
		}
	}
	return m
}

// completeMultipartUploadResult is the CompleteMultipartUpload response
// body.
type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
	mpChecksums
	ChecksumType string `xml:"ChecksumType,omitempty"`
}

// objectLocation returns the URL of the request's object, as reported in
// <Location>: virtual-hosted or path-style like the request.
func objectLocation(r *http.Request, rc *reqContext) string {
	scheme := "http"
	if rc.secure {
		scheme = "https"
	}
	p := "/" + rc.bucket + "/" + rc.key
	if rc.virtualHost {
		p = "/" + rc.key
	}
	return (&url.URL{Scheme: scheme, Host: r.Host, Path: p}).String()
}

// completionPlan is the validated result of a CompleteMultipartUpload
// request: the parts to concatenate and the final object metadata.
type completionPlan struct {
	partNumbers []int
	meta        storage.ObjectMeta
	size        int64
}

// parseCompleteParts validates the order and range of the requested part
// numbers.
func parseCompleteParts(req *completeMultipartUpload, uploadID string) ([]int, error) {
	if len(req.Parts) == 0 {
		return nil, s3err.ErrMalformedXML
	}
	nums := make([]int, len(req.Parts))
	for i, p := range req.Parts {
		n, err := strconv.Atoi(strings.TrimSpace(p.PartNumber))
		if err != nil {
			return nil, s3err.ErrMalformedXML
		}
		if n < minPartNumber || n > maxPartNumber {
			return nil, s3err.ErrInvalidArgument.WithMessage("Part number must be an integer between 1 and 10000, inclusive").
				With("ArgumentName", "partNumber").With("ArgumentValue", strconv.Itoa(n))
		}
		if i > 0 && n <= nums[i-1] {
			return nil, s3err.ErrInvalidPartOrder.With("UploadId", uploadID)
		}
		nums[i] = n
	}
	return nums, nil
}

// planCompletion checks the requested parts against the stored ones and
// computes the final ETag, size and checksum (see the file comment).
func planCompletion(up storage.UploadInfo, req *completeMultipartUpload, nums []int, stored []storage.PartInfo) (completionPlan, error) {
	byNum := make(map[int]*storage.PartInfo, len(stored))
	for i := range stored {
		byNum[stored[i].PartNumber] = &stored[i]
	}
	algo, typ := uploadChecksum(up.Meta)
	minSize := multipartMinPartSize.Load()
	plan := completionPlan{partNumbers: nums}
	etags := make([]string, len(nums))
	parts := make([]*storage.PartInfo, len(nums))
	for i, n := range nums {
		rp := req.Parts[i]
		sp := byNum[n]
		given := trimETag(rp.ETag)
		if sp == nil || given == "" || !strings.EqualFold(given, sp.ETag) {
			return plan, s3err.ErrInvalidPart.With("UploadId", up.UploadID).
				With("PartNumber", strconv.Itoa(n)).With("ETag", truncateValue(given))
		}
		if i < len(nums)-1 && sp.Size < minSize {
			return plan, s3err.ErrEntityTooSmall.With("ProposedSize", strconv.FormatInt(sp.Size, 10)).
				With("MinSizeAllowed", strconv.FormatInt(minSize, 10)).
				With("PartNumber", strconv.Itoa(n)).With("ETag", sp.ETag)
		}
		givenSums := rp.checksums()
		for a, v := range givenSums {
			if algo != "" && a != algo {
				return plan, checksumMismatch(algo, a)
			}
			if sp.Checksums[a] == "" || sp.Checksums[a] != v {
				return plan, s3err.ErrInvalidPart.WithMessage("The %s checksum of part %d does not match the checksum of the uploaded part.", strings.ToLower(a), n).
					With("UploadId", up.UploadID).With("PartNumber", strconv.Itoa(n))
			}
		}
		if typ == ChecksumTypeComposite && givenSums[algo] == "" {
			return plan, s3err.ErrInvalidRequest.WithMessage("The upload was created using a %s checksum. The complete request must include the checksum for each part. It was missing for part %d in the request.",
				strings.ToLower(algo), n)
		}
		plan.size += sp.Size
		if plan.size > maxMultipartObjectSize {
			return plan, s3err.ErrEntityTooLarge.With("MaxSizeAllowed", strconv.FormatInt(maxMultipartObjectSize, 10))
		}
		etags[i] = sp.ETag
		parts[i] = sp
	}
	etag, err := multipartETag(etags)
	if err != nil {
		return plan, err
	}

	meta := up.Meta
	meta.ETag = etag
	meta.Checksums, meta.ChecksumType, meta.PartChecksums = nil, "", nil
	meta.PartSizes = make([]int64, len(parts))
	for i, p := range parts {
		meta.PartSizes[i] = p.Size
	}

	final, finalType := algo, typ
	if final == "" {
		// Default checksum: FULL_OBJECT CRC64NVME when every part has one.
		final, finalType = ChecksumCRC64NVME, ChecksumTypeFullObject
		for _, p := range parts {
			if p.Checksums[final] == "" {
				final, finalType = "", ""
				break
			}
		}
	}
	if final != "" {
		var sum string
		switch finalType {
		case ChecksumTypeComposite:
			sums := make([]string, len(parts))
			for i, p := range parts {
				sums[i] = p.Checksums[final]
			}
			sum, err = compositeChecksum(final, sums)
		default:
			cps := make([]checksumPart, len(parts))
			for i, p := range parts {
				cps[i] = checksumPart{Checksum: p.Checksums[final], Size: p.Size}
			}
			sum, err = combineFullObjectChecksum(final, cps)
		}
		if err != nil {
			return plan, err
		}
		meta.Checksums = map[string]string{final: sum}
		meta.ChecksumType = finalType
		meta.PartChecksums = make([]map[string]string, len(parts))
		for i, p := range parts {
			meta.PartChecksums[i] = map[string]string{final: p.Checksums[final]}
		}
	}
	plan.meta = meta
	return plan, nil
}

// checkCompleteHeaders validates the optional x-amz-checksum-type,
// x-amz-checksum-<algo> (the expected final checksum) and
// x-amz-mp-object-size headers of CompleteMultipartUpload against the
// plan and the upload's declaration.
func checkCompleteHeaders(h http.Header, up storage.UploadInfo, plan completionPlan) error {
	declAlgo, declType := uploadChecksum(up.Meta)
	if v := h.Get("X-Amz-Checksum-Type"); v != "" {
		typ, err := parseChecksumType(v)
		if err != nil {
			return err
		}
		if typ != declType {
			mode := strings.ToLower(declType)
			if mode == "" {
				mode = "no"
			}
			return s3err.ErrInvalidRequest.WithMessage("The upload was created using the %s checksum mode. The complete request must use the same checksum mode.", mode)
		}
	}
	finalAlgo := ""
	for a := range plan.meta.Checksums {
		finalAlgo = a
	}
	for _, a := range checksumAlgorithms {
		v := strings.TrimSpace(h.Get(checksumHeader(a)))
		if v == "" {
			continue
		}
		if a != finalAlgo || (declAlgo == "" && a != ChecksumCRC64NVME) {
			if declAlgo == "" {
				return s3err.ErrInvalidRequest.WithMessage("The upload was created without a checksum algorithm; the %s header is not allowed.", checksumHeader(a))
			}
			return checksumMismatch(declAlgo, a)
		}
		want, _, _ := strings.Cut(v, "-")
		if _, ok := decodeChecksum(a, want); !ok {
			return invalidChecksumValue(checksumHeader(a))
		}
		got, _, _ := strings.Cut(plan.meta.Checksums[a], "-")
		if want != got {
			return s3err.ErrBadDigest.WithMessage("The %s you specified did not match the calculated checksum.", strings.ToLower(a))
		}
	}
	if v := h.Get("X-Amz-Mp-Object-Size"); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			return s3err.ErrInvalidArgument.WithMessage("Value for x-amz-mp-object-size header is invalid.").
				With("ArgumentName", "x-amz-mp-object-size").With("ArgumentValue", truncateValue(v))
		}
		if n != plan.size {
			return s3err.ErrInvalidRequest.WithMessage("The provided 'x-amz-mp-object-size' header value %d does not match what was computed: %d", n, plan.size)
		}
	}
	return nil
}

// checkCompleteConditions evaluates If-None-Match / If-Match against the
// current object before the (atomic, re-checked) commit so that obvious
// failures get a proper status code instead of an error embedded in a 200.
func (s *Server) checkCompleteConditions(ctx context.Context, bucket, key string, cond storage.CommitOptions) error {
	if !cond.IfNoneMatch && cond.IfMatch == "" {
		return nil
	}
	info, err := s.backend.GetObjectInfo(ctx, bucket, key, "")
	if err != nil && !errors.Is(err, storage.ErrObjectNotFound) {
		return bucketError(err, bucket)
	}
	exists := err == nil && !info.DeleteMarker
	if cond.IfNoneMatch && exists {
		return s3err.ErrPreconditionFailed.With("Condition", "If-None-Match")
	}
	if cond.IfMatch != "" {
		if !exists {
			return s3err.ErrNoSuchKey.With("Key", key)
		}
		if trimETag(info.ETag) != cond.IfMatch {
			return s3err.ErrPreconditionFailed.With("Condition", "If-Match")
		}
	}
	return nil
}

// handleCompleteMultipartUpload implements CompleteMultipartUpload: the
// listed parts (strictly ascending, existing, matching ETags, all but the
// last at least 5 MiB, optional per-part checksums matching) are
// concatenated into a new object version. The ETag is the multipart ETag
// and the checksum the COMPOSITE or FULL_OBJECT combination of the part
// checksums. Conditional completion (If-None-Match: *, If-Match) is
// atomic. The concatenation runs with keep-alive (respondWithKeepAlive).
func (s *Server) handleCompleteMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	ctx := r.Context()
	h := r.Header
	bi, err := s.bucketInfo(ctx, rc)
	if err != nil {
		return err
	}
	up, err := s.getUpload(ctx, rc)
	if err != nil {
		return err
	}
	var req completeMultipartUpload
	if err := decodeXMLBody(r.Body, maxCompleteBodySize, &req); err != nil {
		if errors.Is(err, s3err.ErrMissingRequestBody) {
			return s3err.ErrMalformedXML
		}
		return err
	}
	nums, err := parseCompleteParts(&req, up.UploadID)
	if err != nil {
		return err
	}
	cond, err := parseWriteConditions(h)
	if err != nil {
		return err
	}
	stored, err := s.backend.ListParts(ctx, rc.bucket, rc.key, up.UploadID)
	if err != nil {
		return bucketError(err, rc.bucket)
	}
	plan, err := planCompletion(up, &req, nums, stored)
	if err != nil {
		return err
	}
	if err := checkCompleteHeaders(h, up, plan); err != nil {
		return err
	}
	if err := s.checkCompleteConditions(ctx, rc.bucket, rc.key, cond); err != nil {
		return err
	}

	versioned := bi.Versioning != storage.VersioningUnversioned
	early := http.Header{}
	setSSEHeaders(early, plan.meta)
	var trailers []string
	if versioned {
		trailers = []string{"X-Amz-Version-Id"}
	}
	location := objectLocation(r, rc)
	bucket, key := rc.bucket, rc.key
	// Completion continues when the client goes away, like in S3: the
	// backend commit is atomic and the upload is gone afterwards.
	workCtx := context.WithoutCancel(ctx)
	return s.respondWithKeepAlive(w, r, rc, early, trailers, func() longResult {
		info, err := s.backend.CompleteMultipartUpload(workCtx, bucket, key, up.UploadID, plan.partNumbers, plan.meta, cond)
		if err != nil {
			if errors.Is(err, storage.ErrPreconditionFailed) {
				return longResult{err: s.commitError(workCtx, err, bucket, key, cond)}
			}
			return longResult{err: bucketError(err, bucket)}
		}
		res := completeMultipartUploadResult{
			Location:     location,
			Bucket:       bucket,
			Key:          key,
			ETag:         quoteETag(info.ETag),
			ChecksumType: info.ChecksumType,
		}
		res.setAll(info.Checksums, "")
		if res.empty() {
			res.ChecksumType = ""
		}
		hdr := http.Header{}
		if versioned {
			hdr.Set("X-Amz-Version-Id", info.VersionID)
		}
		return longResult{doc: res, headers: hdr}
	})
}

// --- AbortMultipartUpload ---

// handleAbortMultipartUpload implements AbortMultipartUpload (204; the
// upload and all its parts are removed).
func (s *Server) handleAbortMultipartUpload(w http.ResponseWriter, r *http.Request, rc *reqContext) error {
	id, err := uploadIDParam(rc)
	if err != nil {
		return err
	}
	if err := s.backend.AbortMultipartUpload(r.Context(), rc.bucket, rc.key, id); err != nil {
		return bucketError(err, rc.bucket)
	}
	writeNoContent(w)
	return nil
}
