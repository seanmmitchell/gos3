package s3client

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// MaxParts is the largest number of parts of a multipart upload.
const MaxParts = 10000

// MinPartSize is the smallest size of a part other than the last one.
const MinPartSize = 5 << 20

// MaxCopySize is the largest object CopyObject (and one UploadPartCopy
// range) can copy.
const MaxCopySize = 5 << 30

// CompletedPart identifies one part in CompleteMultipartUpload.
type CompletedPart struct {
	Number int
	ETag   string // unquoted
}

// CreateMultipartUpload starts a multipart upload of bucket/key with the
// given object headers and returns its upload ID.
func (c *Client) CreateMultipartUpload(ctx context.Context, bucket, key string, header http.Header) (string, error) {
	h := http.Header{}
	for k, vs := range header {
		h[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	resp, err := c.do(ctx, request{
		op: "CreateMultipartUpload", method: http.MethodPost, bucket: bucket, key: key, hasKey: true,
		query: url.Values{"uploads": {""}}, header: h,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		UploadID string `xml:"UploadId"`
	}
	if err := decodeXML("CreateMultipartUpload", resp, &out); err != nil {
		return "", err
	}
	if out.UploadID == "" {
		return "", fmt.Errorf("s3client: CreateMultipartUpload: empty upload ID")
	}
	return out.UploadID, nil
}

func partQuery(uploadID string, n int) url.Values {
	return url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {uploadID}}
}

// UploadPart uploads r[0:size] as part n and returns the part's ETag.
func (c *Client) UploadPart(ctx context.Context, bucket, key, uploadID string, n int, r io.ReaderAt, size int64) (string, error) {
	return c.UploadPartWith(ctx, bucket, key, uploadID, n, r, size, PartOptions{})
}

// PartOptions are the optional parts of UploadPartWith.
type PartOptions struct {
	ContentMD5 []byte // raw MD5 of the part (nil = not sent)
}

// UploadPartWith is UploadPart with options.
func (c *Client) UploadPartWith(ctx context.Context, bucket, key, uploadID string, n int, r io.ReaderAt, size int64, opts PartOptions) (string, error) {
	var h http.Header
	if opts.ContentMD5 != nil {
		h = http.Header{"Content-Md5": {base64.StdEncoding.EncodeToString(opts.ContentMD5)}}
	}
	resp, err := c.do(ctx, request{
		op: "UploadPart", method: http.MethodPut, bucket: bucket, key: key, hasKey: true,
		query: partQuery(uploadID, n), header: h, body: readerAtBody(r, size), bodySize: size, retryTransport: true,
	})
	if err != nil {
		return "", err
	}
	drain(resp)
	return unquoteETag(resp.Header.Get("ETag")), nil
}

// UploadPartCopy copies bytes [offset, offset+length) of srcKey (in the
// same bucket) into part n; length < 0 copies the whole source. A non-empty
// ifMatch pins the source's ETag.
func (c *Client) UploadPartCopy(ctx context.Context, bucket, key, uploadID string, n int, srcKey string, offset, length int64, ifMatch string) (string, error) {
	rng := ""
	if length >= 0 {
		rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	return c.uploadPartCopy(ctx, bucket, key, uploadID, n, CopyRange{Key: srcKey, IfMatch: ifMatch}, rng)
}

// CopyRange is the source of UploadPartCopyWith.
type CopyRange struct {
	Bucket, Key, VersionID string // Bucket "" = the destination bucket
	// Offset and Length select the bytes to copy; Length <= 0 copies the
	// whole source object.
	Offset, Length int64
	IfMatch        string // pins the source's ETag ("" = no condition)
}

// UploadPartCopyWith copies a byte range of a (versioned) source object
// into part n and returns the part's ETag.
func (c *Client) UploadPartCopyWith(ctx context.Context, bucket, key, uploadID string, n int, src CopyRange) (string, error) {
	rng := ""
	if src.Length > 0 {
		rng = fmt.Sprintf("bytes=%d-%d", src.Offset, src.Offset+src.Length-1)
	}
	return c.uploadPartCopy(ctx, bucket, key, uploadID, n, src, rng)
}

// uploadPartCopy sends UploadPartCopy with the given copy-source range
// ("" = the whole source); src's Offset and Length are not used.
func (c *Client) uploadPartCopy(ctx context.Context, bucket, key, uploadID string, n int, src CopyRange, rng string) (string, error) {
	srcBucket := src.Bucket
	if srcBucket == "" {
		srcBucket = bucket
	}
	h := http.Header{"X-Amz-Copy-Source": {copySource(srcBucket, src.Key, src.VersionID)}}
	if rng != "" {
		h.Set("X-Amz-Copy-Source-Range", rng)
	}
	if src.IfMatch != "" {
		h.Set("X-Amz-Copy-Source-If-Match", quoteETag(src.IfMatch))
	}
	resp, err := c.do(ctx, request{
		op: "UploadPartCopy", method: http.MethodPut, bucket: bucket, key: key, hasKey: true,
		query: partQuery(uploadID, n), header: h, retryTransport: true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		ETag string `xml:"ETag"`
	}
	if err := decodeXML("UploadPartCopy", resp, &out); err != nil {
		return "", err
	}
	return unquoteETag(out.ETag), nil
}

// CompleteOptions are the preconditions of CompleteMultipartUpload.
type CompleteOptions struct {
	IfNoneMatch bool   // "If-None-Match: *"
	IfMatch     string // "If-Match: <etag>"
	// NoRetry sends the request once (see PutOptions.NoRetry).
	NoRetry bool
}

// CompleteMultipartUpload assembles the parts and returns the object's ETag.
func (c *Client) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart, opts CompleteOptions) (string, error) {
	res, err := c.Complete(ctx, bucket, key, uploadID, parts, opts)
	return res.ETag, err
}

// Complete is CompleteMultipartUpload returning the version ID too (see
// Put). The result has Multipart set.
func (c *Client) Complete(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart, opts CompleteOptions) (WriteResult, error) {
	type xmlPart struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	doc := struct {
		XMLName xml.Name  `xml:"CompleteMultipartUpload"`
		Parts   []xmlPart `xml:"Part"`
	}{}
	for _, p := range parts {
		doc.Parts = append(doc.Parts, xmlPart{PartNumber: p.Number, ETag: quoteETag(p.ETag)})
	}
	body, err := xml.Marshal(doc)
	if err != nil {
		return WriteResult{}, err
	}
	h := http.Header{"Content-Type": {"application/xml"}}
	if opts.IfNoneMatch {
		h.Set("If-None-Match", "*")
	}
	if opts.IfMatch != "" {
		h.Set("If-Match", quoteETag(opts.IfMatch))
	}
	resp, err := c.do(ctx, request{
		op: "CompleteMultipartUpload", method: http.MethodPost, bucket: bucket, key: key, hasKey: true,
		query: url.Values{"uploadId": {uploadID}}, header: h, body: bytesBody(body), bodySize: int64(len(body)),
		noRetry: opts.NoRetry,
	})
	if err != nil {
		return WriteResult{}, err
	}
	var out struct {
		ETag string `xml:"ETag"`
	}
	if err := decodeXML("CompleteMultipartUpload", resp, &out); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{ETag: unquoteETag(out.ETag), VersionID: versionIDOf(resp), Multipart: true}, nil
}

// AbortMultipartUpload aborts an upload. An unknown upload is not an error.
func (c *Client) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	resp, err := c.do(ctx, request{
		op: "AbortMultipartUpload", method: http.MethodDelete, bucket: bucket, key: key, hasKey: true,
		query: url.Values{"uploadId": {uploadID}}, retryTransport: true,
	})
	if IsNotFound(err) && !IsCode(err, "NoSuchBucket") {
		return nil
	}
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}
