package s3client

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ObjectAttrs describes a remote object (from HEAD, GET or a listing).
type ObjectAttrs struct {
	Key          string
	Size         int64
	ETag         string // unquoted
	LastModified time.Time
	StorageClass string
	// Date is the server's clock at the time of the answer (its Date
	// response header) for HEAD and GET; zero for listings or when the
	// header is missing. It lets callers judge object ages by the
	// service's clock rather than the local one.
	Date time.Time
	// Header holds the response headers of HEAD/GET (nil for listings):
	// Content-Type, x-amz-meta-*, x-amz-tagging-count, ...
	Header http.Header
	// VersionID is the object's version ID (x-amz-version-id, or the
	// listing's VersionId; "" when the service reported none).
	VersionID string
	// DeleteMarker marks a delete marker (ListObjectVersions entries).
	DeleteMarker bool
	// IsLatest is set by ListObjectVersions for the current version.
	IsLatest bool
}

// attrsFromHeader fills attributes from a HEAD/GET response.
func attrsFromHeader(key string, resp *http.Response) ObjectAttrs {
	a := ObjectAttrs{
		Key:          key,
		ETag:         unquoteETag(resp.Header.Get("ETag")),
		StorageClass: resp.Header.Get("X-Amz-Storage-Class"),
		Header:       resp.Header,
		Size:         resp.ContentLength,
		VersionID:    resp.Header.Get("X-Amz-Version-Id"),
		DeleteMarker: deleteMarkerHeader(resp.Header),
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		a.LastModified = t.UTC()
	}
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		a.Date = t.UTC()
	}
	// A ranged GET reports the full size in Content-Range.
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				a.Size = n
			}
		}
	}
	return a
}

// --- objects ---

// PutOptions are the optional parts of PutObject (and of Upload).
type PutOptions struct {
	// Header holds object headers to send: Content-Type, Content-Encoding,
	// Cache-Control, x-amz-meta-*, x-amz-tagging, x-amz-storage-class, ...
	Header http.Header
	// IfNoneMatch sends "If-None-Match: *": fail if the key exists.
	IfNoneMatch bool
	// IfMatch sends "If-Match: <etag>": fail unless the current object has
	// this (unquoted) ETag.
	IfMatch string
	// NoRetry sends the request once: a 5xx answer or a transport error is
	// returned as is instead of being retried. Callers resolving ambiguous
	// conditional writes themselves (see the package documentation) use it
	// to see the first failure rather than a 412 caused by their own
	// earlier attempt. For a multipart Upload it applies to the completing
	// request (parts are always retried).
	NoRetry bool
	// ContentMD5 is the raw (16-byte) MD5 of the body, sent as Content-MD5
	// (nil = not sent). Services require it for some writes (AWS: objects
	// with object lock headers).
	ContentMD5 []byte
}

