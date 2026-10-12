package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- helpers ---

// mpSetMinPartSize lowers the minimum part size for one test.
func mpSetMinPartSize(t *testing.T, n int64) {
	t.Helper()
	old := multipartMinPartSize.Swap(n)
	t.Cleanup(func() { multipartMinPartSize.Store(old) })
}

// mpSetKeepAlive changes the keep-alive timing for one test.
func mpSetKeepAlive(t *testing.T, delay, interval time.Duration) {
	t.Helper()
	oldD := keepAliveDelay.Swap(int64(delay))
	oldI := keepAliveInterval.Swap(int64(interval))
	t.Cleanup(func() {
		keepAliveDelay.Store(oldD)
		keepAliveInterval.Store(oldI)
	})
}

// mpData returns n deterministic pseudo-random bytes.
func mpData(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// mpCode returns the S3 error code of an SDK error ("" if none).
func mpCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// mpExpectCode fails unless err carries the S3 error code want.
func mpExpectCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got success", want)
	}
	if got := mpCode(err); got != want {
		t.Fatalf("expected %s, got %q (%v)", want, got, err)
	}
}

// mpCreate starts an upload through the SDK and returns its ID.
func mpCreate(t *testing.T, c *awss3.Client, bucket, key string, mod func(*awss3.CreateMultipartUploadInput)) string {
	t.Helper()
	in := &awss3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if mod != nil {
		mod(in)
	}
	out, err := c.CreateMultipartUpload(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if aws.ToString(out.UploadId) == "" || aws.ToString(out.Bucket) != bucket || aws.ToString(out.Key) != key {
		t.Fatalf("CreateMultipartUpload output = %+v", out)
	}
	return aws.ToString(out.UploadId)
}

// mpUploadPart uploads one part through the SDK.
func mpUploadPart(c *awss3.Client, bucket, key, id string, n int32, data []byte, mod func(*awss3.UploadPartInput)) (*awss3.UploadPartOutput, error) {
	in := &awss3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
		PartNumber: aws.Int32(n), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data))),
	}
	if mod != nil {
		mod(in)
	}
	return c.UploadPart(context.Background(), in)
}

// mpMustUploadPart is mpUploadPart failing the test on error.
func mpMustUploadPart(t *testing.T, c *awss3.Client, bucket, key, id string, n int32, data []byte, mod func(*awss3.UploadPartInput)) *awss3.UploadPartOutput {
	t.Helper()
	out, err := mpUploadPart(c, bucket, key, id, n, data, mod)
	if err != nil {
		t.Fatalf("UploadPart %d: %v", n, err)
	}
	return out
}

// mpComplete completes an upload through the SDK.
func mpComplete(c *awss3.Client, bucket, key, id string, parts []types.CompletedPart, mod func(*awss3.CompleteMultipartUploadInput)) (*awss3.CompleteMultipartUploadOutput, error) {
	in := &awss3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}
	if mod != nil {
		mod(in)
	}
	return c.CompleteMultipartUpload(context.Background(), in)
}

// mpPart builds a CompletedPart.
func mpPart(n int32, etag string) types.CompletedPart {
	return types.CompletedPart{PartNumber: aws.Int32(n), ETag: aws.String(etag)}
}

// mpExpectedETag computes the multipart ETag of parts independently.
func mpExpectedETag(parts ...[]byte) string {
	h := md5.New()
	for _, p := range parts {
		s := md5.Sum(p)
		h.Write(s[:])
	}
	return fmt.Sprintf("\"%s-%d\"", hex.EncodeToString(h.Sum(nil)), len(parts))
}

// mpGet reads a whole object through the SDK.
func mpGet(t *testing.T, c *awss3.Client, bucket, key string, mod func(*awss3.GetObjectInput)) (*awss3.GetObjectOutput, []byte) {
	t.Helper()
	in := &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if mod != nil {
		mod(in)
	}
	out, err := c.GetObject(context.Background(), in)
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("GetObject body: %v", err)
	}
	return out, data
}

// mpPath builds "/bucket/key?query".
func mpPath(bucket, key string, q url.Values) string {
	p := "/" + bucket + "/" + key
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return p
}

// mpQuery builds query values from pairs.
func mpQuery(kv ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	return q
}

