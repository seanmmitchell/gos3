package s3

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/seanmmitchell/gos3/internal/s3err"
)

// requestTarget is the addressed resource of a request.
type requestTarget struct {
	Bucket string
	Key    string
	// VirtualHost is true when the bucket came from the Host header.
	VirtualHost bool
}

// parseTarget extracts bucket and key from r, using virtual-hosted-style
// addressing when the Host is "<bucket>.<domain>" for one of domains
// (port ignored, longest matching domain wins) and path-style otherwise.
// Keys are taken from the percent-decoded request path verbatim: trailing
// slashes, repeated slashes, "." and ".." segments are kept as-is.
func parseTarget(r *http.Request, domains []string) (requestTarget, error) {
	raw := rawRequestPath(r)
	if !strings.HasPrefix(raw, "/") {
		return requestTarget{}, s3err.ErrInvalidURI
	}
	if b := virtualHostBucket(r.Host, domains); b != "" {
		key, err := unescapePath(raw[1:])
		if err != nil {
			return requestTarget{}, err
		}
		if !plausibleBucketName(b) {
			return requestTarget{}, s3err.ErrInvalidBucketName.With("BucketName", b)
		}
		return requestTarget{Bucket: b, Key: key, VirtualHost: true}, nil
	}
	rest := raw[1:]
	if rest == "" {
		return requestTarget{}, nil
	}
	rawBucket, rawKey, _ := strings.Cut(rest, "/")
	bucket, err := unescapePath(rawBucket)
	if err != nil {
		return requestTarget{}, err
	}
	key, err := unescapePath(rawKey)
	if err != nil {
		return requestTarget{}, err
	}
	if bucket == "" {
		// "//key": an empty bucket segment cannot address anything.
		return requestTarget{}, s3err.ErrInvalidBucketName.WithMessage("The specified bucket is not valid.")
	}
	if !plausibleBucketName(bucket) {
		return requestTarget{}, s3err.ErrInvalidBucketName.With("BucketName", bucket)
	}
	return requestTarget{Bucket: bucket, Key: key}, nil
}

// rawRequestPath returns the escaped request path as sent by the client
// (from RequestURI when available, so handlers such as http.StripPrefix
// that rewrite r.URL do not change the addressed key).
func rawRequestPath(r *http.Request) string {
	if u := r.RequestURI; strings.HasPrefix(u, "/") {
		if i := strings.IndexByte(u, '?'); i >= 0 {
			u = u[:i]
		}
		return u
	}
	if r.URL == nil {
		return "/"
	}
	p := r.URL.EscapedPath()
	if p == "" {
		p = "/"
	}
	return p
}

// unescapePath percent-decodes a path component ('+' stays '+').
func unescapePath(s string) (string, error) {
	if !strings.ContainsRune(s, '%') {
		return s, nil
	}
	u, err := url.PathUnescape(s)
	if err != nil {
		return "", s3err.ErrInvalidURI
	}
	return u, nil
}

