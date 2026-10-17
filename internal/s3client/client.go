// Package s3client is a small S3 API client used by the remote backings
// (docs/backings.md): the s3 store and the native (passthrough) buckets of
// an S3 account.
//
// It covers exactly the operations those need: objects (also addressed by
// version ID), listings (ListObjectsV2, ListObjectVersions, uploads and
// parts), multipart uploads and server-side copies, buckets (listing,
// creation, regions, versioning, object lock configuration) and the object
// sub-resources retention, legal hold and tagging. It signs every request
// with SigV4 through internal/auth and retries transient failures. It is
// built on net/http only; the AWS SDK is a test-only dependency of gos3.
//
// Clients sign for one region; Client.ForRegion returns a client for
// buckets in another one (on AWS also addressing that region's endpoint),
// and Client.BucketRegion finds a bucket's region.
//
// Helpers on top of the raw operations:
//   - Spool buffers a stream in memory and spills it to a temporary file,
//     so it can be uploaded (and retried) from an io.ReaderAt of known size.
//   - Client.Upload uploads an io.ReaderAt with a single PUT or, above the
//     part size, a parallel multipart upload.
//   - Client.Copy copies an object server-side, with a multipart copy above
//     the 5 GiB limit of CopyObject.
//   - Client.NewReader returns a lazy, seekable reader over an object that
//     issues ranged GETs pinned to one ETag.
//
// # Retries and conditional writes
//
// Requests are retried (up to Config.MaxAttempts) after 5xx answers,
// SlowDown and RequestTimeout, and idempotent requests also after transport
// errors. Conditional writes (PutOptions.IfNoneMatch / IfMatch, and the
// matching CompleteOptions) are not retried after transport errors, but
// they ARE retried after a 5xx answer: the service may have applied the
// write before failing, so the retry can then fail with 412 against the
// caller's own first attempt. A 412 (IsPreconditionFailed) from a
// conditional write is therefore ambiguous, as is any IsUnavailable error:
// callers that need to know whether their write happened must read the
// object back and compare it with what they wrote. PutOptions.NoRetry
// (CompleteOptions.NoRetry) sends such a write once, so the first failure
// is reported unchanged.
package s3client

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// Config configures a Client.
type Config struct {
	// Endpoint is the service base URL, "http(s)://host[:port]" (no path).
	Endpoint  string
	Region    string // signing region (default us-east-1)
	AccessKey string
	SecretKey string
	// PathStyle addresses buckets as <endpoint>/<bucket>/<key>; otherwise
	// as <bucket>.<host>/<key> (virtual-hosted style, required by AWS for
	// buckets created since 2020).
	PathStyle bool
	// HTTPClient sends the requests (default: a client with sensible
	// timeouts for connection setup; transfers are bounded by contexts).
	HTTPClient *http.Client
	// MaxAttempts bounds how often a request is sent when it fails
	// transiently (default 4).
	MaxAttempts int
}

// Client is an S3 API client. It is safe for concurrent use.
type Client struct {
	base        *url.URL
	region      string
	creds       auth.Credentials
	pathStyle   bool
	hc          *http.Client
	maxAttempts int
	// signPayload signs body hashes instead of UNSIGNED-PAYLOAD; used over
	// plain HTTP, where nothing else protects the body.
	signPayload bool
	// now is the clock used for signing (tests).
	now func() time.Time
	// root is the client New returned; ForRegion clones share its
	// regions map and settings.
	root *Client
	// regions memoizes the ForRegion clones of root (region -> *Client);
	// only root's is used.
	regions *sync.Map
}