func (o PutOptions) header() http.Header {
	h := http.Header{}
	for k, vs := range o.Header {
		h[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	if o.IfNoneMatch {
		h.Set("If-None-Match", "*")
	}
	if o.IfMatch != "" {
		h.Set("If-Match", quoteETag(o.IfMatch))
	}
	if o.ContentMD5 != nil {
		h.Set("Content-Md5", base64.StdEncoding.EncodeToString(o.ContentMD5))
	}
	return h
}

// WriteResult is the outcome of a write.
type WriteResult struct {
	ETag      string // unquoted
	VersionID string // x-amz-version-id ("" when the service sent none)
	Multipart bool   // Upload: written with a multipart upload
}

// PutObject uploads r[0:size] as bucket/key and returns the new ETag.
func (c *Client) PutObject(ctx context.Context, bucket, key string, r io.ReaderAt, size int64, opts PutOptions) (string, error) {
	res, err := c.Put(ctx, bucket, key, r, size, opts)
	return res.ETag, err
}

// Put is PutObject returning the version ID too. Services send one for
// buckets whose versioning is enabled or suspended ("null"); for
// unversioned buckets gos3 and AWS send none ("").
func (c *Client) Put(ctx context.Context, bucket, key string, r io.ReaderAt, size int64, opts PutOptions) (WriteResult, error) {
	resp, err := c.do(ctx, request{
		op: "PutObject", method: http.MethodPut, bucket: bucket, key: key, hasKey: true,
		header: opts.header(), body: readerAtBody(r, size), bodySize: size,
		// Without preconditions a repeated PUT is harmless.
		retryTransport: !opts.IfNoneMatch && opts.IfMatch == "",
		noRetry:        opts.NoRetry,
	})
	if err != nil {
		return WriteResult{}, err
	}
	drain(resp)
	return WriteResult{ETag: unquoteETag(resp.Header.Get("ETag")), VersionID: versionIDOf(resp)}, nil
}

// HeadObject returns the attributes of bucket/key.
func (c *Client) HeadObject(ctx context.Context, bucket, key string) (ObjectAttrs, error) {
	return c.HeadObjectWith(ctx, bucket, key, HeadOptions{})
}

// HeadOptions are the optional parts of HeadObjectWith.
type HeadOptions struct {
	VersionID string // "" = the current version
	IfMatch   string // fail (412) unless the object has this ETag
}

// HeadObjectWith is HeadObject for a version. A delete marker answers an
// error for which IsDeleteMarker reports true (405 when addressed by
// version ID, else 404; the answer has no body, so the codes are
// MethodNotAllowed and NotFound).
func (c *Client) HeadObjectWith(ctx context.Context, bucket, key string, opts HeadOptions) (ObjectAttrs, error) {
	var h http.Header
	if opts.IfMatch != "" {
		h = http.Header{"If-Match": {quoteETag(opts.IfMatch)}}
	}
	resp, err := c.do(ctx, request{
		op: "HeadObject", method: http.MethodHead, bucket: bucket, key: key, hasKey: true,
		query: objectQuery("", opts.VersionID), header: h, retryTransport: true,
	})
	if err != nil {
		return ObjectAttrs{}, err
	}
	drain(resp)
	return attrsFromHeader(key, resp), nil
}

// GetOptions are the optional parts of GetObject.
type GetOptions struct {
	// Offset and Length select a byte range (Length < 0: to the end).
	// The zero value reads the whole object.
	Offset, Length int64
	// IfMatch fails the request (412) unless the object has this ETag.
	IfMatch string
	// VersionID reads that version ("" = the current one).
	VersionID string
}

// GetObject opens bucket/key for reading. The caller must close the body.
func (c *Client) GetObject(ctx context.Context, bucket, key string, opts GetOptions) (io.ReadCloser, ObjectAttrs, error) {
	h := http.Header{}
	if opts.Offset > 0 || opts.Length > 0 {
		if opts.Length > 0 {
			h.Set("Range", fmt.Sprintf("bytes=%d-%d", opts.Offset, opts.Offset+opts.Length-1))
		} else {
			h.Set("Range", fmt.Sprintf("bytes=%d-", opts.Offset))
		}
	}
	if opts.IfMatch != "" {
		h.Set("If-Match", quoteETag(opts.IfMatch))
	}
	resp, err := c.do(ctx, request{
		op: "GetObject", method: http.MethodGet, bucket: bucket, key: key, hasKey: true,
		query: objectQuery("", opts.VersionID), header: h, retryTransport: true,
	})
	if err != nil {
		return nil, ObjectAttrs{}, err
	}
	return resp.Body, attrsFromHeader(key, resp), nil
}

// deleteMarkerHeader reports x-amz-delete-marker: true.
func deleteMarkerHeader(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get("X-Amz-Delete-Marker")), "true")
}

// DeleteObject deletes bucket/key. Deleting a missing key is not an error.
func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := c.DeleteObjectWith(ctx, bucket, key, DeleteOptions{})
	return err
}

// DeleteOptions are the optional parts of DeleteObjectWith.
type DeleteOptions struct {
	VersionID string
	// BypassGovernance sends x-amz-bypass-governance-retention: true.
	BypassGovernance bool
}

// DeleteResult is the outcome of a delete (from x-amz-version-id and
// x-amz-delete-marker): in a versioned bucket, deleting without a version
// ID reports the delete marker it created; deleting a version reports that
// version and whether it was a delete marker.
type DeleteResult struct {
	VersionID    string
	DeleteMarker bool
}