// plausibleBucketName is a loose sanity check for bucket names taken from
// requests (strict validation is CreateBucket's job, see validBucketName):
// it rejects names no bucket can ever have, such as ones containing '/' or
// control characters.
func plausibleBucketName(b string) bool {
	if b == "" || len(b) > 255 || !utf8.ValidString(b) {
		return false
	}
	for i := 0; i < len(b); i++ {
		if c := b[i]; c < 0x20 || c == 0x7f || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// virtualHostBucket returns the bucket named by host for virtual-hosted-style
// requests, or "" when host is not "<bucket>.<domain>" for any domain.
func virtualHostBucket(host string, domains []string) string {
	if len(domains) == 0 || host == "" {
		return ""
	}
	h := normalizeHost(host)
	best := ""
	for _, d := range domains {
		d = strings.TrimSpace(d)
		if i := strings.Index(d, "://"); i >= 0 { // tolerate "http://domain:port/"
			d = d[i+3:]
		}
		d, _, _ = strings.Cut(d, "/")
		d = normalizeHost(strings.TrimPrefix(d, "."))
		if d == "" || len(d) <= len(best) {
			continue
		}
		if len(h) > len(d)+1 && strings.HasSuffix(h, d) && h[len(h)-len(d)-1] == '.' {
			best = d
		}
	}
	if best == "" {
		return ""
	}
	return h[:len(h)-len(best)-1]
}

// normalizeHost lowercases host and strips a port and a trailing dot.
func normalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if strings.HasPrefix(h, "[") { // IPv6 literal: never a virtual host
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return strings.TrimSuffix(h, ".")
}

// bucketSubresource maps a bucket subresource query parameter to the
// operation for each method ("" = MethodNotAllowed).
type bucketSubresource struct {
	param            string
	get, put, delete string
}

// bucketSubresources is checked in order; the first parameter present wins.
var bucketSubresources = []bucketSubresource{
	{"versioning", "GetBucketVersioning", "PutBucketVersioning", ""},
	{"tagging", "GetBucketTagging", "PutBucketTagging", "DeleteBucketTagging"},
	{"cors", "GetBucketCors", "PutBucketCors", "DeleteBucketCors"},
	{"policyStatus", "GetBucketPolicyStatus", "", ""},
	{"policy", "GetBucketPolicy", "PutBucketPolicy", "DeleteBucketPolicy"},
	{"lifecycle", "GetBucketLifecycle", "PutBucketLifecycle", "DeleteBucketLifecycle"},
	{"encryption", "GetBucketEncryption", "PutBucketEncryption", "DeleteBucketEncryption"},
	{"acl", "GetBucketAcl", "PutBucketAcl", ""},
	{"object-lock", "GetObjectLockConfiguration", "PutObjectLockConfiguration", ""},
	{"ownershipControls", "GetBucketOwnershipControls", "PutBucketOwnershipControls", "DeleteBucketOwnershipControls"},
	{"website", "GetBucketWebsite", "PutBucketWebsite", "DeleteBucketWebsite"},
	{"notification", "GetBucketNotification", "PutBucketNotification", ""},
	{"logging", "GetBucketLogging", "PutBucketLogging", ""},
	{"publicAccessBlock", "GetPublicAccessBlock", "PutPublicAccessBlock", "DeletePublicAccessBlock"},
	{"replication", "GetBucketReplication", "PutBucketReplication", "DeleteBucketReplication"},
	{"accelerate", "GetBucketAccelerate", "PutBucketAccelerate", ""},
	{"requestPayment", "GetBucketRequestPayment", "PutBucketRequestPayment", ""},
	{"location", "GetBucketLocation", "", ""},
	{"versions", "ListObjectVersions", "", ""},
	{"uploads", "ListMultipartUploads", "", ""},
}

// objectSubresource maps an object subresource to operations per method.
type objectSubresource struct {
	param                  string
	get, put, delete, post string
}

var objectSubresources = []objectSubresource{
	{"uploadId", "ListParts", "UploadPart", "AbortMultipartUpload", "CompleteMultipartUpload"},
	{"tagging", "GetObjectTagging", "PutObjectTagging", "DeleteObjectTagging", ""},
	{"acl", "GetObjectAcl", "PutObjectAcl", "", ""},
	{"retention", "GetObjectRetention", "PutObjectRetention", "", ""},
	{"legal-hold", "GetObjectLegalHold", "PutObjectLegalHold", "", ""},
	{"attributes", "GetObjectAttributes", "", "", ""},
	{"uploads", "", "", "", "CreateMultipartUpload"},
	{"restore", "", "", "", "RestoreObject"},
	{"select", "", "", "", "SelectObjectContent"},
}

// unsupportedSubresources are S3 subresources gos3 does not implement; any
// request carrying one gets NotImplemented. Other unknown query parameters
// are ignored, like AWS does.
var unsupportedSubresources = []string{
	"analytics", "inventory", "metrics", "intelligent-tiering", "torrent",
	"metadataTable", "metadataConfiguration", "metadataInventoryTable",
	"metadataJournalTable", "session", "abac", "renameObject",
}

// detectOp identifies the S3 operation of a request (see the operation
// detection table in docs/ARCHITECTURE.md). OPTIONS requests are CORS
// preflights and are handled before detectOp is called.
func detectOp(method string, t requestTarget, q url.Values, h http.Header) (string, error) {
	for _, p := range unsupportedSubresources {
		if q.Has(p) {
			return "", s3err.ErrNotImplemented.WithMessage("The %q subresource is not implemented.", p)
		}
	}
	notAllowed := func(kind resourceKind) error {
		return s3err.ErrMethodNotAllowed.With("Method", method).With("ResourceType", kind.String())
	}
	switch {
	case t.Bucket == "":
		if method == http.MethodGet {
			return "ListBuckets", nil
		}
		return "", notAllowed(resService)

	case t.Key == "":
		switch method {
		case http.MethodHead:
			return "HeadBucket", nil
		case http.MethodPost:
			if q.Has("delete") {
				return "DeleteObjects", nil
			}
			if isMultipartForm(h.Get("Content-Type")) {
				return "PostObject", nil
			}
			return "", notAllowed(resBucket)
		case http.MethodGet, http.MethodPut, http.MethodDelete:
		default:
			return "", notAllowed(resBucket)
		}
		for _, sr := range bucketSubresources {
			if !q.Has(sr.param) {
				continue
			}
			var op string
			switch method {
			case http.MethodGet:
				op = sr.get
			case http.MethodPut:
				op = sr.put
			case http.MethodDelete:
				op = sr.delete
			}
			if op == "" {
				return "", notAllowed(resBucket)
			}
			return op, nil
		}
		switch method {
		case http.MethodGet:
			if q.Get("list-type") == "2" {
				return "ListObjectsV2", nil
			}
			return "ListObjects", nil
		case http.MethodPut:
			return "CreateBucket", nil
		default:
			return "DeleteBucket", nil
		}

	default:
		switch method {
		case http.MethodHead:
			return "HeadObject", nil
		case http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPost:
		default:
			return "", notAllowed(resObject)
		}
		for _, sr := range objectSubresources {
			if !q.Has(sr.param) {
				continue
			}
			var op string
			switch method {
			case http.MethodGet:
				op = sr.get
			case http.MethodPut:
				op = sr.put
				if op == "UploadPart" && h.Get("X-Amz-Copy-Source") != "" {
					op = "UploadPartCopy"
				}
			case http.MethodDelete:
				op = sr.delete
			case http.MethodPost:
				op = sr.post
			}
			if op == "" {
				return "", notAllowed(resObject)
			}
			return op, nil
		}
		switch method {
		case http.MethodGet:
			return "GetObject", nil
		case http.MethodPut:
			if h.Get("X-Amz-Copy-Source") != "" {
				return "CopyObject", nil
			}
			return "PutObject", nil
		case http.MethodDelete:
			return "DeleteObject", nil
		default:
			return "", notAllowed(resObject)
		}
	}
}

// isMultipartForm reports whether a Content-Type is multipart/form-data.
func isMultipartForm(ct string) bool {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.EqualFold(strings.TrimSpace(mt), "multipart/form-data")
}
