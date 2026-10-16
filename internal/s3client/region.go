package s3client

import (
	"context"
	"encoding/xml"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Region returns the client's signing region.
func (c *Client) Region() string { return c.region }

// ForRegion returns a client for buckets in region: same credentials,
// transport and settings, signing with region and, on AWS hosts
// (amazonaws.com), addressing the region's endpoint. Clients are memoized
// per region; ForRegion("") and ForRegion(c.Region()) return c.
//
// On other hosts only the signing region changes: such services serve all
// their regions from one endpoint, if they have regions at all.
func (c *Client) ForRegion(region string) *Client {
	if region == "" || region == c.region {
		return c
	}
	root := c.root
	if region == root.region {
		return root
	}
	if v, ok := root.regions.Load(region); ok {
		return v.(*Client)
	}
	clone := *root
	clone.region = region
	u := *root.base
	if host, ok := awsRegionalHost(u.Host, region); ok {
		u.Host = host
	}
	clone.base = &u
	v, _ := root.regions.LoadOrStore(region, &clone)
	return v.(*Client)
}

// awsRegionalHost rewrites an AWS S3 service host (host[:port]) to the
// endpoint of region: s3.amazonaws.com, s3.<r>.amazonaws.com,
// s3-<r>.amazonaws.com and s3.dualstack.<r>.amazonaws.com (also with
// s3-fips and in the amazonaws.com.cn partition) become
// s3[.dualstack].<region>.amazonaws.com[.cn]. ok is false for other hosts.
func awsRegionalHost(hostport, region string) (string, bool) {
	host, port := hostport, ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	}
	host = strings.ToLower(host)
	var suffix string
	switch {
	case strings.HasSuffix(host, ".amazonaws.com"):
		suffix = ".amazonaws.com"
	case strings.HasSuffix(host, ".amazonaws.com.cn"):
		suffix = ".amazonaws.com.cn"
	default:
		return "", false
	}
	labels := strings.Split(strings.TrimSuffix(host, suffix), ".")
	service := labels[0]
	dualstack := false
	switch {
	case service == "s3" || service == "s3-fips":
		switch rest := labels[1:]; {
		case len(rest) == 0: // global endpoint
		case len(rest) == 1 && rest[0] != "dualstack": // s3.<region>
		case len(rest) == 2 && rest[0] == "dualstack": // s3.dualstack.<region>
			dualstack = true
		default:
			return "", false
		}
	case strings.HasPrefix(service, "s3-") && !strings.HasPrefix(service, "s3-website") && len(labels) == 1:
		// The legacy dash form s3-<region> (and s3-external-1).
		service = "s3"
	default:
		return "", false
	}
	out := service
	if dualstack {
		out += ".dualstack"
	}
	out += "." + region + suffix
	if port != "" {
		out = net.JoinHostPort(out, port)
	}
	return out, true
}

// isAWS reports whether the client talks to an AWS S3 endpoint.
func (c *Client) isAWS() bool {
	_, ok := awsRegionalHost(c.base.Host, "us-east-1")
	return ok
}

// BucketRegion returns the region of bucket: the x-amz-bucket-region
// header of HEAD bucket (also sent on 301, 400 and 403 answers), else
// GetBucketLocation, whose "" means us-east-1 and "EU" eu-west-1. A
// service that reports neither (the bucket is accessible but the service
// has no regions) yields "". A missing bucket is an error (IsNotFound).
func (c *Client) BucketRegion(ctx context.Context, bucket string) (string, error) {
	resp, headErr := c.do(ctx, request{op: "HeadBucket", method: http.MethodHead, bucket: bucket, retryTransport: true})
	if headErr == nil {
		drain(resp)
		if r := resp.Header.Get("X-Amz-Bucket-Region"); r != "" {
			return r, nil
		}
	} else {
		var e *Error
		if errors.As(headErr, &e) && e.Region != "" {
			return e.Region, nil
		}
		if IsNotFound(headErr) || IsUnavailable(headErr) {
			return "", headErr
		}
	}
	loc, err := c.GetBucketLocation(ctx, bucket)
	if err != nil {
		if r, ok := IsWrongRegion(err); ok && r != "" {
			// Signed for the wrong region; the answer names the right one
			// (AWS, and gos3 with a strict region).
			return r, nil
		}
		if headErr == nil && (IsNotImplemented(err) || !IsUnavailable(err)) {
			// The bucket is accessible, but the service will not say
			// where it is.
			return "", nil
		}
		return "", err
	}
	switch loc {
	case "":
		return "us-east-1", nil
	case "EU":
		return "eu-west-1", nil
	}
	return loc, nil
}

// GetBucketLocation returns the LocationConstraint of bucket (raw: "" for
// us-east-1 on AWS, and the legacy "EU" for eu-west-1).
func (c *Client) GetBucketLocation(ctx context.Context, bucket string) (string, error) {
	resp, err := c.do(ctx, request{
		op: "GetBucketLocation", method: http.MethodGet, bucket: bucket,
		query: url.Values{"location": {""}}, retryTransport: true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		XMLName xml.Name
		Region  string `xml:",chardata"`
	}
	if err := decodeXML("GetBucketLocation", resp, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Region), nil
}