// mpCompleteBody renders a CompleteMultipartUpload document from
// (part number, ETag) pairs.
func mpCompleteBody(pairs ...string) []byte {
	var b strings.Builder
	b.WriteString(`<CompleteMultipartUpload xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, "<Part><PartNumber>%s</PartNumber><ETag>%s</ETag></Part>", pairs[i], pairs[i+1])
	}
	b.WriteString("</CompleteMultipartUpload>")
	return []byte(b.String())
}

// mpRawCreate starts an upload with a raw signed request.
func mpRawCreate(t *testing.T, e *testEnv, bucket, key string, hdr http.Header) string {
	t.Helper()
	resp := e.signed(http.MethodPost, mpPath(bucket, key, nil)+"?uploads", nil, hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CreateMultipartUpload: %d %s", resp.StatusCode, resp.Data)
	}
	var res struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(resp.Data, &res); err != nil || res.UploadID == "" {
		t.Fatalf("CreateMultipartUpload body %s: %v", resp.Data, err)
	}
	return res.UploadID
}

// --- full flows ---

func TestMultipartSDKFlow(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("mp")
	c := e.client
	ctx := context.Background()

	// Real S3 minimum: 5 MiB parts except the last.
	p1, p2, p3 := mpData(1, 5<<20), mpData(2, 5<<20+17), mpData(3, 1234)
	id := mpCreate(t, c, "mp", "dir/big object", func(in *awss3.CreateMultipartUploadInput) {
		in.ContentType = aws.String("video/mp4")
		in.CacheControl = aws.String("max-age=60")
		in.Metadata = map[string]string{"color": "blue"}
		in.Tagging = aws.String("a=1&b=2")
		in.StorageClass = types.StorageClassStandardIa
	})
	// Out of order upload.
	o3 := mpMustUploadPart(t, c, "mp", "dir/big object", id, 3, p3, nil)
	o1 := mpMustUploadPart(t, c, "mp", "dir/big object", id, 1, p1, nil)
	o2 := mpMustUploadPart(t, c, "mp", "dir/big object", id, 2, p2, nil)
	for i, o := range []*awss3.UploadPartOutput{o1, o2, o3} {
		want := "\"" + md5Hex([][]byte{p1, p2, p3}[i]) + "\""
		if aws.ToString(o.ETag) != want {
			t.Fatalf("part %d ETag = %s, want %s", i+1, aws.ToString(o.ETag), want)
		}
	}

	out, err := mpComplete(c, "mp", "dir/big object", id, []types.CompletedPart{
		mpPart(1, aws.ToString(o1.ETag)), mpPart(2, aws.ToString(o2.ETag)), mpPart(3, aws.ToString(o3.ETag)),
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, want := aws.ToString(out.ETag), mpExpectedETag(p1, p2, p3); got != want {
		t.Fatalf("ETag = %s, want %s", got, want)
	}
	if aws.ToString(out.Bucket) != "mp" || aws.ToString(out.Key) != "dir/big object" {
		t.Fatalf("Complete output = %+v", out)
	}
	if loc := aws.ToString(out.Location); loc != e.srv.URL+"/mp/dir/big%20object" {
		t.Fatalf("Location = %q", loc)
	}
	if out.VersionId != nil {
		t.Fatalf("unversioned bucket returned VersionId %q", aws.ToString(out.VersionId))
	}
	// Default checksum: FULL_OBJECT CRC64NVME of the whole object.
	all := bytes.Join([][]byte{p1, p2, p3}, nil)
	if got, want := aws.ToString(out.ChecksumCRC64NVME), computeChecksum(ChecksumCRC64NVME, all); got != want || out.ChecksumType != types.ChecksumTypeFullObject {
		t.Fatalf("checksum = %s %s, want %s FULL_OBJECT", got, out.ChecksumType, want)
	}

	// The SDK validates the FULL_OBJECT checksum while reading.
	g, data := mpGet(t, c, "mp", "dir/big object", func(in *awss3.GetObjectInput) { in.ChecksumMode = types.ChecksumModeEnabled })
	if !bytes.Equal(data, all) {
		t.Fatal("object content differs")
	}
	if aws.ToString(g.ContentType) != "video/mp4" || aws.ToString(g.CacheControl) != "max-age=60" ||
		g.Metadata["color"] != "blue" || aws.ToInt32(g.TagCount) != 2 || g.StorageClass != types.StorageClassStandardIa {
		t.Fatalf("GetObject headers = %+v", g)
	}
	if aws.ToInt32(g.PartsCount) != 3 {
		t.Fatalf("PartsCount = %d", aws.ToInt32(g.PartsCount))
	}

	// GetObject?partNumber returns exactly that part.
	g2, part2 := mpGet(t, c, "mp", "dir/big object", func(in *awss3.GetObjectInput) {
		in.PartNumber = aws.Int32(2)
		in.ChecksumMode = types.ChecksumModeEnabled
	})
	if !bytes.Equal(part2, p2) {
		t.Fatalf("part 2 content differs (%d bytes)", len(part2))
	}
	if cr := aws.ToString(g2.ContentRange); cr != fmt.Sprintf("bytes %d-%d/%d", len(p1), len(p1)+len(p2)-1, len(all)) {
		t.Fatalf("part 2 Content-Range = %q", cr)
	}

	// The upload is gone.
	if _, err := c.ListParts(ctx, &awss3.ListPartsInput{Bucket: aws.String("mp"), Key: aws.String("dir/big object"), UploadId: aws.String(id)}); mpCode(err) != "NoSuchUpload" {
		t.Fatalf("ListParts after complete: %v", err)
	}
	info, err := e.backend.GetObjectInfo(ctx, "mp", "dir/big object", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.PartSizes) != 3 || info.PartSizes[1] != int64(len(p2)) || len(info.PartChecksums) != 3 ||
		info.PartChecksums[2][ChecksumCRC64NVME] != computeChecksum(ChecksumCRC64NVME, p3) {
		t.Fatalf("stored part info = %v %v", info.PartSizes, info.PartChecksums)
	}
	if info.Tags["a"] != "1" || info.Tags["b"] != "2" {
		t.Fatalf("tags = %v", info.Tags)
	}
}

func TestMultipartPartReplacementAndErrors(t *testing.T) {
	mpSetMinPartSize(t, 1024)
	e := newTestEnv(t)
	e.createBucket("mp")
	c := e.client
	a, b := mpData(10, 2048), mpData(11, 2048)
	last := []byte("tail")
	id := mpCreate(t, c, "mp", "k", nil)
	oa := mpMustUploadPart(t, c, "mp", "k", id, 1, a, nil)
	ob := mpMustUploadPart(t, c, "mp", "k", id, 1, b, nil) // replaces part 1
	ol := mpMustUploadPart(t, c, "mp", "k", id, 2, last, nil)
	small := mpMustUploadPart(t, c, "mp", "k", id, 3, []byte("tiny"), nil)

	tests := []struct {
		name  string
		parts []types.CompletedPart
		code  string
	}{
		{"replaced part's old ETag", []types.CompletedPart{mpPart(1, aws.ToString(oa.ETag)), mpPart(2, aws.ToString(ol.ETag))}, "InvalidPart"},
		{"wrong ETag", []types.CompletedPart{mpPart(1, `"00000000000000000000000000000000"`), mpPart(2, aws.ToString(ol.ETag))}, "InvalidPart"},
		{"garbage ETag", []types.CompletedPart{mpPart(1, "nope"), mpPart(2, aws.ToString(ol.ETag))}, "InvalidPart"},
		{"missing part", []types.CompletedPart{mpPart(1, aws.ToString(ob.ETag)), mpPart(4, aws.ToString(ol.ETag))}, "InvalidPart"},
		{"descending", []types.CompletedPart{mpPart(2, aws.ToString(ol.ETag)), mpPart(1, aws.ToString(ob.ETag))}, "InvalidPartOrder"},
		{"duplicate", []types.CompletedPart{mpPart(1, aws.ToString(ob.ETag)), mpPart(1, aws.ToString(ob.ETag))}, "InvalidPartOrder"},
		{"small middle part", []types.CompletedPart{mpPart(1, aws.ToString(ob.ETag)), mpPart(2, aws.ToString(ol.ETag)), mpPart(3, aws.ToString(small.ETag))}, "EntityTooSmall"},
		{"part number out of range", []types.CompletedPart{mpPart(10001, aws.ToString(ob.ETag))}, "InvalidArgument"},
		{"no parts", nil, "MalformedXML"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mpComplete(c, "mp", "k", id, tc.parts, nil)
			mpExpectCode(t, err, tc.code)
		})
	}

	// Unquoted ETags are accepted; the replacement's content wins.
	out, err := mpComplete(c, "mp", "k", id, []types.CompletedPart{
		mpPart(1, strings.Trim(aws.ToString(ob.ETag), `"`)), mpPart(2, aws.ToString(ol.ETag)),
	}, nil)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if aws.ToString(out.ETag) != mpExpectedETag(b, last) {
		t.Fatalf("ETag = %s", aws.ToString(out.ETag))
	}
	_, data := mpGet(t, c, "mp", "k", nil)
	if !bytes.Equal(data, append(append([]byte{}, b...), last...)) {
		t.Fatal("content differs")
	}
	// Part 3 (not listed) was discarded with the upload.
	_, err = mpComplete(c, "mp", "k", id, []types.CompletedPart{mpPart(3, aws.ToString(small.ETag))}, nil)
	mpExpectCode(t, err, "NoSuchUpload")
}

