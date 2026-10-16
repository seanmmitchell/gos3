package s3client

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// s3Namespace is the XML namespace of S3 documents.
const s3Namespace = "http://s3.amazonaws.com/doc/2006-03-01/"

// HeadBucket checks that bucket exists and is accessible.
func (c *Client) HeadBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, request{op: "HeadBucket", method: http.MethodHead, bucket: bucket, retryTransport: true})
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// --- listing ---

// BucketEntry is one bucket of ListBuckets.
type BucketEntry struct {
	Name         string
	CreationDate time.Time
	Region       string // BucketRegion, when the service reports it
}

// ListBucketsInput selects a ListBuckets page.
type ListBucketsInput struct {
	Prefix            string
	BucketRegion      string // only buckets in this region (where supported)
	ContinuationToken string
	MaxBuckets        int // 0 = service default
}

// ListBucketsOutput is one ListBuckets page.
type ListBucketsOutput struct {
	Buckets           []BucketEntry
	ContinuationToken string // "" = last page
}

// ListBuckets lists one page of the buckets the credentials can see.
// Services that do not paginate return everything in one page (and may
// ignore Prefix and BucketRegion: callers filter again when it matters).
func (c *Client) ListBuckets(ctx context.Context, in ListBucketsInput) (ListBucketsOutput, error) {
	q := url.Values{}
	if in.Prefix != "" {
		q.Set("prefix", in.Prefix)
	}
	if in.BucketRegion != "" {
		q.Set("bucket-region", in.BucketRegion)
	}
	if in.ContinuationToken != "" {
		q.Set("continuation-token", in.ContinuationToken)
	}
	if in.MaxBuckets > 0 {
		q.Set("max-buckets", strconv.Itoa(in.MaxBuckets))
	}
	resp, err := c.do(ctx, request{op: "ListBuckets", method: http.MethodGet, query: q, retryTransport: true})
	if err != nil {
		return ListBucketsOutput{}, err
	}
	var x struct {
		Buckets []struct {
			Name         string `xml:"Name"`
			CreationDate string `xml:"CreationDate"`
			BucketRegion string `xml:"BucketRegion"`
		} `xml:"Buckets>Bucket"`
		ContinuationToken string `xml:"ContinuationToken"`
	}
	if err := decodeXML("ListBuckets", resp, &x); err != nil {
		return ListBucketsOutput{}, err
	}
	out := ListBucketsOutput{ContinuationToken: x.ContinuationToken, Buckets: make([]BucketEntry, 0, len(x.Buckets))}
	for _, b := range x.Buckets {
		out.Buckets = append(out.Buckets, BucketEntry{Name: b.Name, CreationDate: parseTime(b.CreationDate), Region: b.BucketRegion})
	}
	return out, nil
}

// WalkBuckets calls fn for every bucket with the prefix, following
// continuation tokens. fn returning an error stops the walk with that
// error. A service repeating a continuation token is an error (instead of
// a walk that never ends).
func (c *Client) WalkBuckets(ctx context.Context, prefix string, fn func(BucketEntry) error) error {
	in := ListBucketsInput{Prefix: prefix}
	seen := map[string]bool{}
	for {
		page, err := c.ListBuckets(ctx, in)
		if err != nil {
			return err
		}
		for _, b := range page.Buckets {
			if !strings.HasPrefix(b.Name, prefix) {
				continue // a service ignoring the prefix
			}
			if err := fn(b); err != nil {
				return err
			}
		}
		if page.ContinuationToken == "" {
			return nil
		}
		if seen[page.ContinuationToken] {
			return fmt.Errorf("s3client: ListBuckets: the service repeated continuation token %q", page.ContinuationToken)
		}
		seen[page.ContinuationToken] = true
		in.ContinuationToken = page.ContinuationToken
	}
}

// --- creation and deletion ---

// createBucketRequest returns a CreateBucket request for bucket in region
// ("" or us-east-1: no LocationConstraint).
func createBucketRequest(bucket, region string) (request, error) {
	req := request{op: "CreateBucket", method: http.MethodPut, bucket: bucket}
	if region == "" || region == "us-east-1" {
		return req, nil
	}
	doc := struct {
		XMLName            xml.Name `xml:"CreateBucketConfiguration"`
		XMLNS              string   `xml:"xmlns,attr"`
		LocationConstraint string   `xml:"LocationConstraint"`
	}{XMLNS: s3Namespace, LocationConstraint: region}
	body, err := xml.Marshal(doc)
	if err != nil {
		return request{}, fmt.Errorf("s3client: CreateBucket: %w", err)
	}
	req.body, req.bodySize = bytesBody(body), int64(len(body))
	req.header = http.Header{"Content-Type": {"application/xml"}}
	return req, nil
}