// DeleteObjectWith deletes bucket/key, or one version of it. Without a
// version ID a missing key is not an error (as DeleteObject); with one,
// the service's answer is returned as is (AWS answers 204 for a version
// that does not exist, other services 404 or 400).
func (c *Client) DeleteObjectWith(ctx context.Context, bucket, key string, opts DeleteOptions) (DeleteResult, error) {
	var h http.Header
	if opts.BypassGovernance {
		h = http.Header{"X-Amz-Bypass-Governance-Retention": {"true"}}
	}
	resp, err := c.do(ctx, request{
		op: "DeleteObject", method: http.MethodDelete, bucket: bucket, key: key, hasKey: true,
		query: objectQuery("", opts.VersionID), header: h, retryTransport: true,
	})
	if opts.VersionID == "" && IsNotFound(err) && !IsCode(err, "NoSuchBucket") {
		return DeleteResult{}, nil
	}
	if err != nil {
		return DeleteResult{}, err
	}
	drain(resp)
	return DeleteResult{VersionID: resp.Header.Get("X-Amz-Version-Id"), DeleteMarker: deleteMarkerHeader(resp.Header)}, nil
}

// MaxDeleteKeys is the most keys one DeleteObjects request may name;
// Client.DeleteObjects splits longer lists.
const MaxDeleteKeys = 1000

// DeleteFailure is a key a DeleteObjects request could not delete.
type DeleteFailure struct {
	Key string
	// VersionID is the version that was not deleted (DeleteVersions; ""
	// for DeleteObjects).
	VersionID string `xml:"VersionId"`
	Code      string // S3 error code, e.g. "AccessDenied"
	Message   string
}

// DeleteObjectsError reports the keys DeleteObjects (or the versions
// DeleteVersions) could not delete (the others were deleted). It unwraps to
// one *Error per key, so IsCode works on it.
type DeleteObjectsError struct {
	Failed []DeleteFailure
}

func (e *DeleteObjectsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "s3client: DeleteObjects: %d key(s) not deleted:", len(e.Failed))
	for i, f := range e.Failed {
		if i == 3 {
			fmt.Fprintf(&b, " (and %d more)", len(e.Failed)-i)
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, " %q", f.Key)
		if f.VersionID != "" {
			fmt.Fprintf(&b, " (version %q)", f.VersionID)
		}
		fmt.Fprintf(&b, ": %s", f.Code)
		if f.Message != "" {
			b.WriteString(": " + f.Message)
		}
	}
	return b.String()
}

// Unwrap returns the per-key failures as *Error values.
func (e *DeleteObjectsError) Unwrap() []error {
	errs := make([]error, len(e.Failed))
	for i, f := range e.Failed {
		errs[i] = &Error{Op: "DeleteObjects", Code: f.Code, Message: f.Message}
	}
	return errs
}

// deleteFailures collects the failures of batched deletes for the error
// DeleteObjects and DeleteVersions return.
type deleteFailures []DeleteFailure

// result returns err (the error that stopped the deletes, nil if none)
// joined with the collected failures.
func (f deleteFailures) result(err error) error {
	var fe error
	if len(f) > 0 {
		fe = &DeleteObjectsError{Failed: f}
	}
	if err == nil {
		return fe
	}
	return errors.Join(err, fe)
}

// DeleteObjects deletes keys from bucket with batched DeleteObjects
// requests (quiet mode, at most MaxDeleteKeys keys each, sent one after the
// other). Missing keys are not an error. Keys that the service could not
// delete are reported together as a *DeleteObjectsError once every batch
// was sent; a failed request stops at once and returns its error (joined
// with the failures so far). Keys that cannot be written in an XML request
// body (invalid UTF-8 or control characters) are deleted one by one with
// DeleteObject.
func (c *Client) DeleteObjects(ctx context.Context, bucket string, keys []string) error {
	var (
		failed deleteFailures
		batch  = make([]deleteEntry, 0, min(len(keys), MaxDeleteKeys))
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, errs, err := c.deleteBatch(ctx, bucket, batch, true, false)
		for _, f := range errs {
			if !missingCode(f.Code) {
				failed = append(failed, f)
			}
		}
		batch = batch[:0]
		return err
	}
	for _, key := range keys {
		if key == "" {
			return failed.result(errors.New("s3client: DeleteObjects: empty key"))
		}
		if !xmlSafe(key) {
			if err := c.DeleteObject(ctx, bucket, key); err != nil {
				return failed.result(err)
			}
			continue
		}
		batch = append(batch, deleteEntry{Key: key})
		if len(batch) == MaxDeleteKeys {
			if err := flush(); err != nil {
				return failed.result(err)
			}
		}
	}
	return failed.result(flush())
}