func TestMultipartEntityTooSmallDetails(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("mp")
	id := mpRawCreate(t, e, "mp", "k", nil)
	q := mpQuery("uploadId", id, "partNumber", "1")
	r1 := e.signed(http.MethodPut, mpPath("mp", "k", q), []byte("small"), nil)
	q.Set("partNumber", "2")
	r2 := e.signed(http.MethodPut, mpPath("mp", "k", q), []byte("last"), nil)
	if r1.StatusCode != 200 || r2.StatusCode != 200 {
		t.Fatalf("UploadPart: %d %d", r1.StatusCode, r2.StatusCode)
	}
	resp := e.signed(http.MethodPost, mpPath("mp", "k", mpQuery("uploadId", id)),
		mpCompleteBody("1", r1.Header.Get("ETag"), "2", r2.Header.Get("ETag")), nil)
	if resp.StatusCode != http.StatusBadRequest || resp.code() != "EntityTooSmall" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Data)
	}
	for _, want := range []string{"<ProposedSize>5</ProposedSize>", "<MinSizeAllowed>5242880</MinSizeAllowed>", "<PartNumber>1</PartNumber>"} {
		if !bytes.Contains(resp.Data, []byte(want)) {
			t.Fatalf("error lacks %s: %s", want, resp.Data)
		}
	}
	// A single small part is fine (it is the last one).
	resp = e.signed(http.MethodPost, mpPath("mp", "k", mpQuery("uploadId", id)), mpCompleteBody("2", r2.Header.Get("ETag")), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single small part: %d %s", resp.StatusCode, resp.Data)
	}
}