// CreateBucket creates bucket in the client's region. An existing bucket
// owned by the caller (BucketAlreadyOwnedByYou) is not an error.
func (c *Client) CreateBucket(ctx context.Context, bucket string) error {
	req, err := createBucketRequest(bucket, c.region)
	if err != nil {
		return err
	}
	req.retryTransport = true
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

// CreateBucketOptions are the options of CreateBucketWith.
type CreateBucketOptions struct {
	// Region is the bucket's region ("" = the client's); a
	// LocationConstraint is sent unless it is us-east-1.
	Region string
	// ObjectLock creates the bucket with object lock enabled
	// (x-amz-bucket-object-lock-enabled: true).
	ObjectLock bool
}

// CreateBucketWith creates bucket. Unlike CreateBucket, an existing bucket
// is an error (BucketAlreadyOwnedByYou / BucketAlreadyExists); note that
// AWS us-east-1 answers 200 for an existing bucket the caller owns, so
// callers that must not adopt check with HeadBucket first.
//
// On AWS the request goes to the endpoint of the bucket's region (see
// ForRegion); other services get it at the client's endpoint. It is not
// retried after transport errors, and a retry after a 5xx answer may fail
// with BucketAlreadyOwnedByYou because of its own first attempt (see the
// package documentation on ambiguous writes).
func (c *Client) CreateBucketWith(ctx context.Context, bucket string, opts CreateBucketOptions) error {
	region := opts.Region
	if region == "" {
		region = c.region
	}
	cl := c
	if c.isAWS() {
		cl = c.ForRegion(region)
	}
	req, err := createBucketRequest(bucket, region)
	if err != nil {
		return err
	}
	if opts.ObjectLock {
		if req.header == nil {
			req.header = http.Header{}
		}
		req.header.Set("X-Amz-Bucket-Object-Lock-Enabled", "true")
	}
	resp, err := cl.do(ctx, req)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// DeleteBucket deletes an empty bucket. It is not retried after transport
// errors (a retry could only report NoSuchBucket for its own first
// attempt).
func (c *Client) DeleteBucket(ctx context.Context, bucket string) error {
	resp, err := c.do(ctx, request{op: "DeleteBucket", method: http.MethodDelete, bucket: bucket})
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// --- versioning ---

// versioningConfiguration is the document of Get/PutBucketVersioning.
type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

// GetBucketVersioning returns the bucket's versioning status: "",
// "Enabled" or "Suspended".
func (c *Client) GetBucketVersioning(ctx context.Context, bucket string) (string, error) {
	resp, err := c.do(ctx, request{
		op: "GetBucketVersioning", method: http.MethodGet, bucket: bucket,
		query: url.Values{"versioning": {""}}, retryTransport: true,
	})
	if err != nil {
		return "", err
	}
	var out versioningConfiguration
	if err := decodeXML("GetBucketVersioning", resp, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Status), nil
}

// PutBucketVersioning sets the bucket's versioning status ("Enabled" or
// "Suspended").
func (c *Client) PutBucketVersioning(ctx context.Context, bucket, status string) error {
	req, err := xmlRequest("PutBucketVersioning", http.MethodPut, bucket, url.Values{"versioning": {""}},
		versioningConfiguration{XMLNS: s3Namespace, Status: status})
	if err != nil {
		return err
	}
	req.retryTransport = true
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}

// --- object lock configuration ---

// ObjectLockConfig is a bucket's object lock configuration.
type ObjectLockConfig struct {
	Enabled bool
	// Mode, Days and Years are the default retention ("" = none); at most
	// one of Days and Years is set.
	Mode        string
	Days, Years int
}

// objectLockConfiguration is the document of Get/PutObjectLockConfiguration.
type objectLockConfiguration struct {
	XMLName           xml.Name        `xml:"ObjectLockConfiguration"`
	XMLNS             string          `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *objectLockRule `xml:"Rule,omitempty"`
}

type objectLockRule struct {
	DefaultRetention struct {
		Mode  string `xml:"Mode"`
		Days  int    `xml:"Days,omitempty"`
		Years int    `xml:"Years,omitempty"`
	} `xml:"DefaultRetention"`
}

// noLockConfig reports the answers of services for a bucket without object
// lock configuration.
func noLockConfig(err error) bool {
	return IsCode(err, "ObjectLockConfigurationNotFoundError") || IsCode(err, "NoSuchObjectLockConfiguration")
}

// GetObjectLockConfiguration returns the bucket's object lock
// configuration; found is false when the bucket has none
// (ObjectLockConfigurationNotFoundError).
func (c *Client) GetObjectLockConfiguration(ctx context.Context, bucket string) (cfg ObjectLockConfig, found bool, err error) {
	resp, err := c.do(ctx, request{
		op: "GetObjectLockConfiguration", method: http.MethodGet, bucket: bucket,
		query: url.Values{"object-lock": {""}}, retryTransport: true,
	})
	if noLockConfig(err) {
		return ObjectLockConfig{}, false, nil
	}
	if err != nil {
		return ObjectLockConfig{}, false, err
	}
	var out objectLockConfiguration
	if err := decodeXML("GetObjectLockConfiguration", resp, &out); err != nil {
		return ObjectLockConfig{}, false, err
	}
	cfg.Enabled = strings.TrimSpace(out.ObjectLockEnabled) == "Enabled"
	if r := out.Rule; r != nil {
		cfg.Mode = strings.TrimSpace(r.DefaultRetention.Mode)
		cfg.Days, cfg.Years = r.DefaultRetention.Days, r.DefaultRetention.Years
	}
	return cfg, true, nil
}

// PutObjectLockConfiguration replaces the bucket's object lock
// configuration (enabling object lock on a versioned bucket, where the
// service allows it). cfg.Enabled must be set: object lock cannot be
// disabled. A configuration without Mode removes the default retention.
func (c *Client) PutObjectLockConfiguration(ctx context.Context, bucket string, cfg ObjectLockConfig) error {
	if !cfg.Enabled {
		return errors.New("s3client: PutObjectLockConfiguration: object lock cannot be disabled")
	}
	doc := objectLockConfiguration{XMLNS: s3Namespace, ObjectLockEnabled: "Enabled"}
	if cfg.Mode != "" {
		doc.Rule = &objectLockRule{}
		doc.Rule.DefaultRetention.Mode = cfg.Mode
		doc.Rule.DefaultRetention.Days, doc.Rule.DefaultRetention.Years = cfg.Days, cfg.Years
	}
	req, err := xmlRequest("PutObjectLockConfiguration", http.MethodPut, bucket, url.Values{"object-lock": {""}}, doc)
	if err != nil {
		return err
	}
	req.retryTransport = true
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	drain(resp)
	return nil
}