// ObjectID names an object version for DeleteVersions.
type ObjectID struct{ Key, VersionID string }

// DeleteVersions deletes the given versions (an empty VersionID deletes as
// DeleteObject does: in a versioned bucket it adds a delete marker) with
// non-quiet DeleteObjects requests of at most MaxDeleteKeys entries each,
// sent one after the other, with x-amz-bypass-governance-retention: true
// when bypassGovernance is set.
//
// The results are aligned with ids: results[i] is the outcome of ids[i]
// (the version deleted, or the delete marker created). Missing versions
// are not an error; their result names the requested version. Entries the
// service could not delete keep the zero DeleteResult and are reported
// together as a *DeleteObjectsError once every batch was sent; a failed
// request stops at once and returns its error (joined with the failures so
// far) along with the results known so far. Entries that cannot be written
// in an XML request body are deleted one by one with DeleteObjectWith.
func (c *Client) DeleteVersions(ctx context.Context, bucket string, ids []ObjectID, bypassGovernance bool) ([]DeleteResult, error) {
	results := make([]DeleteResult, len(ids))
	var (
		failed deleteFailures
		batch  = make([]deleteEntry, 0, min(len(ids), MaxDeleteKeys))
		at     = make([]int, 0, cap(batch)) // at[j] is the index in ids of batch[j]
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		deleted, errs, err := c.deleteBatch(ctx, bucket, batch, false, bypassGovernance)
		if err == nil {
			pos := make(map[ObjectID][]int, len(batch))
			for j, e := range batch {
				id := ObjectID{e.Key, e.VersionID}
				pos[id] = append(pos[id], at[j])
			}
			for _, d := range deleted {
				r := DeleteResult{VersionID: d.VersionID, DeleteMarker: d.DeleteMarker}
				if r.VersionID == "" {
					// Deleting without a version ID created this marker.
					r.VersionID = d.DeleteMarkerVersionID
				}
				for _, i := range pos[ObjectID{d.Key, d.VersionID}] {
					results[i] = r
				}
			}
			for _, f := range errs {
				if !missingCode(f.Code) {
					failed = append(failed, f)
					continue
				}
				for _, i := range pos[ObjectID{f.Key, f.VersionID}] {
					results[i] = DeleteResult{VersionID: f.VersionID}
				}
			}
		}
		batch, at = batch[:0], at[:0]
		return err
	}
	for i, id := range ids {
		if id.Key == "" {
			return results, failed.result(errors.New("s3client: DeleteObjects: empty key"))
		}
		if !xmlSafe(id.Key) || !xmlSafe(id.VersionID) {
			r, err := c.DeleteObjectWith(ctx, bucket, id.Key, DeleteOptions{VersionID: id.VersionID, BypassGovernance: bypassGovernance})
			switch {
			case err == nil:
				results[i] = r
			case IsNotFound(err) && !IsCode(err, "NoSuchBucket"):
				results[i] = DeleteResult{VersionID: id.VersionID}
			default:
				return results, failed.result(err)
			}
			continue
		}
		batch = append(batch, deleteEntry{Key: id.Key, VersionID: id.VersionID})
		at = append(at, i)
		if len(batch) == MaxDeleteKeys {
			if err := flush(); err != nil {
				return results, failed.result(err)
			}
		}
	}
	return results, failed.result(flush())
}

// missingCode reports whether a DeleteObjects entry failed only because
// there was nothing to delete.
func missingCode(code string) bool { return code == "NoSuchKey" || code == "NoSuchVersion" }

// deleteEntry is one <Object> of a DeleteObjects request.
type deleteEntry struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}

// deletedEntry is one <Deleted> element of a DeleteObjects answer.
type deletedEntry struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId"`
	DeleteMarker          bool   `xml:"DeleteMarker"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId"`
}