// New returns a client for cfg.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("s3client: endpoint %q must be an absolute http(s) URL", cfg.Endpoint)
	}
	if u.Path != "" || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("s3client: endpoint %q must not have a path, query or credentials", cfg.Endpoint)
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3client: access key and secret key are required")
	}
	c := &Client{
		base:        u,
		region:      cfg.Region,
		creds:       auth.Credentials{AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey},
		pathStyle:   cfg.PathStyle,
		hc:          cfg.HTTPClient,
		maxAttempts: cfg.MaxAttempts,
		signPayload: u.Scheme == "http",
		now:         time.Now,
		regions:     new(sync.Map),
	}
	c.root = c
	if c.region == "" {
		c.region = auth.DefaultRegion
	}
	if c.hc == nil {
		c.hc = defaultHTTPClient()
	}
	if c.maxAttempts <= 0 {
		c.maxAttempts = 4
	}
	return c, nil
}

func defaultHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	t.ResponseHeaderTimeout = 2 * time.Minute
	t.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{
		Transport: t,
		// S3 never redirects a correctly addressed request; following a
		// redirect would drop the signature anyway.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Endpoint returns the configured endpoint URL.
func (c *Client) Endpoint() string { return c.base.String() }

// --- errors ---

// Error is an error answer of the S3 service (or a transport failure, then
// with StatusCode 0 and Err set).
type Error struct {
	StatusCode int
	Code       string // S3 error code, e.g. "NoSuchKey" ("" if the body had none)
	Message    string
	RequestID  string
	Op         string // e.g. "PutObject"
	Err        error  // transport error, if any
	// Header holds the response headers of the error answer (nil for
	// transport failures): x-amz-delete-marker, x-amz-version-id,
	// x-amz-bucket-region, ...
	Header http.Header
	// Region is the bucket's region when the answer reported one (the
	// x-amz-bucket-region header or a <Region> element), e.g. on a 301
	// PermanentRedirect or a 400 AuthorizationHeaderMalformed.
	Region string
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("s3client: %s: %v", e.Op, e.Err)
	}
	msg := e.Code
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return fmt.Sprintf("s3client: %s: HTTP %d %s", e.Op, e.StatusCode, msg)
}

func (e *Error) Unwrap() error { return e.Err }

// statusOf returns the HTTP status of an *Error in err's chain (0 if none).
func statusOf(err error) (int, string) {
	var e *Error
	if errors.As(err, &e) {
		return e.StatusCode, e.Code
	}
	return 0, ""
}

// IsNotFound reports whether err is a 404 answer (NoSuchKey, NoSuchBucket,
// NoSuchUpload, NoSuchVersion or a body-less 404).
func IsNotFound(err error) bool {
	st, _ := statusOf(err)
	return st == http.StatusNotFound
}

// IsCode reports whether err is an S3 error answer with the given code.
func IsCode(err error, code string) bool {
	_, c := statusOf(err)
	return c != "" && c == code
}

// IsPreconditionFailed reports whether err is a failed conditional request
// (412, or 409 ConditionalRequestConflict for a concurrent conditional
// write).
func IsPreconditionFailed(err error) bool {
	st, code := statusOf(err)
	return st == http.StatusPreconditionFailed || code == "ConditionalRequestConflict"
}

// IsUnavailable reports whether err means the service could not be reached
// or failed on its side (transport error, 5xx) rather than rejecting the
// request.
func IsUnavailable(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Err != nil || e.StatusCode >= 500
}

// IsDeleteMarker reports whether err is the answer to a HEAD/GET/DELETE of
// a delete marker (404 or 405 with x-amz-delete-marker: true) and returns
// the marker's version ID (x-amz-version-id, "" when not sent).
func IsDeleteMarker(err error) (versionID string, ok bool) {
	var e *Error
	if !errors.As(err, &e) || e.Header == nil {
		return "", false
	}
	if e.StatusCode != http.StatusNotFound && e.StatusCode != http.StatusMethodNotAllowed {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(e.Header.Get("X-Amz-Delete-Marker")), "true") {
		return "", false
	}
	return e.Header.Get("X-Amz-Version-Id"), true
}

