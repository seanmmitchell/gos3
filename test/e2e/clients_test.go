package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// ctx returns a context bounded by the test's deadline (or 5 minutes).
func ctx(t testing.TB) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return c
}

// s3Client returns a path-style aws-sdk-go-v2 client acting as creds.
// Extra option functions customise it (checksum modes, ...).
func (e *env) s3Client(creds auth.Credentials, opts ...func(*s3.Options)) *s3.Client {
	o := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(creds.AccessKey, creds.SecretKey, ""),
		BaseEndpoint: aws.String(e.base),
		UsePathStyle: true,
		HTTPClient:   e.httpc,
		Retryer:      aws.NopRetryer{},
		// Multipart objects have composite checksums the SDK cannot
		// validate; it would only log a warning.
		DisableLogOutputChecksumValidationSkipped: true,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return s3.New(o)
}

// root returns a path-style SDK client acting as root.
func (e *env) root(opts ...func(*s3.Options)) *s3.Client { return e.s3Client(rootCreds, opts...) }

// vhost returns a virtual-hosted-style SDK client (Host <bucket>.s3.test)
// acting as creds. Only fsEnv has the s3.test domain configured.
func (e *env) vhost(creds auth.Credentials) *s3.Client {
	_, port, _ := strings.Cut(e.addr, ":")
	scheme := "http"
	if e.tls {
		scheme = "https"
	}
	return e.s3Client(creds, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(scheme + "://" + vhostZone + ":" + port)
		o.UsePathStyle = false
	})
}

// minioClient returns a minio-go client acting as creds.
func (e *env) minioClient(t testing.TB, creds auth.Credentials, trailing bool) *minio.Client {
	t.Helper()
	tr := e.httpc.Transport
	c, err := minio.New(e.addr, &minio.Options{
		Creds:           miniocreds.NewStaticV4(creds.AccessKey, creds.SecretKey, ""),
		Secure:          e.tls,
		Region:          region,
		Transport:       tr,
		BucketLookup:    minio.BucketLookupPath,
		TrailingHeaders: trailing,
		MaxRetries:      1,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	return c
}

var bucketSeq atomic.Int64

// bucketName returns a unique, valid bucket name derived from the test name.
func bucketName(t testing.TB) string {
	t.Helper()
	var b strings.Builder
	for _, r := range strings.ToLower(t.Name()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 40 {
		name = strings.Trim(name[len(name)-40:], "-")
	}
	return fmt.Sprintf("%s-%d", name, bucketSeq.Add(1))
}

// newBucket creates a fresh bucket as root and returns its name.
func (e *env) newBucket(t testing.TB) string {
	t.Helper()
	name := bucketName(t)
	if _, err := e.root().CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("CreateBucket %s: %v", name, err)
	}
	return name
}

// newVersionedBucket creates a bucket with versioning enabled.
func (e *env) newVersionedBucket(t testing.TB) string {
	t.Helper()
	name := e.newBucket(t)
	e.setVersioning(t, name, "Enabled")
	return name
}

func (e *env) setVersioning(t testing.TB, bucket, status string) {
	t.Helper()
	_, err := e.root().PutBucketVersioning(ctx(t), &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatus(status)},
	})
	if err != nil {
		t.Fatalf("PutBucketVersioning %s: %v", status, err)
	}
}

// put stores an object as root and returns its output.
func (e *env) put(t testing.TB, bucket, key string, data []byte, opts ...func(*s3.PutObjectInput)) *s3.PutObjectOutput {
	t.Helper()
	in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}
	for _, fn := range opts {
		fn(in)
	}
	out, err := e.root().PutObject(ctx(t), in)
	if err != nil {
		t.Fatalf("PutObject %s/%s: %v", bucket, key, err)
	}
	return out
}

// get reads an object as root and returns its body.
func (e *env) get(t testing.TB, bucket, key string) []byte {
	t.Helper()
	out, err := e.root().GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading %s/%s: %v", bucket, key, err)
	}
	return data
}

// apiCode returns the S3 error code of err ("" if it is not an API error).
func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// httpStatus returns the HTTP status of a failed SDK call (0 if none).
func httpStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// wantErr asserts that err is an S3 error with the given code and status
// (status 0 skips the status check).
func wantErr(t testing.TB, err error, code string, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %s (%d), got success", code, status)
	}
	if got := apiCode(err); got != code {
		t.Fatalf("expected error code %q, got %q (%v)", code, got, err)
	}
	if status != 0 {
		if got := httpStatus(err); got != status {
			t.Fatalf("expected HTTP %d for %s, got %d", status, code, got)
		}
	}
}

