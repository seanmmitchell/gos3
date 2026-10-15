package s3client

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// taggingDoc is the <Tagging> document of the object tagging operations.
type taggingDoc struct {
	XMLName xml.Name `xml:"Tagging"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Tags    []tag    `xml:"TagSet>Tag"`
}

type tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// sortedKeys returns the keys of tags in ascending order.
func sortedKeys(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// GetObjectTagging returns a version's tags (versionID "" = the current
// one); an object without tags yields an empty map.
func (c *Client) GetObjectTagging(ctx context.Context, bucket, key, versionID string) (map[string]string, error) {
	resp, err := c.do(ctx, request{
		op: "GetObjectTagging", method: http.MethodGet, bucket: bucket, key: key, hasKey: true,
		query: objectQuery("tagging", versionID), retryTransport: true,
	})
	if err != nil {
		return nil, err
	}
	var out taggingDoc
	if err := decodeXML("GetObjectTagging", resp, &out); err != nil {
		return nil, err
	}
	tags := make(map[string]string, len(out.Tags))
	for _, t := range out.Tags {
		tags[t.Key] = t.Value
	}
	return tags, nil
}

// PutObjectTagging replaces a version's tags (versionID "" = the current
// one).
func (c *Client) PutObjectTagging(ctx context.Context, bucket, key, versionID string, tags map[string]string) error {
	doc := taggingDoc{XMLNS: s3Namespace, Tags: make([]tag, 0, len(tags))}
	for _, k := range sortedKeys(tags) {
		doc.Tags = append(doc.Tags, tag{Key: k, Value: tags[k]})
	}
	req, err := xmlRequest("PutObjectTagging", http.MethodPut, bucket, objectQuery("tagging", versionID), doc)
	if err != nil {
		return err
	}
	req.key, req.hasKey, req.retryTransport = key, true, true
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// DeleteObjectTagging removes a version's tags (versionID "" = the
// current one).
func (c *Client) DeleteObjectTagging(ctx context.Context, bucket, key, versionID string) error {
	resp, err := c.do(ctx, request{
		op: "DeleteObjectTagging", method: http.MethodDelete, bucket: bucket, key: key, hasKey: true,
		query: objectQuery("tagging", versionID), retryTransport: true,
	})
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// EncodeTagging returns the x-amz-tagging header value for tags: URL
// query encoding with the keys sorted, e.g. "a=1&b=x%20y". Spaces are
// written as %20 rather than "+", which services decoding the header as a
// URL query and as a form both read as a space.
func EncodeTagging(tags map[string]string) string {
	esc := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	var b strings.Builder
	for i, k := range sortedKeys(tags) {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(esc(k))
		b.WriteByte('=')
		b.WriteString(esc(tags[k]))
	}
	return b.String()
}