// IsWrongRegion reports whether err says the bucket lives in another
// region (301 PermanentRedirect or 307 TemporaryRedirect, 400
// AuthorizationHeaderMalformed or IllegalLocationConstraintException) and
// returns that region when the answer named it. AuthorizationHeaderMalformed
// only counts when it is about the region (it names one, or its message
// mentions the region), since it also reports other malformed signatures.
func IsWrongRegion(err error) (region string, ok bool) {
	var e *Error
	if !errors.As(err, &e) || e.Err != nil {
		return "", false
	}
	switch {
	case e.StatusCode == http.StatusMovedPermanently, e.Code == "PermanentRedirect":
	case e.StatusCode == http.StatusTemporaryRedirect, e.Code == "TemporaryRedirect":
	case e.Code == "IllegalLocationConstraintException":
	case e.Code == "AuthorizationHeaderMalformed":
		if e.Region == "" && !strings.Contains(strings.ToLower(e.Message), "region") {
			return "", false
		}
	default:
		return "", false
	}
	return e.Region, true
}

// IsNotImplemented reports a 501 / NotImplemented answer.
func IsNotImplemented(err error) bool {
	st, code := statusOf(err)
	return st == http.StatusNotImplemented || code == "NotImplemented"
}

// IsAccessDenied reports a 403 answer (or an AccessDenied failure of a
// DeleteObjects entry).
func IsAccessDenied(err error) bool {
	st, code := statusOf(err)
	return st == http.StatusForbidden || code == "AccessDenied"
}

// --- requests ---

// request describes one API call. body, when set, must return a fresh
// reader on every call so the request can be retried.
type request struct {
	op       string
	method   string
	bucket   string
	key      string
	hasKey   bool // address an object (even with an empty key)
	query    url.Values
	header   http.Header
	body     func() io.Reader
	bodySize int64
	// retryTransport allows retrying after a transport error, i.e. when
	// the request may have reached the server (idempotent requests only).
	retryTransport bool
	// noRetry sends the request exactly once.
	noRetry bool
	// md5 sends Content-MD5, computed from body (meant for small bodies
	// such as XML documents: the body is read once more for it).
	md5 bool
}

// url builds the request URL.
func (c *Client) url(bucket, key string, hasKey bool, q url.Values) *url.URL {
	u := *c.base
	path := "/"
	switch {
	case bucket == "":
	case c.pathStyle:
		path = "/" + bucket
		if hasKey {
			path += "/" + key
		}
	default:
		u.Host = bucket + "." + u.Host
		if hasKey {
			path = "/" + key
		}
	}
	u.Path = path
	u.RawPath = ""
	if len(q) > 0 {
		// S3 sub-resources such as "uploads" or "delete" have no value; Go's
		// encoder writes "uploads=", which S3 accepts as well.
		u.RawQuery = q.Encode()
	}
	return &u
}