func TestMultipartAbort(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("mp")
	c := e.client
	ctx := context.Background()
	id := mpCreate(t, c, "mp", "k", nil)
	o := mpMustUploadPart(t, c, "mp", "k", id, 1, []byte("data"), nil)

	resp := e.signed(http.MethodDelete, mpPath("mp", "k", mpQuery("uploadId", id)), nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("abort: %d %s", resp.StatusCode, resp.Data)
	}
	_, err := mpComplete(c, "mp", "k", id, []types.CompletedPart{mpPart(1, aws.ToString(o.ETag))}, nil)
	mpExpectCode(t, err, "NoSuchUpload")
	_, err = mpUploadPart(c, "mp", "k", id, 2, []byte("x"), nil)
	mpExpectCode(t, err, "NoSuchUpload")
	_, err = c.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{Bucket: aws.String("mp"), Key: aws.String("k"), UploadId: aws.String(id)})
	mpExpectCode(t, err, "NoSuchUpload")

	// The raw error names the upload.
	resp = e.signed(http.MethodDelete, mpPath("mp", "k", mpQuery("uploadId", id)), nil, nil)
	if resp.StatusCode != http.StatusNotFound || !bytes.Contains(resp.Data, []byte("<UploadId>"+id+"</UploadId>")) {
		t.Fatalf("second abort: %d %s", resp.StatusCode, resp.Data)
	}
	// Wrong key for an existing upload.
	id2 := mpCreate(t, c, "mp", "k2", nil)
	resp = e.signed(http.MethodDelete, mpPath("mp", "other", mpQuery("uploadId", id2)), nil, nil)
	if resp.code() != "NoSuchUpload" {
		t.Fatalf("abort with wrong key: %d %s", resp.StatusCode, resp.Data)
	}
	// Missing bucket.
	resp = e.signed(http.MethodDelete, mpPath("nobucket", "k", mpQuery("uploadId", id2)), nil, nil)
	if resp.code() != "NoSuchBucket" {
		t.Fatalf("abort in missing bucket: %d %s", resp.StatusCode, resp.Data)
	}
}

func TestMultipartRequestValidation(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("mp")
	id := mpRawCreate(t, e, "mp", "k", nil)
	tests := []struct {
		name, method string
		q            url.Values
		body         []byte
		hdr          http.Header
		status       int
		code         string
	}{
		{"part number zero", "PUT", mpQuery("uploadId", id, "partNumber", "0"), []byte("x"), nil, 400, "InvalidArgument"},
		{"part number too big", "PUT", mpQuery("uploadId", id, "partNumber", "10001"), []byte("x"), nil, 400, "InvalidArgument"},
		{"part number not a number", "PUT", mpQuery("uploadId", id, "partNumber", "one"), []byte("x"), nil, 400, "InvalidArgument"},
		{"part number missing", "PUT", mpQuery("uploadId", id), []byte("x"), nil, 400, "InvalidArgument"},
		{"empty upload id", "PUT", mpQuery("uploadId", "", "partNumber", "1"), []byte("x"), nil, 404, "NoSuchUpload"},
		{"unknown upload id", "PUT", mpQuery("uploadId", "nope", "partNumber", "1"), []byte("x"), nil, 404, "NoSuchUpload"},
		{"bad content md5", "PUT", mpQuery("uploadId", id, "partNumber", "1"), []byte("x"),
			http.Header{"Content-Md5": {"1B2M2Y8AsgTpgAmY7PhCfg=="}}, 400, "BadDigest"},
		{"malformed content md5", "PUT", mpQuery("uploadId", id, "partNumber", "1"), []byte("x"),
			http.Header{"Content-Md5": {"zzz"}}, 400, "InvalidDigest"},
		{"bad checksum", "PUT", mpQuery("uploadId", id, "partNumber", "1"), []byte("x"),
			http.Header{"X-Amz-Checksum-Crc32": {"AAAAAA=="}}, 400, "BadDigest"},
		{"sse-c", "PUT", mpQuery("uploadId", id, "partNumber", "1"), []byte("x"),
			http.Header{"X-Amz-Server-Side-Encryption-Customer-Algorithm": {"AES256"}}, 501, "NotImplemented"},
		{"complete empty body", "POST", mpQuery("uploadId", id), nil, nil, 400, "MalformedXML"},
		{"complete invalid xml", "POST", mpQuery("uploadId", id), []byte("<CompleteMultipartUpload><Part>"), nil, 400, "MalformedXML"},
		{"complete wrong root", "POST", mpQuery("uploadId", id), []byte("<Foo/>"), nil, 400, "MalformedXML"},
		{"complete bad part number", "POST", mpQuery("uploadId", id), mpCompleteBody("x", `"a"`), nil, 400, "MalformedXML"},
		{"complete unknown upload", "POST", mpQuery("uploadId", "nope"), mpCompleteBody("1", `"a"`), nil, 404, "NoSuchUpload"},
		{"complete if-none-match etag", "POST", mpQuery("uploadId", id), mpCompleteBody("1", `"a"`), http.Header{"If-None-Match": {`"abc"`}}, 501, "NotImplemented"},
		{"list parts bad max", "GET", mpQuery("uploadId", id, "max-parts", "x"), nil, nil, 400, "InvalidArgument"},
		{"list parts negative max", "GET", mpQuery("uploadId", id, "max-parts", "-1"), nil, nil, 400, "InvalidArgument"},
		{"list parts bad marker", "GET", mpQuery("uploadId", id, "part-number-marker", "x"), nil, nil, 400, "InvalidArgument"},
		{"list parts unknown upload", "GET", mpQuery("uploadId", "nope"), nil, nil, 404, "NoSuchUpload"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.signed(tc.method, mpPath("mp", "k", tc.q), tc.body, tc.hdr)
			if resp.StatusCode != tc.status || resp.code() != tc.code {
				t.Fatalf("got %d %q, want %d %s: %s", resp.StatusCode, resp.code(), tc.status, tc.code, resp.Data)
			}
		})
	}

	bucketTests := []struct {
		name string
		q    url.Values
		code string
	}{
		{"bad max-uploads", mpQuery("uploads", "", "max-uploads", "abc"), "InvalidArgument"},
		{"bad encoding", mpQuery("uploads", "", "encoding-type", "base64"), "InvalidArgument"},
	}
	for _, tc := range bucketTests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.signed(http.MethodGet, "/mp?"+tc.q.Encode(), nil, nil)
			if resp.StatusCode != 400 || resp.code() != tc.code {
				t.Fatalf("got %d %s", resp.StatusCode, resp.Data)
			}
		})
	}
	if resp := e.signed(http.MethodGet, "/nobucket?uploads", nil, nil); resp.code() != "NoSuchBucket" {
		t.Fatalf("missing bucket: %d %s", resp.StatusCode, resp.Data)
	}
	if resp := e.signed(http.MethodPost, "/nobucket/k?uploads", nil, nil); resp.code() != "NoSuchBucket" {
		t.Fatalf("create in missing bucket: %d %s", resp.StatusCode, resp.Data)
	}
}