// deleteBatch sends one DeleteObjects request for objs and returns the
// entries the service reported deleted (none in quiet mode) and the ones it
// reported failed, including the NoSuchKey / NoSuchVersion answers some
// services give for missing entries (see missingCode).
func (c *Client) deleteBatch(ctx context.Context, bucket string, objs []deleteEntry, quiet, bypassGovernance bool) ([]deletedEntry, []DeleteFailure, error) {
	doc := struct {
		XMLName xml.Name      `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Delete"`
		Quiet   bool          `xml:"Quiet"`
		Objects []deleteEntry `xml:"Object"`
	}{Quiet: quiet, Objects: objs}
	// S3 requires an integrity header (Content-MD5) on DeleteObjects.
	req, err := xmlRequest("DeleteObjects", http.MethodPost, bucket, url.Values{"delete": {""}}, doc)
	if err != nil {
		return nil, nil, err
	}
	if bypassGovernance {
		req.header.Set("X-Amz-Bypass-Governance-Retention", "true")
	}
	// Deleting is idempotent.
	req.retryTransport = true
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Deleted []deletedEntry  `xml:"Deleted"`
		Errors  []DeleteFailure `xml:"Error"`
	}
	if err := decodeXML("DeleteObjects", resp, &out); err != nil {
		return nil, nil, err
	}
	return out.Deleted, out.Errors, nil
}

// xmlSafe reports whether s survives an XML 1.0 round trip unchanged (the
// encoder replaces invalid UTF-8 and characters outside the XML Char
// production).
func xmlSafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == 0x9 || r == 0xA || r == 0xD:
		case r >= 0x20 && r <= 0xD7FF:
		case r >= 0xE000 && r <= 0xFFFD:
		case r >= 0x10000 && r <= 0x10FFFF:
		default:
			return false
		}
	}
	return true
}

// CopyOptions are the optional parts of CopyObject.
type CopyOptions struct {
	// ReplaceMetadata replaces the object headers with Header
	// (x-amz-metadata-directive REPLACE, and REPLACE for tags when Header
	// carries x-amz-tagging); otherwise the source's are kept.
	ReplaceMetadata bool
	Header          http.Header
	// IfMatch makes the copy conditional on the source's ETag.
	IfMatch string
	// SourceBucket is the source's bucket ("" = the destination bucket)
	// and SourceVersionID the source version ("" = the current one). They
	// are used by CopyObjectWith only.
	SourceBucket    string
	SourceVersionID string
}

