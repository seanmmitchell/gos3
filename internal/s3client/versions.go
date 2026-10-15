package s3client

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// --- ListObjectVersions ---

// VersionListInput selects a ListObjectVersions page.
type VersionListInput struct {
	Prefix, Delimiter          string
	KeyMarker, VersionIDMarker string
	MaxKeys                    int // 0 = service default
}

// VersionListOutput is one ListObjectVersions page. Entries holds the
// versions and delete markers in the order the service returned them
// (newest first per key), with VersionID, DeleteMarker and IsLatest set.
type VersionListOutput struct {
	Entries                            []ObjectAttrs
	CommonPrefixes                     []string
	IsTruncated                        bool
	NextKeyMarker, NextVersionIDMarker string
}

// versionEntry is a <Version> or <DeleteMarker> element (XMLName tells
// which) of a ListVersionsResult.
type versionEntry struct {
	XMLName      xml.Name
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

// listVersionsResult is the ListObjectVersions answer. The known scalar
// children are named so that Entries (",any") receives the <Version> and
// <DeleteMarker> elements, and whatever else a service adds, in document
// order; decoding them into two slices would lose their interleaving.
type listVersionsResult struct {
	Name                string `xml:"Name"`
	Prefix              string `xml:"Prefix"`
	KeyMarker           string `xml:"KeyMarker"`
	VersionIDMarker     string `xml:"VersionIdMarker"`
	NextKeyMarker       string `xml:"NextKeyMarker"`
	NextVersionIDMarker string `xml:"NextVersionIdMarker"`
	MaxKeys             string `xml:"MaxKeys"`
	Delimiter           string `xml:"Delimiter"`
	IsTruncated         bool   `xml:"IsTruncated"`
	EncodingType        string `xml:"EncodingType"`
	CommonPrefixes      []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
	Entries []versionEntry `xml:",any"`
}

// urlDecoder returns the decoding of encoding-type=url values: the
// identity unless the answer says EncodingType url (a service may ignore
// the request parameter).
func urlDecoder(op, encodingType string) func(what, s string) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(encodingType), "url") {
		return func(_, s string) (string, error) { return s, nil }
	}
	return func(what, s string) (string, error) {
		d, err := url.QueryUnescape(s)
		if err != nil {
			return "", fmt.Errorf("s3client: %s: bad %s encoding %q", op, what, s)
		}
		return d, nil
	}
}

// ListObjectVersions lists one page of versions (encoding-type=url, so
// any key survives the XML round trip; version IDs are never encoded).
// Pass the output's NextKeyMarker and NextVersionIDMarker as the next
// page's KeyMarker and VersionIDMarker.
func (c *Client) ListObjectVersions(ctx context.Context, bucket string, in VersionListInput) (VersionListOutput, error) {
	q := url.Values{"versions": {""}, "encoding-type": {"url"}}
	if in.Prefix != "" {
		q.Set("prefix", in.Prefix)
	}
	if in.Delimiter != "" {
		q.Set("delimiter", in.Delimiter)
	}
	if in.KeyMarker != "" {
		q.Set("key-marker", in.KeyMarker)
		if in.VersionIDMarker != "" {
			q.Set("version-id-marker", in.VersionIDMarker)
		}
	}
	if in.MaxKeys > 0 {
		q.Set("max-keys", strconv.Itoa(in.MaxKeys))
	}
	resp, err := c.do(ctx, request{op: "ListObjectVersions", method: http.MethodGet, bucket: bucket, query: q, retryTransport: true})
	if err != nil {
		return VersionListOutput{}, err
	}
	var x listVersionsResult
	if err := decodeXML("ListObjectVersions", resp, &x); err != nil {
		return VersionListOutput{}, err
	}
	decode := urlDecoder("ListObjectVersions", x.EncodingType)
	out := VersionListOutput{IsTruncated: x.IsTruncated, NextVersionIDMarker: x.NextVersionIDMarker}
	if out.NextKeyMarker, err = decode("key marker", x.NextKeyMarker); err != nil {
		return VersionListOutput{}, err
	}
	for _, e := range x.Entries {
		marker := false
		switch e.XMLName.Local {
		case "Version":
		case "DeleteMarker":
			marker = true
		default:
			continue
		}
		key, err := decode("key", e.Key)
		if err != nil {
			return VersionListOutput{}, err
		}
		a := ObjectAttrs{
			Key: key, VersionID: e.VersionID, DeleteMarker: marker, IsLatest: e.IsLatest,
			LastModified: parseTime(e.LastModified), StorageClass: e.StorageClass,
		}
		if !marker {
			a.ETag, a.Size = unquoteETag(e.ETag), e.Size
		}
		out.Entries = append(out.Entries, a)
	}
	for _, p := range x.CommonPrefixes {
		prefix, err := decode("prefix", p.Prefix)
		if err != nil {
			return VersionListOutput{}, err
		}
		out.CommonPrefixes = append(out.CommonPrefixes, prefix)
	}
	if out.IsTruncated && out.NextKeyMarker == "" {
		return VersionListOutput{}, fmt.Errorf("s3client: ListObjectVersions: truncated page without a next key marker")
	}
	return out, nil
}