// must fails the test on err.
func must(t testing.TB, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// response is a raw HTTP response with its body read.
type response struct {
	status int
	header http.Header
	body   []byte
}

// code returns the S3 XML error code in the body ("" if none).
func (r response) code() string {
	var doc struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(r.body, &doc) != nil {
		return ""
	}
	return doc.Code
}

// rawRequest describes one raw HTTP request.
type rawRequest struct {
	method string
	path   string // path (+ query) relative to the server, or an absolute URL
	host   string // optional Host override (virtual-host requests)
	body   []byte
	header map[string]string
	creds  *auth.Credentials // nil = anonymous
	// payloadHash overrides the signed x-amz-content-sha256 ("" = hex
	// SHA-256 of body).
	payloadHash string
}

// do sends a raw (optionally SigV4-signed) request.
func (e *env) do(t testing.TB, rr rawRequest) response {
	t.Helper()
	return e.doAt(t, rr, time.Now())
}

// doAt is do with the signing time given.
func (e *env) doAt(t testing.TB, rr rawRequest, at time.Time) response {
	t.Helper()
	u := rr.path
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = e.base + rr.path
	}
	var body io.Reader
	if rr.body != nil {
		body = bytes.NewReader(rr.body)
	}
	req, err := http.NewRequestWithContext(ctx(t), rr.method, u, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if rr.host != "" {
		req.Host = rr.host
	}
	for k, v := range rr.header {
		req.Header.Set(k, v)
	}
	if rr.creds != nil {
		ph := rr.payloadHash
		if ph == "" {
			sum := sha256.Sum256(rr.body)
			ph = hex.EncodeToString(sum[:])
		}
		if err := auth.SignRequest(req, *rr.creds, region, "s3", ph, at); err != nil {
			t.Fatalf("SignRequest: %v", err)
		}
	}
	resp, err := e.httpc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", rr.method, u, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response of %s %s: %v", rr.method, u, err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

// anon sends an anonymous raw request.
func (e *env) anon(t testing.TB, method, path string, body []byte, hdr map[string]string) response {
	t.Helper()
	return e.do(t, rawRequest{method: method, path: path, body: body, header: hdr})
}

// signed sends a raw request signed as creds.
func (e *env) signed(t testing.TB, creds auth.Credentials, method, path string, body []byte, hdr map[string]string) response {
	t.Helper()
	return e.do(t, rawRequest{method: method, path: path, body: body, header: hdr, creds: &creds})
}

// adminPrefix is the admin API base path.
const adminPrefix = "/_gos3/admin/v1"

// admin calls the admin API signed as creds with a JSON body (nil = none)
// and decodes a JSON response into out (if non-nil and the call succeeded).
func (e *env) admin(t testing.TB, creds auth.Credentials, method, path string, in, out any) response {
	t.Helper()
	var body []byte
	hdr := map[string]string{}
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			t.Fatalf("encoding admin request: %v", err)
		}
		hdr["Content-Type"] = "application/json"
	}
	resp := e.signed(t, creds, method, adminPrefix+path, body, hdr)
	if out != nil && resp.status >= 200 && resp.status < 300 {
		if err := json.Unmarshal(resp.body, out); err != nil {
			t.Fatalf("decoding admin response %q: %v", resp.body, err)
		}
	}
	return resp
}

// adminUser is the subset of iam.User / AccessKey the tests need.
type adminKey struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	Status    string `json:"status"`
}

// createUser creates an IAM user with policies through the admin API and
// returns credentials of a new access key.
func (e *env) createUser(t testing.TB, name string, policies ...string) auth.Credentials {
	t.Helper()
	if policies == nil {
		policies = []string{}
	}
	var out struct {
		Key *adminKey `json:"key"`
	}
	resp := e.admin(t, rootCreds, http.MethodPost, "/users",
		map[string]any{"name": name, "policies": policies, "createKey": true}, &out)
	if resp.status != http.StatusCreated {
		t.Fatalf("POST /users %s: %d %s", name, resp.status, resp.body)
	}
	if out.Key == nil || out.Key.AccessKey == "" || out.Key.SecretKey == "" {
		t.Fatalf("POST /users %s: no key in %s", name, resp.body)
	}
	return auth.Credentials{AccessKey: out.Key.AccessKey, SecretKey: out.Key.SecretKey}
}

// putPolicy creates or replaces a custom IAM policy through the admin API.
func (e *env) putPolicy(t testing.TB, name, doc string) {
	t.Helper()
	resp := e.admin(t, rootCreds, http.MethodPut, "/policies/"+url.PathEscape(name),
		map[string]any{"description": "e2e", "document": json.RawMessage(doc)}, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("PUT /policies/%s: %d %s", name, resp.status, resp.body)
	}
}

var userSeq atomic.Int64

// userName returns a unique IAM user name.
func userName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, userSeq.Add(1))
}

// randomData returns n pseudo-random bytes (deterministic per seed).
func randomData(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

// readAll reads and closes body.
func readAll(t testing.TB, body io.ReadCloser) []byte {
	t.Helper()
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return data
}

// stringsReader returns a seekable reader of s.
func stringsReader(s string) *strings.Reader { return strings.NewReader(s) }

// sentRequest is what a recordingTransport saw a client send.
type sentRequest struct {
	method, path, query string
	header              http.Header
}

// recordingTransport records the requests passing through it.
type recordingTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	reqs []sentRequest
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, sentRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
	rt.mu.Unlock()
	return rt.base.RoundTrip(r)
}

// last returns the last recorded request with the given method.
func (rt *recordingTransport) last(method string) sentRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for i := len(rt.reqs) - 1; i >= 0; i-- {
		if rt.reqs[i].method == method {
			return rt.reqs[i]
		}
	}
	return sentRequest{header: http.Header{}}
}

// recordingClient returns an http.Client like e.httpc that records requests.
func (e *env) recordingClient() (*http.Client, *recordingTransport) {
	rt := &recordingTransport{base: e.httpc.Transport}
	return &http.Client{Transport: rt, Timeout: e.httpc.Timeout, CheckRedirect: e.httpc.CheckRedirect}, rt
}
