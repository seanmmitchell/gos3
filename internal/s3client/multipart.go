package s3client

import (
	"context"
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
	resp, err := c.do(ctx, request{
		op: "UploadPart", method: http.MethodPut, bucket: bucket, key: key, hasKey: true,
		query: partQuery(uploadID, n), body: readerAtBody(r, size), bodySize: size, retryTransport: true,
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
	h := http.Header{"X-Amz-Copy-Source": {"/" + bucket + "/" + escapeKey(srcKey)}}
	if length >= 0 {
		h.Set("X-Amz-Copy-Source-Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}
	if ifMatch != "" {
		h.Set("X-Amz-Copy-Source-If-Match", quoteETag(ifMatch))
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
		return "", err
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
		return "", err
	}
	var out struct {
		ETag string `xml:"ETag"`
	}
	if err := decodeXML("CompleteMultipartUpload", resp, &out); err != nil {
		return "", err
	}
	return unquoteETag(out.ETag), nil
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