func TestCreateMultipartUploadHeaders(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("mp")
	ctx := context.Background()

	tests := []struct {
		name   string
		hdr    http.Header
		status int
		code   string
		algo   string
		typ    string
	}{
		{"no checksum", nil, 200, "", "", ""},
		{"sha256 default composite", http.Header{"X-Amz-Checksum-Algorithm": {"SHA256"}}, 200, "", "SHA256", "COMPOSITE"},
		{"crc32 lowercase", http.Header{"X-Amz-Checksum-Algorithm": {"crc32"}}, 200, "", "CRC32", "COMPOSITE"},
		{"crc32c full object", http.Header{"X-Amz-Checksum-Algorithm": {"CRC32C"}, "X-Amz-Checksum-Type": {"FULL_OBJECT"}}, 200, "", "CRC32C", "FULL_OBJECT"},
		{"crc64nvme default full object", http.Header{"X-Amz-Checksum-Algorithm": {"CRC64NVME"}}, 200, "", "CRC64NVME", "FULL_OBJECT"},
		{"crc64nvme composite", http.Header{"X-Amz-Checksum-Algorithm": {"CRC64NVME"}, "X-Amz-Checksum-Type": {"COMPOSITE"}}, 400, "InvalidRequest", "", ""},
		{"sha1 full object", http.Header{"X-Amz-Checksum-Algorithm": {"SHA1"}, "X-Amz-Checksum-Type": {"FULL_OBJECT"}}, 400, "InvalidRequest", "", ""},
		{"type without algorithm", http.Header{"X-Amz-Checksum-Type": {"COMPOSITE"}}, 400, "InvalidRequest", "", ""},
		{"unknown algorithm", http.Header{"X-Amz-Checksum-Algorithm": {"MD4"}}, 400, "InvalidRequest", "", ""},
		{"unknown type", http.Header{"X-Amz-Checksum-Algorithm": {"CRC32"}, "X-Amz-Checksum-Type": {"PARTIAL"}}, 400, "InvalidRequest", "", ""},
		{"bad storage class", http.Header{"X-Amz-Storage-Class": {"FAST"}}, 400, "InvalidStorageClass", "", ""},
		{"bad acl", http.Header{"X-Amz-Acl": {"everyone"}}, 400, "InvalidArgument", "", ""},
		{"bad tagging", http.Header{"X-Amz-Tagging": {"a=1&a=2"}}, 400, "InvalidArgument", "", ""},
		{"object lock without lock bucket", http.Header{"X-Amz-Object-Lock-Legal-Hold": {"ON"}}, 400, "InvalidRequest", "", ""},
		{"metadata too large", http.Header{"X-Amz-Meta-Big": {strings.Repeat("x", 3000)}}, 400, "MetadataTooLarge", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.signed(http.MethodPost, "/mp/obj?uploads", nil, tc.hdr)
			if resp.StatusCode != tc.status || resp.code() != tc.code {
				t.Fatalf("got %d %q: %s", resp.StatusCode, resp.code(), resp.Data)
			}
			if tc.status != 200 {
				return
			}
			if got := resp.Header.Get("X-Amz-Checksum-Algorithm"); got != tc.algo {
				t.Fatalf("x-amz-checksum-algorithm = %q, want %q", got, tc.algo)
			}
			if got := resp.Header.Get("X-Amz-Checksum-Type"); got != tc.typ {
				t.Fatalf("x-amz-checksum-type = %q, want %q", got, tc.typ)
			}
			body := string(resp.Data)
			if !strings.HasPrefix(body, xmlHeader) ||
				!strings.Contains(body, `<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>mp</Bucket><Key>obj</Key><UploadId>`) {
				t.Fatalf("body = %s", body)
			}
		})
	}

	// ACL, SSE and website redirect are carried to the final object.
	id := mpRawCreate(t, e, "mp", "acl", http.Header{
		"X-Amz-Acl":                       {"public-read"},
		"X-Amz-Server-Side-Encryption":    {"AES256"},
		"X-Amz-Website-Redirect-Location": {"/other"},
		"Content-Disposition":             {"attachment"},
	})
	q := mpQuery("uploadId", id, "partNumber", "1")
	pr := e.signed(http.MethodPut, mpPath("mp", "acl", q), []byte("hello"), nil)
	if pr.StatusCode != 200 || pr.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Fatalf("UploadPart: %d %v", pr.StatusCode, pr.Header)
	}
	resp := e.signed(http.MethodPost, mpPath("mp", "acl", mpQuery("uploadId", id)), mpCompleteBody("1", pr.Header.Get("ETag")), nil)
	if resp.StatusCode != 200 || resp.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Fatalf("Complete: %d %v %s", resp.StatusCode, resp.Header, resp.Data)
	}
	info, err := e.backend.GetObjectInfo(ctx, "mp", "acl", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ACL != aclPublicRead || info.SSE != "AES256" || info.WebsiteRedirect != "/other" || info.ContentDisposition != "attachment" {
		t.Fatalf("stored meta = %+v", info.ObjectMeta)
	}
}

