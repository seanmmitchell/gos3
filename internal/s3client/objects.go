package s3client

import (
	"context"
	"crypto/md5"
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
}

// attrsFromHeader fills attributes from a HEAD/GET response.
func attrsFromHeader(key string, resp *http.Response) ObjectAttrs {
	a := ObjectAttrs{
		Key:          key,
		ETag:         unquoteETag(resp.Header.Get("ETag")),
		StorageClass: resp.Header.Get("X-Amz-Storage-Class"),
		Header:       resp.Header,
		Size:         resp.ContentLength,
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

// --- buckets ---

// HeadBucket checks that bucket exists and is accessible.
func (c *Client) HeadBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, request{op: "HeadBucket", method: http.MethodHead, bucket: bucket, retryTransport: true})
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// CreateBucket creates bucket in the client's region. An existing bucket
// owned by the caller (BucketAlreadyOwnedByYou) is not an error.
func (c *Client) CreateBucket(ctx context.Context, bucket string) error {
	var body []byte
	if c.region != "" && c.region != "us-east-1" {
		body = []byte(`<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>` +
			c.region + `</LocationConstraint></CreateBucketConfiguration>`)
	}
	req := request{op: "CreateBucket", method: http.MethodPut, bucket: bucket, retryTransport: true}
	if body != nil {
		req.body, req.bodySize = bytesBody(body), int64(len(body))
		req.header = http.Header{"Content-Type": {"application/xml"}}
	}
	resp, err := c.do(ctx, req)
	if IsCode(err, "BucketAlreadyOwnedByYou") {
		return nil
	}
	if err != nil {
		return err
	}
	drain(resp)
	return nil
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
	return h
}

// PutObject uploads r[0:size] as bucket/key and returns the new ETag.
func (c *Client) PutObject(ctx context.Context, bucket, key string, r io.ReaderAt, size int64, opts PutOptions) (string, error) {
	resp, err := c.do(ctx, request{
		op: "PutObject", method: http.MethodPut, bucket: bucket, key: key, hasKey: true,
		header: opts.header(), body: readerAtBody(r, size), bodySize: size,
		// Without preconditions a repeated PUT is harmless.
		retryTransport: !opts.IfNoneMatch && opts.IfMatch == "",
		noRetry:        opts.NoRetry,
	})
	if err != nil {
		return "", err
	}
	drain(resp)
	return unquoteETag(resp.Header.Get("ETag")), nil
}

// HeadObject returns the attributes of bucket/key.
func (c *Client) HeadObject(ctx context.Context, bucket, key string) (ObjectAttrs, error) {
	resp, err := c.do(ctx, request{op: "HeadObject", method: http.MethodHead, bucket: bucket, key: key, hasKey: true, retryTransport: true})
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
	resp, err := c.do(ctx, request{op: "GetObject", method: http.MethodGet, bucket: bucket, key: key, hasKey: true, header: h, retryTransport: true})
	if err != nil {
		return nil, ObjectAttrs{}, err
	}
	return resp.Body, attrsFromHeader(key, resp), nil
}

// DeleteObject deletes bucket/key. Deleting a missing key is not an error.
func (c *Client) DeleteObject(ctx context.Context, bucket, key string) error {
	resp, err := c.do(ctx, request{op: "DeleteObject", method: http.MethodDelete, bucket: bucket, key: key, hasKey: true, retryTransport: true})
	if IsNotFound(err) && !IsCode(err, "NoSuchBucket") {
		return nil
	}
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// MaxDeleteKeys is the most keys one DeleteObjects request may name;
// Client.DeleteObjects splits longer lists.
const MaxDeleteKeys = 1000

// DeleteFailure is a key a DeleteObjects request could not delete.
type DeleteFailure struct {
	Key     string
	Code    string // S3 error code, e.g. "AccessDenied"
	Message string
}

// DeleteObjectsError reports the keys DeleteObjects could not delete (the
// others were deleted). It unwraps to one *Error per key, so IsCode works
// on it.
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
		fmt.Fprintf(&b, " %q: %s", f.Key, f.Code)
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
		failed []DeleteFailure
		batch  = make([]string, 0, min(len(keys), MaxDeleteKeys))
	)
	result := func(err error) error {
		var fe error
		if len(failed) > 0 {
			fe = &DeleteObjectsError{Failed: failed}
		}
		if err == nil {
			return fe
		}
		return errors.Join(err, fe)
	}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		f, err := c.deleteBatch(ctx, bucket, batch)
		failed = append(failed, f...)
		batch = batch[:0]
		return err
	}
	for _, key := range keys {
		if key == "" {
			return result(errors.New("s3client: DeleteObjects: empty key"))
		}
		if !xmlSafe(key) {
			if err := c.DeleteObject(ctx, bucket, key); err != nil {
				return result(err)
			}
			continue
		}
		batch = append(batch, key)
		if len(batch) == MaxDeleteKeys {
			if err := flush(); err != nil {
				return result(err)
			}
		}
	}
	return result(flush())
}

// deleteBatch sends one quiet DeleteObjects request for keys and returns
// the keys it failed to delete (NoSuchKey is not a failure).
func (c *Client) deleteBatch(ctx context.Context, bucket string, keys []string) ([]DeleteFailure, error) {
	type object struct {
		Key string `xml:"Key"`
	}
	doc := struct {
		XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Delete"`
		Quiet   bool     `xml:"Quiet"`
		Objects []object `xml:"Object"`
	}{Quiet: true, Objects: make([]object, len(keys))}
	for i, k := range keys {
		doc.Objects[i].Key = k
	}
	body, err := xml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("s3client: DeleteObjects: %w", err)
	}
	sum := md5.Sum(body)
	resp, err := c.do(ctx, request{
		op: "DeleteObjects", method: http.MethodPost, bucket: bucket, query: url.Values{"delete": {""}},
		header: http.Header{
			"Content-Type": {"application/xml"},
			// S3 requires an integrity header on DeleteObjects.
			"Content-Md5": {base64.StdEncoding.EncodeToString(sum[:])},
		},
		body: bytesBody(body), bodySize: int64(len(body)),
		// Deleting is idempotent.
		retryTransport: true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Errors []DeleteFailure `xml:"Error"`
	}
	if err := decodeXML("DeleteObjects", resp, &out); err != nil {
		return nil, err
	}
	var failed []DeleteFailure
	for _, f := range out.Errors {
		if f.Code != "NoSuchKey" {
			failed = append(failed, f)
		}
	}
	return failed, nil
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
	h.Set("X-Amz-Copy-Source", "/"+srcBucket+"/"+escapeKey(srcKey))
	if o.IfMatch != "" {
		h.Set("X-Amz-Copy-Source-If-Match", quoteETag(o.IfMatch))
	}
	return h
}

// CopyObject copies srcKey to dstKey within bucket server-side (at most
// 5 GiB; see Copy for larger objects) and returns the new ETag.
func (c *Client) CopyObject(ctx context.Context, bucket, dstKey, srcKey string, opts CopyOptions) (string, error) {
	resp, err := c.do(ctx, request{
		op: "CopyObject", method: http.MethodPut, bucket: bucket, key: dstKey, hasKey: true,
		header: opts.header(bucket, srcKey), retryTransport: true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		ETag string `xml:"ETag"`
	}
	if err := decodeXML("CopyObject", resp, &out); err != nil {
		return "", err
	}
	return unquoteETag(out.ETag), nil
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