func (o CopyOptions) header(srcBucket, srcKey string) http.Header {
	h := http.Header{}
	if o.ReplaceMetadata {
		for k, vs := range o.Header {
			h[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
		h.Set("X-Amz-Metadata-Directive", "REPLACE")
		if h.Get("X-Amz-Tagging") != "" {
			h.Set("X-Amz-Tagging-Directive", "REPLACE")
		}
	}
	h.Set("X-Amz-Copy-Source", copySource(srcBucket, srcKey, o.SourceVersionID))
	if o.IfMatch != "" {
		h.Set("X-Amz-Copy-Source-If-Match", quoteETag(o.IfMatch))
	}
	return h
}

// copySource returns the x-amz-copy-source value addressing
// bucket/key, version versionID ("" = the current one).
func copySource(bucket, key, versionID string) string {
	v := "/" + bucket + "/" + escapeKey(key)
	if versionID != "" {
		v += "?versionId=" + url.QueryEscape(versionID)
	}
	return v
}

// CopyObject copies srcKey to dstKey within bucket server-side (at most
// 5 GiB; see Copy for larger objects) and returns the new ETag.
// opts.SourceBucket and opts.SourceVersionID are ignored.
func (c *Client) CopyObject(ctx context.Context, bucket, dstKey, srcKey string, opts CopyOptions) (string, error) {
	opts.SourceBucket, opts.SourceVersionID = "", ""
	res, err := c.CopyObjectWith(ctx, bucket, dstKey, srcKey, opts)
	return res.ETag, err
}

// CopyObjectWith copies opts.SourceBucket/srcKey (version
// opts.SourceVersionID) to bucket/dstKey server-side (at most 5 GiB) and
// returns the new object's ETag and version ID.
func (c *Client) CopyObjectWith(ctx context.Context, bucket, dstKey, srcKey string, opts CopyOptions) (WriteResult, error) {
	srcBucket := opts.SourceBucket
	if srcBucket == "" {
		srcBucket = bucket
	}
	resp, err := c.do(ctx, request{
		op: "CopyObject", method: http.MethodPut, bucket: bucket, key: dstKey, hasKey: true,
		header: opts.header(srcBucket, srcKey), retryTransport: true,
	})
	if err != nil {
		return WriteResult{}, err
	}
	var out struct {
		ETag string `xml:"ETag"`
	}
	if err := decodeXML("CopyObject", resp, &out); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{ETag: unquoteETag(out.ETag), VersionID: versionIDOf(resp)}, nil
}

// escapeKey percent-encodes a key for x-amz-copy-source (keeping '/').
func escapeKey(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// --- listing ---

// ListInput selects a ListObjectsV2 page.
type ListInput struct {
	Prefix            string
	Delimiter         string
	StartAfter        string
	ContinuationToken string
	MaxKeys           int // 0 = service default (1000)
}

// ListOutput is one ListObjectsV2 page.
type ListOutput struct {
	Objects               []ObjectAttrs
	CommonPrefixes        []string
	IsTruncated           bool
	NextContinuationToken string
}

// ListObjectsV2 lists one page of bucket. Keys are requested URL-encoded
// (encoding-type=url) so any key survives the XML round trip.
func (c *Client) ListObjectsV2(ctx context.Context, bucket string, in ListInput) (ListOutput, error) {
	q := url.Values{"list-type": {"2"}, "encoding-type": {"url"}}
	if in.Prefix != "" {
		q.Set("prefix", in.Prefix)
	}
	if in.Delimiter != "" {
		q.Set("delimiter", in.Delimiter)
	}
	if in.StartAfter != "" {
		q.Set("start-after", in.StartAfter)
	}
	if in.ContinuationToken != "" {
		q.Set("continuation-token", in.ContinuationToken)
	}
	if in.MaxKeys > 0 {
		q.Set("max-keys", strconv.Itoa(in.MaxKeys))
	}
	resp, err := c.do(ctx, request{op: "ListObjectsV2", method: http.MethodGet, bucket: bucket, query: q, retryTransport: true})
	if err != nil {
		return ListOutput{}, err
	}
	var x struct {
		IsTruncated           bool   `xml:"IsTruncated"`
		NextContinuationToken string `xml:"NextContinuationToken"`
		EncodingType          string `xml:"EncodingType"`
		Contents              []struct {
			Key          string `xml:"Key"`
			LastModified string `xml:"LastModified"`
			ETag         string `xml:"ETag"`
			Size         int64  `xml:"Size"`
			StorageClass string `xml:"StorageClass"`
		} `xml:"Contents"`
		CommonPrefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
	}
	if err := decodeXML("ListObjectsV2", resp, &x); err != nil {
		return ListOutput{}, err
	}
	decode := func(s string) (string, error) {
		if !strings.EqualFold(x.EncodingType, "url") {
			return s, nil
		}
		return url.QueryUnescape(s)
	}
	out := ListOutput{IsTruncated: x.IsTruncated, NextContinuationToken: x.NextContinuationToken}
	for _, o := range x.Contents {
		key, err := decode(o.Key)
		if err != nil {
			return ListOutput{}, fmt.Errorf("s3client: ListObjectsV2: bad key encoding %q", o.Key)
		}
		t, _ := time.Parse(time.RFC3339Nano, o.LastModified)
		out.Objects = append(out.Objects, ObjectAttrs{
			Key: key, Size: o.Size, ETag: unquoteETag(o.ETag), LastModified: t.UTC(), StorageClass: o.StorageClass,
		})
	}
	for _, p := range x.CommonPrefixes {
		prefix, err := decode(p.Prefix)
		if err != nil {
			return ListOutput{}, fmt.Errorf("s3client: ListObjectsV2: bad prefix encoding %q", p.Prefix)
		}
		out.CommonPrefixes = append(out.CommonPrefixes, prefix)
	}
	if out.IsTruncated && out.NextContinuationToken == "" {
		return ListOutput{}, fmt.Errorf("s3client: ListObjectsV2: truncated page without a continuation token")
	}
	return out, nil
}

// Walk calls fn for every object of bucket with the given prefix whose key
// is greater than startAfter, in ascending key order, paging through
// ListObjectsV2. fn returning an error stops the walk with that error.
func (c *Client) Walk(ctx context.Context, bucket, prefix, startAfter string, fn func(ObjectAttrs) error) error {
	in := ListInput{Prefix: prefix, StartAfter: startAfter}
	for {
		page, err := c.ListObjectsV2(ctx, bucket, in)
		if err != nil {
			return err
		}
		for _, o := range page.Objects {
			if err := fn(o); err != nil {
				return err
			}
		}
		if !page.IsTruncated {
			return nil
		}
		in.ContinuationToken = page.NextContinuationToken
	}
}