// --- ListMultipartUploads ---

// UploadEntry is one in-progress multipart upload.
type UploadEntry struct {
	Key, UploadID string
	Initiated     time.Time
}

// ListUploadsInput selects a ListMultipartUploads page.
type ListUploadsInput struct {
	Prefix, KeyMarker, UploadIDMarker string
	MaxUploads                        int // 0 = service default
}

// ListUploadsOutput is one ListMultipartUploads page.
type ListUploadsOutput struct {
	Uploads                           []UploadEntry
	IsTruncated                       bool
	NextKeyMarker, NextUploadIDMarker string
}

// ListMultipartUploads lists one page of in-progress uploads, in key and
// then initiation order (encoding-type=url).
func (c *Client) ListMultipartUploads(ctx context.Context, bucket string, in ListUploadsInput) (ListUploadsOutput, error) {
	q := url.Values{"uploads": {""}, "encoding-type": {"url"}}
	if in.Prefix != "" {
		q.Set("prefix", in.Prefix)
	}
	if in.KeyMarker != "" {
		q.Set("key-marker", in.KeyMarker)
		if in.UploadIDMarker != "" {
			q.Set("upload-id-marker", in.UploadIDMarker)
		}
	}
	if in.MaxUploads > 0 {
		q.Set("max-uploads", strconv.Itoa(in.MaxUploads))
	}
	resp, err := c.do(ctx, request{op: "ListMultipartUploads", method: http.MethodGet, bucket: bucket, query: q, retryTransport: true})
	if err != nil {
		return ListUploadsOutput{}, err
	}
	var x struct {
		NextKeyMarker      string `xml:"NextKeyMarker"`
		NextUploadIDMarker string `xml:"NextUploadIdMarker"`
		IsTruncated        bool   `xml:"IsTruncated"`
		EncodingType       string `xml:"EncodingType"`
		Uploads            []struct {
			Key       string `xml:"Key"`
			UploadID  string `xml:"UploadId"`
			Initiated string `xml:"Initiated"`
		} `xml:"Upload"`
	}
	if err := decodeXML("ListMultipartUploads", resp, &x); err != nil {
		return ListUploadsOutput{}, err
	}
	decode := urlDecoder("ListMultipartUploads", x.EncodingType)
	out := ListUploadsOutput{IsTruncated: x.IsTruncated, NextUploadIDMarker: x.NextUploadIDMarker}
	if out.NextKeyMarker, err = decode("key marker", x.NextKeyMarker); err != nil {
		return ListUploadsOutput{}, err
	}
	for _, u := range x.Uploads {
		key, err := decode("key", u.Key)
		if err != nil {
			return ListUploadsOutput{}, err
		}
		out.Uploads = append(out.Uploads, UploadEntry{Key: key, UploadID: u.UploadID, Initiated: parseTime(u.Initiated)})
	}
	if out.IsTruncated && out.NextKeyMarker == "" {
		return ListUploadsOutput{}, fmt.Errorf("s3client: ListMultipartUploads: truncated page without a next key marker")
	}
	return out, nil
}

// --- ListParts ---

// PartEntry is one uploaded part.
type PartEntry struct {
	Number       int
	ETag         string // unquoted
	Size         int64
	LastModified time.Time
}

// ListPartsOutput is one ListParts page.
type ListPartsOutput struct {
	Parts                []PartEntry
	IsTruncated          bool
	NextPartNumberMarker int
}

// ListParts lists one page of an upload's parts: those after part number
// marker (0 = from the first), at most max (0 = service default). An
// unknown upload is NoSuchUpload (IsNotFound).
func (c *Client) ListParts(ctx context.Context, bucket, key, uploadID string, marker, max int) (ListPartsOutput, error) {
	q := url.Values{"uploadId": {uploadID}}
	if marker > 0 {
		q.Set("part-number-marker", strconv.Itoa(marker))
	}
	if max > 0 {
		q.Set("max-parts", strconv.Itoa(max))
	}
	resp, err := c.do(ctx, request{op: "ListParts", method: http.MethodGet, bucket: bucket, key: key, hasKey: true, query: q, retryTransport: true})
	if err != nil {
		return ListPartsOutput{}, err
	}
	var x struct {
		NextPartNumberMarker int  `xml:"NextPartNumberMarker"`
		IsTruncated          bool `xml:"IsTruncated"`
		Parts                []struct {
			PartNumber   int    `xml:"PartNumber"`
			ETag         string `xml:"ETag"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		} `xml:"Part"`
	}
	if err := decodeXML("ListParts", resp, &x); err != nil {
		return ListPartsOutput{}, err
	}
	out := ListPartsOutput{IsTruncated: x.IsTruncated, NextPartNumberMarker: x.NextPartNumberMarker}
	for _, p := range x.Parts {
		out.Parts = append(out.Parts, PartEntry{
			Number: p.PartNumber, ETag: unquoteETag(p.ETag), Size: p.Size, LastModified: parseTime(p.LastModified),
		})
	}
	if out.IsTruncated && out.NextPartNumberMarker <= marker {
		return ListPartsOutput{}, fmt.Errorf("s3client: ListParts: truncated page without a next part number marker")
	}
	return out, nil
}