func TestMultipartObjectLock(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if err := e.backend.CreateBucket(ctx, "locked", storage.CreateBucketOptions{ObjectLockEnabled: true}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	id := mpRawCreate(t, e, "locked", "k", http.Header{
		"X-Amz-Object-Lock-Mode":              {"GOVERNANCE"},
		"X-Amz-Object-Lock-Retain-Until-Date": {until.Format(time.RFC3339)},
		"X-Amz-Object-Lock-Legal-Hold":        {"ON"},
	})
	pr := e.signed(http.MethodPut, mpPath("locked", "k", mpQuery("uploadId", id, "partNumber", "1")), []byte("x"), nil)
	resp := e.signed(http.MethodPost, mpPath("locked", "k", mpQuery("uploadId", id)), mpCompleteBody("1", pr.Header.Get("ETag")), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("Complete: %d %s", resp.StatusCode, resp.Data)
	}
	if resp.Header.Get("X-Amz-Version-Id") == "" {
		t.Fatal("versioned (object lock) bucket must report x-amz-version-id")
	}
	info, err := e.backend.GetObjectInfo(ctx, "locked", "k", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.RetentionMode != "GOVERNANCE" || !info.RetainUntil.Equal(until) || !info.LegalHold {
		t.Fatalf("lock meta = %s %v %v", info.RetentionMode, info.RetainUntil, info.LegalHold)
	}
}

func TestMultipartVersionedAndConditional(t *testing.T) {
	mpSetMinPartSize(t, 1)
	e := newTestEnv(t)
	e.createBucket("mp")
	ctx := context.Background()
	if err := e.backend.SetBucketVersioning(ctx, "mp", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	c := e.client
	upload := func(key string, data []byte) (string, types.CompletedPart) {
		id := mpCreate(t, c, "mp", key, nil)
		o := mpMustUploadPart(t, c, "mp", key, id, 1, data, nil)
		return id, mpPart(1, aws.ToString(o.ETag))
	}

	// If-Match on a missing key: NoSuchKey.
	id, p := upload("k", []byte("one"))
	_, err := mpComplete(c, "mp", "k", id, []types.CompletedPart{p}, func(in *awss3.CompleteMultipartUploadInput) { in.IfMatch = aws.String(`"abc"`) })
	mpExpectCode(t, err, "NoSuchKey")
	// If-None-Match on a missing key succeeds.
	out, err := mpComplete(c, "mp", "k", id, []types.CompletedPart{p}, func(in *awss3.CompleteMultipartUploadInput) { in.IfNoneMatch = aws.String("*") })
	if err != nil {
		t.Fatalf("If-None-Match on new key: %v", err)
	}
	v1 := aws.ToString(out.VersionId)
	if v1 == "" || v1 == "null" {
		t.Fatalf("VersionId = %q", v1)
	}
	etag1 := aws.ToString(out.ETag)

	id, p = upload("k", []byte("two"))
	_, err = mpComplete(c, "mp", "k", id, []types.CompletedPart{p}, func(in *awss3.CompleteMultipartUploadInput) { in.IfNoneMatch = aws.String("*") })
	mpExpectCode(t, err, "PreconditionFailed")
	_, err = mpComplete(c, "mp", "k", id, []types.CompletedPart{p}, func(in *awss3.CompleteMultipartUploadInput) { in.IfMatch = aws.String(`"0123"`) })
	mpExpectCode(t, err, "PreconditionFailed")
	out, err = mpComplete(c, "mp", "k", id, []types.CompletedPart{p}, func(in *awss3.CompleteMultipartUploadInput) { in.IfMatch = aws.String(etag1) })
	if err != nil {
		t.Fatalf("If-Match with current ETag: %v", err)
	}
	if v2 := aws.ToString(out.VersionId); v2 == "" || v2 == v1 {
		t.Fatalf("second version = %q (first %q)", v2, v1)
	}
	// Both versions exist.
	_, d1 := mpGet(t, c, "mp", "k", func(in *awss3.GetObjectInput) { in.VersionId = aws.String(v1) })
	_, d2 := mpGet(t, c, "mp", "k", nil)
	if string(d1) != "one" || string(d2) != "two" {
		t.Fatalf("versions = %q %q", d1, d2)
	}
}

func TestMultipartConcurrentParts(t *testing.T) {
	mpSetMinPartSize(t, 1024)
	e := newTestEnv(t)
	e.createBucket("mp")
	c := e.client
	id := mpCreate(t, c, "mp", "k", nil)
	const n = 16
	data := make([][]byte, n)
	etags := make([]string, n)
	errs := make(chan error, n)
	for i := range n {
		data[i] = mpData(int64(100+i), 1024+i)
		go func(i int) {
			o, err := mpUploadPart(c, "mp", "k", id, int32(i+1), data[i], nil)
			if err == nil {
				etags[i] = aws.ToString(o.ETag)
			}
			errs <- err
		}(i)
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	parts := make([]types.CompletedPart, n)
	for i := range n {
		parts[i] = mpPart(int32(i+1), etags[i])
	}
	out, err := mpComplete(c, "mp", "k", id, parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.ETag) != mpExpectedETag(data...) {
		t.Fatalf("ETag = %s", aws.ToString(out.ETag))
	}
	_, got := mpGet(t, c, "mp", "k", nil)
	if !bytes.Equal(got, bytes.Join(data, nil)) {
		t.Fatal("content differs")
	}
}

// --- keep-alive ---

// mpSlowBackend delays (and optionally fails) CompleteMultipartUpload.
type mpSlowBackend struct {
	storage.Backend
	delay time.Duration

	mu   sync.Mutex
	fail error
}

func (b *mpSlowBackend) setFail(err error) {
	b.mu.Lock()
	b.fail = err
	b.mu.Unlock()
}

func (b *mpSlowBackend) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []int, meta storage.ObjectMeta, opts storage.CommitOptions) (storage.ObjectInfo, error) {
	time.Sleep(b.delay)
	b.mu.Lock()
	fail := b.fail
	b.mu.Unlock()
	if fail != nil {
		return storage.ObjectInfo{}, fail
	}
	return b.Backend.CompleteMultipartUpload(ctx, bucket, key, uploadID, parts, meta, opts)
}

func TestCompleteMultipartKeepAlive(t *testing.T) {
	mpSetKeepAlive(t, 20*time.Millisecond, 10*time.Millisecond)
	slow := &mpSlowBackend{delay: 150 * time.Millisecond}
	e := newTestEnv(t, withOptions(func(o *Options) {
		slow.Backend = o.Backend
		o.Backend = slow
	}))
	e.createBucket("mp")
	ctx := context.Background()
	if err := e.backend.SetBucketVersioning(ctx, "mp", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}

	t.Run("raw success", func(t *testing.T) {
		id := mpRawCreate(t, e, "mp", "k", nil)
		pr := e.signed(http.MethodPut, mpPath("mp", "k", mpQuery("uploadId", id, "partNumber", "1")), []byte("payload"), nil)
		resp := e.signed(http.MethodPost, mpPath("mp", "k", mpQuery("uploadId", id)), mpCompleteBody("1", pr.Header.Get("ETag")), nil)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		body := string(resp.Data)
		rest, ok := strings.CutPrefix(body, xmlHeader)
		if !ok {
			t.Fatalf("body does not start with the XML declaration: %q", body)
		}
		trimmed := strings.TrimLeft(rest, " ")
		if len(trimmed) == len(rest) {
			t.Fatalf("no keep-alive whitespace: %q", body)
		}
		if !strings.HasPrefix(trimmed, `<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`) {
			t.Fatalf("body = %q", body)
		}
		var res completeMultipartUploadResult
		if err := xml.Unmarshal(resp.Data, &res); err != nil || res.ETag != mpExpectedETag([]byte("payload")) {
			t.Fatalf("result %+v: %v", res, err)
		}
		if v := resp.Trailer.Get("X-Amz-Version-Id"); v == "" {
			t.Fatalf("version id trailer missing: %v", resp.Trailer)
		}
		if resp.Header.Get("Content-Type") != "application/xml" {
			t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
		}
	})

	t.Run("sdk success", func(t *testing.T) {
		id := mpCreate(t, e.client, "mp", "k2", nil)
		o := mpMustUploadPart(t, e.client, "mp", "k2", id, 1, []byte("abc"), nil)
		out, err := mpComplete(e.client, "mp", "k2", id, []types.CompletedPart{mpPart(1, aws.ToString(o.ETag))}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.ETag) != mpExpectedETag([]byte("abc")) {
			t.Fatalf("ETag = %s", aws.ToString(out.ETag))
		}
	})

	t.Run("error after header", func(t *testing.T) {
		id := mpCreate(t, e.client, "mp", "k3", nil)
		o := mpMustUploadPart(t, e.client, "mp", "k3", id, 1, []byte("abc"), nil)
		slow.setFail(fmt.Errorf("wrapped: %w", storage.ErrUploadNotFound))
		defer slow.setFail(nil)
		resp := e.signed(http.MethodPost, mpPath("mp", "k3", mpQuery("uploadId", id)), mpCompleteBody("1", aws.ToString(o.ETag)), nil)
		if resp.StatusCode != 200 || resp.code() != "NoSuchUpload" || !bytes.Contains(resp.Data, []byte("<UploadId>"+id+"</UploadId>")) {
			t.Fatalf("got %d %s", resp.StatusCode, resp.Data)
		}
		if !strings.Contains(e.logs.String(), `"error":"NoSuchUpload"`) {
			t.Fatal("access log does not record the embedded error")
		}
		// The SDK detects the embedded error.
		_, err := mpComplete(e.client, "mp", "k3", id, []types.CompletedPart{mpPart(1, aws.ToString(o.ETag))}, nil)
		mpExpectCode(t, err, "NoSuchUpload")
	})

	t.Run("fast errors keep their status", func(t *testing.T) {
		resp := e.signed(http.MethodPost, mpPath("mp", "k", mpQuery("uploadId", "nope")), mpCompleteBody("1", `"x"`), nil)
		if resp.StatusCode != 404 {
			t.Fatalf("got %d %s", resp.StatusCode, resp.Data)
		}
	})
}

func TestUploadPartCopyKeepAlive(t *testing.T) {
	mpSetKeepAlive(t, 0, time.Millisecond)
	e := newTestEnv(t)
	e.createBucket("mp")
	src := mpData(7, 4096)
	e.putObject("mp", "src", src, storage.ObjectMeta{})
	id := mpCreate(t, e.client, "mp", "dst", nil)
	out, err := e.client.UploadPartCopy(context.Background(), &awss3.UploadPartCopyInput{
		Bucket: aws.String("mp"), Key: aws.String("dst"), UploadId: aws.String(id), PartNumber: aws.Int32(1),
		CopySource: aws.String("mp/src"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.CopyPartResult.ETag) != "\""+md5Hex(src)+"\"" {
		t.Fatalf("ETag = %s", aws.ToString(out.CopyPartResult.ETag))
	}
}

// mpPanicBackend panics in CompleteMultipartUpload.
type mpPanicBackend struct{ storage.Backend }

func (mpPanicBackend) CompleteMultipartUpload(context.Context, string, string, string, []int, storage.ObjectMeta, storage.CommitOptions) (storage.ObjectInfo, error) {
	panic("boom")
}

func TestCompleteMultipartPanicIsInternalError(t *testing.T) {
	e := newTestEnv(t, withOptions(func(o *Options) { o.Backend = mpPanicBackend{o.Backend} }))
	e.createBucket("mp")
	id := mpRawCreate(t, e, "mp", "k", nil)
	pr := e.signed(http.MethodPut, mpPath("mp", "k", mpQuery("uploadId", id, "partNumber", "1")), []byte("x"), nil)
	resp := e.signed(http.MethodPost, mpPath("mp", "k", mpQuery("uploadId", id)), mpCompleteBody("1", pr.Header.Get("ETag")), nil)
	if resp.StatusCode != http.StatusInternalServerError || resp.code() != "InternalError" {
		t.Fatalf("got %d %s", resp.StatusCode, resp.Data)
	}
	if !strings.Contains(e.logs.String(), "panic in long-running operation") {
		t.Fatal("panic not logged")
	}
}

func TestMultipartAnonymousInitiator(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("pub")
	e.putBucketConfig("pub", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
		"Action":["s3:PutObject","s3:ListMultipartUploadParts","s3:AbortMultipartUpload"],"Resource":"arn:aws:s3:::pub/*"},
		{"Effect":"Allow","Principal":"*","Action":"s3:ListBucketMultipartUploads","Resource":"arn:aws:s3:::pub"}]}`)
	resp := e.anon(http.MethodPost, "/pub/k?uploads", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("anonymous create: %d %s", resp.StatusCode, resp.Data)
	}
	resp = e.anon(http.MethodGet, "/pub?uploads", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Data), "<Initiator><ID>"+ownerFor(anonymousInitiator).ID+"</ID><DisplayName>anonymous</DisplayName></Initiator>") {
		t.Fatalf("anonymous listing: %d %s", resp.StatusCode, resp.Data)
	}
}