// do sends req (retrying transient failures) and returns the response of a
// 2xx answer; the caller must close its body. Error answers become *Error.
func (c *Client) do(ctx context.Context, req request) (*http.Response, error) {
	if req.md5 && req.body != nil {
		sum, err := md5Of(req.body())
		if err != nil {
			return nil, &Error{Op: req.op, Err: err}
		}
		req.header = req.header.Clone()
		if req.header == nil {
			req.header = http.Header{}
		}
		req.header.Set("Content-Md5", base64.StdEncoding.EncodeToString(sum))
	}
	var lastErr error
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return nil, lastErr
			}
		}
		resp, err := c.send(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if req.noRetry || ctx.Err() != nil || !c.retryable(req, err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// retryable reports whether err (from send) is worth another attempt.
func (c *Client) retryable(req request, err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	if e.Err != nil {
		return req.retryTransport
	}
	switch e.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.Code == "SlowDown" || e.Code == "RequestTimeout"
}

func sleepBackoff(ctx context.Context, attempt int) error {
	d := time.Duration(100<<min(attempt, 6)) * time.Millisecond
	d = d/2 + rand.N(d/2+1)
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// send performs one attempt.
func (c *Client) send(ctx context.Context, req request) (*http.Response, error) {
	u := c.url(req.bucket, req.key, req.hasKey, req.query)
	var body io.Reader
	payload := auth.PayloadEmptySHA256
	if req.body != nil {
		body = req.body()
		payload = auth.PayloadUnsigned
		if c.signPayload {
			h := sha256.New()
			if _, err := io.Copy(h, req.body()); err != nil {
				return nil, &Error{Op: req.op, Err: err}
			}
			payload = hex.EncodeToString(h.Sum(nil))
		}
	}
	r, err := http.NewRequestWithContext(ctx, req.method, u.String(), body)
	if err != nil {
		return nil, &Error{Op: req.op, Err: err}
	}
	// NewRequest re-parses the URL; restore the unescaped path so the
	// signer encodes it the AWS way and pins RawPath.
	r.URL.Path, r.URL.RawPath = u.Path, ""
	for k, vs := range req.header {
		r.Header[k] = append([]string(nil), vs...)
	}
	if r.Header.Get("Accept-Encoding") == "" {
		// Without an explicit Accept-Encoding, net/http asks for gzip and
		// transparently decompresses "Content-Encoding: gzip" answers,
		// which would alter the bytes (and drop the size) of objects stored
		// gzip-encoded.
		r.Header.Set("Accept-Encoding", "identity")
	}
	// net/http may keep reading a request body after Do returns (when the
	// server answers before reading it, the transport's writer can still
	// be sending), and callers release the body's source (a Spool, a file)
	// as soon as send returns. Track every body handed to the transport
	// and wait until the transport closed them all: it always does, also
	// on errors.
	var bodies bodyTracker
	if req.body != nil {
		r.ContentLength = req.bodySize
		if req.bodySize == 0 {
			r.Body = http.NoBody
		} else {
			r.Body = bodies.wrap(body)
		}
		r.GetBody = func() (io.ReadCloser, error) { return bodies.wrap(req.body()), nil }
	}
	if err := auth.SignRequest(r, c.creds, c.region, "s3", payload, c.now()); err != nil {
		return nil, &Error{Op: req.op, Err: err}
	}
	resp, err := c.hc.Do(r)
	bodies.wait()
	if err != nil {
		return nil, &Error{Op: req.op, Err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	return nil, parseError(req.op, resp)
}

// bodyTracker counts the request bodies of one request that the transport
// has not closed yet (see send).
type bodyTracker struct{ wg sync.WaitGroup }

// wrap returns r as a request body counted by t.
func (t *bodyTracker) wrap(r io.Reader) io.ReadCloser {
	t.wg.Add(1)
	return &trackedBody{r: r, t: t}
}

// wait blocks until every body returned by wrap has been closed.
func (t *bodyTracker) wait() { t.wg.Wait() }

type trackedBody struct {
	r    io.Reader
	t    *bodyTracker
	once sync.Once
}

func (b *trackedBody) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *trackedBody) Close() error {
	b.once.Do(b.t.wg.Done)
	return nil
}

// parseError turns an error answer into an *Error. Answers without a body
// (HEAD requests) get a code from their status where S3 clients agree on
// one: a HEAD of a delete marker addressed by version ID, for example,
// answers a body-less 405, which becomes MethodNotAllowed.
func parseError(op string, resp *http.Response) *Error {
	e := &Error{
		Op:         op,
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Amz-Request-Id"),
		Header:     resp.Header.Clone(),
		Region:     resp.Header.Get("X-Amz-Bucket-Region"),
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var x struct {
		Code      string `xml:"Code"`
		Message   string `xml:"Message"`
		RequestID string `xml:"RequestId"`
		Region    string `xml:"Region"`
	}
	if len(bytes.TrimSpace(data)) > 0 && xml.Unmarshal(data, &x) == nil {
		e.Code, e.Message = x.Code, x.Message
		if x.RequestID != "" {
			e.RequestID = x.RequestID
		}
		if e.Region == "" {
			e.Region = strings.TrimSpace(x.Region)
		}
	}
	if e.Code == "" {
		switch resp.StatusCode {
		case http.StatusMovedPermanently:
			e.Code = "PermanentRedirect"
		case http.StatusNotFound:
			e.Code = "NotFound"
		case http.StatusMethodNotAllowed:
			e.Code = "MethodNotAllowed"
		case http.StatusPreconditionFailed:
			e.Code = "PreconditionFailed"
		case http.StatusNotModified:
			e.Code = "NotModified"
		case http.StatusNotImplemented:
			e.Code = "NotImplemented"
		}
	}
	return e
}

// decodeXML reads an XML response body into v. S3 may answer some calls
// (CompleteMultipartUpload, CopyObject, UploadPartCopy) with 200 and an
// <Error> body; that is returned as an *Error.
func decodeXML(op string, resp *http.Response, v any) error {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return &Error{Op: op, Err: err}
	}
	var root struct {
		XMLName   xml.Name
		Code      string `xml:"Code"`
		Message   string `xml:"Message"`
		RequestID string `xml:"RequestId"`
	}
	if err := xml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("s3client: %s: decode response: %w", op, err)
	}
	if root.XMLName.Local == "Error" {
		return &Error{
			Op: op, StatusCode: http.StatusInternalServerError, Code: root.Code, Message: root.Message,
			RequestID: root.RequestID, Header: resp.Header.Clone(),
		}
	}
	if v == nil {
		return nil
	}
	if err := xml.Unmarshal(data, v); err != nil {
		return fmt.Errorf("s3client: %s: decode response: %w", op, err)
	}
	return nil
}

// drain discards and closes a response body.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// readerAtBody returns a body factory over r[0:size].
func readerAtBody(r io.ReaderAt, size int64) func() io.Reader {
	return func() io.Reader { return io.NewSectionReader(r, 0, size) }
}

// bytesBody returns a body factory over b.
func bytesBody(b []byte) func() io.Reader {
	return func() io.Reader { return bytes.NewReader(b) }
}

// md5Of returns the raw MD5 of everything r yields.
func md5Of(r io.Reader) ([]byte, error) {
	h := md5.New()
	if _, err := io.Copy(h, r); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// xmlRequest returns a request carrying the XML document doc as its body,
// with Content-Type and Content-MD5 (which S3 requires for most
// configuration PUTs).
func xmlRequest(op, method, bucket string, query url.Values, doc any) (request, error) {
	body, err := xml.Marshal(doc)
	if err != nil {
		return request{}, fmt.Errorf("s3client: %s: %w", op, err)
	}
	return request{
		op: op, method: method, bucket: bucket, query: query,
		header: http.Header{"Content-Type": {"application/xml"}},
		body:   bytesBody(body), bodySize: int64(len(body)), md5: true,
	}, nil
}

// versionIDOf returns the x-amz-version-id of a response: a header, or a
// trailer when the service sent it after a keep-alive body (gos3 does for
// long CompleteMultipartUpload and CopyObject requests). Trailers are only
// known once the body was read to its end.
func versionIDOf(resp *http.Response) string {
	if v := resp.Header.Get("X-Amz-Version-Id"); v != "" {
		return v
	}
	return resp.Trailer.Get("X-Amz-Version-Id")
}

// objectQuery returns the query of an object request: the sub-resource
// sub ("" = none, e.g. "tagging") and versionId when versionID is not "".
func objectQuery(sub, versionID string) url.Values {
	q := url.Values{}
	if sub != "" {
		q.Set(sub, "")
	}
	if versionID != "" {
		q.Set("versionId", versionID)
	}
	return q
}

// parseTime parses an ISO 8601 timestamp of a listing or document (zero
// when it is missing or malformed).
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// quoteETag returns the ETag in its quoted wire form.
func quoteETag(etag string) string {
	if etag == "*" || strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, `W/"`) {
		return etag
	}
	return `"` + etag + `"`
}

// unquoteETag strips quotes (and a weak prefix) from a wire ETag.
func unquoteETag(etag string) string {
	etag = strings.TrimPrefix(strings.TrimSpace(etag), "W/")
	return strings.Trim(etag, `"`)
}
