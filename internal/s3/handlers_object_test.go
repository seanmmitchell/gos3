package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/s3err"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- helpers (prefixed obj to stay clear of other areas' test helpers) ---

// objCode returns the S3 error code of an SDK error.
func objCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// objStatus returns the HTTP status of an SDK error.
func objStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// objExpect checks a raw response's status and S3 error code.
func objExpect(t *testing.T, resp *testResponse, status int, code string) {
	t.Helper()
	if resp.StatusCode != status || resp.code() != code {
		t.Fatalf("got %d %q, want %d %q; body: %s", resp.StatusCode, resp.code(), status, code, resp.Data)
	}
}

// objSDKErr checks an SDK error's status and code.
func objSDKErr(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %d %s, got success", status, code)
	}
	if objStatus(err) != status || (code != "" && objCode(err) != code) {
		t.Fatalf("got %d %q (%v), want %d %q", objStatus(err), objCode(err), err, status, code)
	}
}

// objVersioned enables versioning on a bucket directly in the backend.
func objVersioned(t *testing.T, e *testEnv, bucket string, st storage.VersioningStatus) {
	t.Helper()
	if err := e.backend.SetBucketVersioning(context.Background(), bucket, st); err != nil {
		t.Fatalf("SetBucketVersioning: %v", err)
	}
}

// objLockBucket creates a bucket with object lock enabled.
func objLockBucket(t *testing.T, e *testEnv, name string) {
	t.Helper()
	if err := e.backend.CreateBucket(context.Background(), name, storage.CreateBucketOptions{Region: "us-east-1", ObjectLockEnabled: true}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

// objHdr builds a header from key/value pairs.
func objHdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

// objGetInfo reads object info from the backend.
func objGetInfo(t *testing.T, e *testEnv, bucket, key, vid string) storage.ObjectInfo {
	t.Helper()
	info, err := e.backend.GetObjectInfo(context.Background(), bucket, key, vid)
	if err != nil {
		t.Fatalf("GetObjectInfo %s/%s@%s: %v", bucket, key, vid, err)
	}
	return info
}

func objBody(t *testing.T, out *awss3.GetObjectOutput) []byte {
	t.Helper()
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// --- PutObject / GetObject / HeadObject ---

func TestObjectPutGetHeadSDK(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	ctx := context.Background()
	data := []byte("hello, gos3 object world")
	put, err := e.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt"), Body: bytes.NewReader(data),
		ContentType: aws.String("text/plain"), CacheControl: aws.String("max-age=60"),
		ContentDisposition: aws.String(`attachment; filename="x.txt"`), ContentLanguage: aws.String("en"),
		Metadata: map[string]string{"color": "blue", "Owner": "Alice"},
		Tagging:  aws.String("a=1&b=two"), StorageClass: types.StorageClassStandardIa,
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if got, want := aws.ToString(put.ETag), `"`+md5Hex(data)+`"`; got != want {
		t.Errorf("ETag %s, want %s", got, want)
	}
	if put.VersionId != nil {
		t.Errorf("unversioned bucket returned version %q", *put.VersionId)
	}

	get, err := e.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt")})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if b := objBody(t, get); !bytes.Equal(b, data) {
		t.Errorf("body %q", b)
	}
	if aws.ToString(get.ContentType) != "text/plain" || aws.ToString(get.CacheControl) != "max-age=60" ||
		aws.ToString(get.ContentLanguage) != "en" || aws.ToString(get.ContentDisposition) != `attachment; filename="x.txt"` {
		t.Errorf("headers: %+v", get)
	}
	if get.Metadata["color"] != "blue" || get.Metadata["owner"] != "Alice" {
		t.Errorf("metadata %v", get.Metadata)
	}
	if aws.ToInt32(get.TagCount) != 2 || get.StorageClass != types.StorageClassStandardIa {
		t.Errorf("tag count %v storage class %v", get.TagCount, get.StorageClass)
	}
	if aws.ToInt64(get.ContentLength) != int64(len(data)) || aws.ToString(get.AcceptRanges) != "bytes" {
		t.Errorf("length %v accept-ranges %v", get.ContentLength, get.AcceptRanges)
	}

	head, err := e.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt")})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(data)) || aws.ToString(head.ETag) != aws.ToString(put.ETag) {
		t.Errorf("head %+v", head)
	}

	_, err = e.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("missing")})
	objSDKErr(t, err, 404, "NoSuchKey")
	_, err = e.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("missing")})
	objSDKErr(t, err, 404, "")
	_, err = e.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("nobkt"), Key: aws.String("k"), Body: bytes.NewReader(data)})
	objSDKErr(t, err, 404, "NoSuchBucket")
}

func TestObjectPutRawHeaders(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	data := []byte("raw body")
	resp := e.signed("PUT", "/bkt/raw", data, nil)
	objExpect(t, resp, 200, "")
	if resp.Header.Get("ETag") != `"`+md5Hex(data)+`"` {
		t.Errorf("ETag %q", resp.Header.Get("ETag"))
	}
	// Without a requested checksum a CRC64NVME checksum is stored by default.
	want := computeChecksum(ChecksumCRC64NVME, data)
	if got := resp.Header.Get("X-Amz-Checksum-Crc64nvme"); got != want {
		t.Errorf("default checksum %q, want %q", got, want)
	}
	if resp.Header.Get("X-Amz-Checksum-Type") != ChecksumTypeFullObject || resp.Header.Get("X-Amz-Version-Id") != "" {
		t.Errorf("headers %v", resp.Header)
	}
	info := objGetInfo(t, e, "bkt", "raw", "")
	if info.Checksums[ChecksumCRC64NVME] != want || info.ChecksumType != ChecksumTypeFullObject || info.ETag != md5Hex(data) {
		t.Errorf("stored %+v", info.ObjectMeta)
	}
	// GET returns the checksum only in checksum mode.
	g := e.signed("GET", "/bkt/raw", nil, nil)
	if g.Header.Get("X-Amz-Checksum-Crc64nvme") != "" || g.Header.Get("Content-Type") != "binary/octet-stream" {
		t.Errorf("GET headers %v", g.Header)
	}
	g = e.signed("GET", "/bkt/raw", nil, objHdr("X-Amz-Checksum-Mode", "ENABLED"))
	if g.Header.Get("X-Amz-Checksum-Crc64nvme") != want {
		t.Errorf("checksum mode headers %v", g.Header)
	}
	// Ranged reads never carry full-object checksums.
	g = e.signed("GET", "/bkt/raw", nil, objHdr("X-Amz-Checksum-Mode", "ENABLED", "Range", "bytes=0-1"))
	if g.StatusCode != 206 || g.Header.Get("X-Amz-Checksum-Crc64nvme") != "" {
		t.Errorf("ranged checksum headers %d %v", g.StatusCode, g.Header)
	}

	// Empty objects are fine.
	objExpect(t, e.signed("PUT", "/bkt/empty", nil, nil), 200, "")
	g = e.signed("GET", "/bkt/empty", nil, nil)
	if g.StatusCode != 200 || len(g.Data) != 0 || g.Header.Get("Content-Length") != "0" {
		t.Errorf("empty GET %d %q %v", g.StatusCode, g.Data, g.Header)
	}
	if g.Header.Get("ETag") != `"d41d8cd98f00b204e9800998ecf8427e"` {
		t.Errorf("empty ETag %q", g.Header.Get("ETag"))
	}
}

func TestObjectPutValidation(t *testing.T) {
	e := newTestEnv(t, withOptions(func(o *Options) { o.MaxObjectSize = 16 }))
	e.createBucket("bkt")
	data := []byte("0123456789")
	good := base64.StdEncoding.EncodeToString(md5Sum16(data))
	bad := base64.StdEncoding.EncodeToString(md5Sum16([]byte("x")))
	for _, tc := range []struct {
		name   string
		body   []byte
		hdr    http.Header
		status int
		code   string
	}{
		{"content-md5 ok", data, objHdr("Content-MD5", good), 200, ""},
		{"content-md5 mismatch", data, objHdr("Content-MD5", bad), 400, "BadDigest"},
		{"content-md5 malformed", data, objHdr("Content-MD5", "zzz"), 400, "InvalidDigest"},
		{"checksum mismatch", data, objHdr("X-Amz-Checksum-Sha256", computeChecksum(ChecksumSHA256, []byte("no"))), 400, "BadDigest"},
		{"checksum ok", data, objHdr("X-Amz-Checksum-Sha1", computeChecksum(ChecksumSHA1, data)), 200, ""},
		{"too large", bytes.Repeat([]byte("x"), 17), nil, 400, "EntityTooLarge"},
		{"bad storage class", data, objHdr("X-Amz-Storage-Class", "FAST"), 400, "InvalidStorageClass"},
		{"bad acl", data, objHdr("X-Amz-Acl", "everyone"), 400, "InvalidArgument"},
		{"bad tagging", data, objHdr("X-Amz-Tagging", "aws:x=1"), 400, "InvalidTag"},
		{"metadata too large", data, objHdr("X-Amz-Meta-Big", strings.Repeat("v", 2100)), 400, "MetadataTooLarge"},
		{"lock on plain bucket", data, objHdr("X-Amz-Object-Lock-Legal-Hold", "ON"), 400, "InvalidRequest"},
		{"sse-c", data, objHdr("X-Amz-Server-Side-Encryption-Customer-Algorithm", "AES256"), 501, "NotImplemented"},
		{"grant header", data, objHdr("X-Amz-Grant-Read", "uri=x"), 501, "NotImplemented"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.signed("PUT", "/bkt/v-"+strings.ReplaceAll(tc.name, " ", "-"), tc.body, tc.hdr)
			objExpect(t, resp, tc.status, tc.code)
			_, err := e.backend.GetObjectInfo(context.Background(), "bkt", "v-"+strings.ReplaceAll(tc.name, " ", "-"), "")
			if (err == nil) != (tc.status == 200) {
				t.Errorf("stored=%v for status %d", err == nil, tc.status)
			}
		})
	}
}

func TestObjectPutMissingContentLength(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	req := e.newRequest("PUT", "/bkt/k", nil, nil)
	req.Body = io.NopCloser(strings.NewReader("streamed"))
	req.ContentLength = -1
	if err := auth.SignRequest(req, testRoot, "us-east-1", "s3", auth.PayloadUnsigned, time.Now()); err != nil {
		t.Fatal(err)
	}
	objExpect(t, e.do(req), http.StatusLengthRequired, "MissingContentLength")
}

func TestObjectPutConditional(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	first := []byte("first")
	objExpect(t, e.signed("PUT", "/bkt/k", first, objHdr("If-None-Match", "*")), 200, "")
	resp := e.signed("PUT", "/bkt/k", []byte("second"), objHdr("If-None-Match", "*"))
	objExpect(t, resp, 412, "PreconditionFailed")
	if !strings.Contains(string(resp.Data), "<Condition>If-None-Match</Condition>") {
		t.Errorf("body %s", resp.Data)
	}
	objExpect(t, e.signed("PUT", "/bkt/k", []byte("x"), objHdr("If-Match", `"0000"`)), 412, "PreconditionFailed")
	objExpect(t, e.signed("PUT", "/bkt/k", []byte("third"), objHdr("If-Match", `"`+md5Hex(first)+`"`)), 200, "")
	if info := objGetInfo(t, e, "bkt", "k", ""); info.ETag != md5Hex([]byte("third")) {
		t.Errorf("content not replaced: %s", info.ETag)
	}
	objExpect(t, e.signed("PUT", "/bkt/nope", []byte("x"), objHdr("If-Match", `"abc"`)), 404, "NoSuchKey")
	objExpect(t, e.signed("PUT", "/bkt/k", []byte("x"), objHdr("If-None-Match", `"abc"`)), 501, "NotImplemented")
	objExpect(t, e.signed("PUT", "/bkt/k", []byte("x"), objHdr("If-None-Match", "*", "If-Match", `"abc"`)), 501, "NotImplemented")

	// The SDK's IfNoneMatch maps onto the same atomic check.
	_, err := e.client.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("sdk"), IfNoneMatch: aws.String("*"),
	})
	objSDKErr(t, err, 412, "PreconditionFailed")

	// Concurrent If-None-Match writers: exactly one wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := e.signed("PUT", "/bkt/race", []byte("w"+strconv.Itoa(i)), objHdr("If-None-Match", "*"))
			if r.StatusCode == 200 {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if r.StatusCode != 412 {
				t.Errorf("racer %d: %d %s", i, r.StatusCode, r.Data)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d writers won", wins)
	}
}

func TestObjectPutVersioned(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	objVersioned(t, e, "bkt", storage.VersioningEnabled)
	r1 := e.signed("PUT", "/bkt/k", []byte("v1"), nil)
	r2 := e.signed("PUT", "/bkt/k", []byte("v2"), nil)
	v1, v2 := r1.Header.Get("X-Amz-Version-Id"), r2.Header.Get("X-Amz-Version-Id")
	if v1 == "" || v2 == "" || v1 == v2 || v1 == "null" {
		t.Fatalf("versions %q %q", v1, v2)
	}
	g := e.signed("GET", "/bkt/k?versionId="+v1, nil, nil)
	if string(g.Data) != "v1" || g.Header.Get("X-Amz-Version-Id") != v1 {
		t.Errorf("GET v1: %q %v", g.Data, g.Header)
	}
	g = e.signed("GET", "/bkt/k", nil, nil)
	if string(g.Data) != "v2" || g.Header.Get("X-Amz-Version-Id") != v2 {
		t.Errorf("GET latest: %q %v", g.Data, g.Header)
	}
	objVersioned(t, e, "bkt", storage.VersioningSuspended)
	r3 := e.signed("PUT", "/bkt/k", []byte("v3"), nil)
	if r3.Header.Get("X-Amz-Version-Id") != "null" {
		t.Errorf("suspended version %q", r3.Header.Get("X-Amz-Version-Id"))
	}
}

func TestObjectPutObjectLock(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	objLockBucket(t, e, "lock")
	ctx := context.Background()
	until := time.Now().Add(time.Hour).UTC()
	_, err := e.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: aws.String("lock"), Key: aws.String("k"), Body: strings.NewReader("x"),
		ObjectLockMode: types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: aws.Time(until),
		ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
	})
	if err != nil {
		t.Fatalf("PutObject with lock: %v", err)
	}
	info := objGetInfo(t, e, "lock", "k", "")
	if info.RetentionMode != "GOVERNANCE" || !info.LegalHold || info.RetainUntil.Unix() != until.Unix() {
		t.Errorf("stored lock %+v", info.ObjectMeta)
	}
	head, err := e.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String("lock"), Key: aws.String("k")})
	if err != nil || head.ObjectLockMode != types.ObjectLockModeGovernance || head.ObjectLockLegalHoldStatus != types.ObjectLockLegalHoldStatusOn {
		t.Errorf("head lock: %v %+v", err, head)
	}

	// A user allowed s3:PutObject but not s3:PutObjectRetention cannot set retention.
	e.putPolicy("putonly", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject"],"Resource":["arn:aws:s3:::lock/*"]}]}`)
	u := e.createUser("writer", "putonly")
	resp := e.signedAs(u, "PUT", "/lock/u", []byte("x"), objHdr("X-Amz-Object-Lock-Mode", "COMPLIANCE",
		"X-Amz-Object-Lock-Retain-Until-Date", until.Format(time.RFC3339)))
	objExpect(t, resp, 403, "AccessDenied")
	resp = e.signedAs(u, "PUT", "/lock/u", []byte("x"), objHdr("X-Amz-Object-Lock-Legal-Hold", "ON"))
	objExpect(t, resp, 403, "AccessDenied")
	objExpect(t, e.signedAs(u, "PUT", "/lock/u", []byte("x"), nil), 200, "")

	// Bucket default retention applies when no explicit retention is given.
	e.putBucketConfig("lock", storage.ConfigObjectLock, `<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>2</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
	objExpect(t, e.signed("PUT", "/lock/d", []byte("x"), nil), 200, "")
	info = objGetInfo(t, e, "lock", "d", "")
	if info.RetentionMode != "COMPLIANCE" || time.Until(info.RetainUntil) < 47*time.Hour || time.Until(info.RetainUntil) > 49*time.Hour {
		t.Errorf("default retention %s %v", info.RetentionMode, info.RetainUntil)
	}
	// Explicit headers win over the default.
	objExpect(t, e.signed("PUT", "/lock/x", []byte("x"), objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE",
		"X-Amz-Object-Lock-Retain-Until-Date", until.Format(time.RFC3339))), 200, "")
	if info := objGetInfo(t, e, "lock", "x", ""); info.RetentionMode != "GOVERNANCE" {
		t.Errorf("explicit retention %s", info.RetentionMode)
	}
	// Invalid lock headers.
	objExpect(t, e.signed("PUT", "/lock/y", []byte("x"), objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE")), 400, "InvalidArgument")
	objExpect(t, e.signed("PUT", "/lock/y", []byte("x"), objHdr("X-Amz-Object-Lock-Mode", "GOVERNANCE",
		"X-Amz-Object-Lock-Retain-Until-Date", time.Now().Add(-time.Hour).Format(time.RFC3339))), 400, "InvalidArgument")
}

func TestObjectPutDefaultEncryptionAndACL(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	resp := e.signed("PUT", "/bkt/plain", []byte("x"), nil)
	if resp.Header.Get("X-Amz-Server-Side-Encryption") != "" {
		t.Errorf("unexpected SSE header")
	}
	resp = e.signed("PUT", "/bkt/explicit", []byte("x"), objHdr("X-Amz-Server-Side-Encryption", "AES256"))
	if resp.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Errorf("explicit SSE: %v", resp.Header)
	}
	e.putBucketConfig("bkt", storage.ConfigEncryption, `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>key-1</KMSMasterKeyID></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`)
	resp = e.signed("PUT", "/bkt/enc", []byte("x"), nil)
	if resp.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" || resp.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "key-1" {
		t.Errorf("default SSE: %v", resp.Header)
	}
	h := e.signed("HEAD", "/bkt/enc", nil, nil)
	if h.Header.Get("X-Amz-Server-Side-Encryption") != "aws:kms" {
		t.Errorf("HEAD SSE: %v", h.Header)
	}

	// Public ACLs are refused under BlockPublicAcls; ACLs are refused when
	// ownership is BucketOwnerEnforced.
	e.createBucket("pab")
	e.putBucketConfig("pab", storage.ConfigPublicAccessBlock, `<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls></PublicAccessBlockConfiguration>`)
	objExpect(t, e.signed("PUT", "/pab/k", []byte("x"), objHdr("X-Amz-Acl", "public-read")), 403, "AccessDenied")
	objExpect(t, e.signed("PUT", "/pab/k", []byte("x"), objHdr("X-Amz-Acl", "private")), 200, "")
	e.createBucket("own")
	e.putBucketConfig("own", storage.ConfigOwnership, `<OwnershipControls><Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`)
	objExpect(t, e.signed("PUT", "/own/k", []byte("x"), objHdr("X-Amz-Acl", "public-read")), 400, "AccessControlListNotSupported")
	objExpect(t, e.signed("PUT", "/own/k", []byte("x"), objHdr("X-Amz-Acl", "bucket-owner-full-control")), 200, "")
	objExpect(t, e.signed("PUT", "/own/k2", []byte("x"), nil), 200, "")
}

func TestObjectGetRange(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	data := []byte("0123456789")
	e.putObject("bkt", "k", data, storage.ObjectMeta{})
	for _, tc := range []struct {
		rng    string
		status int
		body   string
		cr     string
	}{
		{"bytes=0-4", 206, "01234", "bytes 0-4/10"},
		{"bytes=5-", 206, "56789", "bytes 5-9/10"},
		{"bytes=-3", 206, "789", "bytes 7-9/10"},
		{"bytes=8-100", 206, "89", "bytes 8-9/10"},
		{"bytes=-100", 206, "0123456789", "bytes 0-9/10"},
		{"bytes=0-0", 206, "0", "bytes 0-0/10"},
		{"bytes=0-1,3-4", 200, "0123456789", ""},
		{"items=0-1", 200, "0123456789", ""},
		{"bytes=5-2", 200, "0123456789", ""},
		{"bytes=10-", 416, "", "bytes */10"},
		{"bytes=-0", 416, "", "bytes */10"},
	} {
		t.Run(tc.rng, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				resp := e.signed(method, "/bkt/k", nil, objHdr("Range", tc.rng))
				if resp.StatusCode != tc.status || resp.Header.Get("Content-Range") != tc.cr {
					t.Fatalf("%s: %d %q (%s)", method, resp.StatusCode, resp.Header.Get("Content-Range"), resp.Data)
				}
				if tc.status == 416 {
					if method == "GET" && resp.code() != "InvalidRange" {
						t.Errorf("code %q", resp.code())
					}
					continue
				}
				if want := strconv.Itoa(len(tc.body)); resp.Header.Get("Content-Length") != want {
					t.Errorf("%s Content-Length %q want %s", method, resp.Header.Get("Content-Length"), want)
				}
				if method == "GET" && string(resp.Data) != tc.body {
					t.Errorf("body %q", resp.Data)
				}
				if method == "HEAD" && len(resp.Data) != 0 {
					t.Errorf("HEAD body %q", resp.Data)
				}
			}
		})
	}
	// SDK range read.
	out, err := e.client.GetObject(context.Background(), &awss3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Range: aws.String("bytes=2-3")})
	if err != nil {
		t.Fatal(err)
	}
	if b := objBody(t, out); string(b) != "23" || aws.ToString(out.ContentRange) != "bytes 2-3/10" {
		t.Errorf("SDK range %q %v", b, out.ContentRange)
	}
}

func TestObjectGetConditional(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	info := e.putObject("bkt", "k", []byte("data"), storage.ObjectMeta{})
	etag := `"` + info.ETag + `"`
	past := info.LastModified.Add(-time.Hour).UTC().Format(http.TimeFormat)
	future := info.LastModified.Add(time.Hour).UTC().Format(http.TimeFormat)
	for _, tc := range []struct {
		name   string
		hdr    http.Header
		status int
	}{
		{"if-match ok", objHdr("If-Match", etag), 200},
		{"if-match star", objHdr("If-Match", "*"), 200},
		{"if-match fail", objHdr("If-Match", `"nope"`), 412},
		{"if-none-match hit", objHdr("If-None-Match", etag), 304},
		{"if-none-match miss", objHdr("If-None-Match", `"nope"`), 200},
		{"if-modified-since future", objHdr("If-Modified-Since", future), 304},
		{"if-modified-since past", objHdr("If-Modified-Since", past), 200},
		{"if-unmodified-since past", objHdr("If-Unmodified-Since", past), 412},
		{"if-unmodified-since future", objHdr("If-Unmodified-Since", future), 200},
		{"if-match beats unmodified-since", objHdr("If-Match", etag, "If-Unmodified-Since", past), 200},
		{"if-none-match beats modified-since", objHdr("If-None-Match", `"nope"`, "If-Modified-Since", future), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"GET", "HEAD"} {
				resp := e.signed(method, "/bkt/k", nil, tc.hdr)
				if resp.StatusCode != tc.status {
					t.Fatalf("%s: %d %s", method, resp.StatusCode, resp.Data)
				}
				switch tc.status {
				case 304:
					if resp.Header.Get("ETag") != etag || resp.Header.Get("Last-Modified") == "" || len(resp.Data) != 0 {
						t.Errorf("304 headers %v body %q", resp.Header, resp.Data)
					}
				case 412:
					if method == "GET" && resp.code() != "PreconditionFailed" {
						t.Errorf("code %q", resp.code())
					}
				case 200:
					if method == "GET" && string(resp.Data) != "data" {
						t.Errorf("body %q", resp.Data)
					}
				}
			}
		})
	}
}

func TestObjectGetPartNumber(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	data := []byte("aaaaabbbcc")
	parts := []map[string]string{
		{ChecksumCRC32: computeChecksum(ChecksumCRC32, data[:5])},
		{ChecksumCRC32: computeChecksum(ChecksumCRC32, data[5:8])},
		{ChecksumCRC32: computeChecksum(ChecksumCRC32, data[8:])},
	}
	e.putObject("bkt", "mp", data, storage.ObjectMeta{
		ETag: "0123456789abcdef0123456789abcdef-3", PartSizes: []int64{5, 3, 2}, PartChecksums: parts,
		Checksums: map[string]string{ChecksumCRC32: "AAAAAA==-3"}, ChecksumType: ChecksumTypeComposite,
	})
	e.putObject("bkt", "single", data, storage.ObjectMeta{})

	resp := e.signed("GET", "/bkt/mp?partNumber=2", nil, objHdr("X-Amz-Checksum-Mode", "ENABLED"))
	if resp.StatusCode != 206 || string(resp.Data) != "bbb" || resp.Header.Get("Content-Range") != "bytes 5-7/10" ||
		resp.Header.Get("X-Amz-Mp-Parts-Count") != "3" || resp.Header.Get("Content-Length") != "3" {
		t.Fatalf("part 2: %d %q %v", resp.StatusCode, resp.Data, resp.Header)
	}
	if resp.Header.Get("X-Amz-Checksum-Crc32") != parts[1][ChecksumCRC32] {
		t.Errorf("part checksum %q", resp.Header.Get("X-Amz-Checksum-Crc32"))
	}
	resp = e.signed("HEAD", "/bkt/mp?partNumber=3", nil, nil)
	if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 8-9/10" || resp.Header.Get("Content-Length") != "2" {
		t.Errorf("HEAD part 3: %d %v", resp.StatusCode, resp.Header)
	}
	objExpect(t, e.signed("GET", "/bkt/mp?partNumber=4", nil, nil), 416, "InvalidPartNumber")
	objExpect(t, e.signed("GET", "/bkt/mp?partNumber=0", nil, nil), 400, "InvalidArgument")
	objExpect(t, e.signed("GET", "/bkt/mp?partNumber=x", nil, nil), 400, "InvalidArgument")
	objExpect(t, e.signed("GET", "/bkt/mp?partNumber=1", nil, objHdr("Range", "bytes=0-1")), 400, "InvalidRequest")

	// Part 1 of a single-part object is the whole object, served like a
	// ranged GET (206 + Content-Range, as S3 does).
	resp = e.signed("GET", "/bkt/single?partNumber=1", nil, nil)
	if resp.StatusCode != 206 || string(resp.Data) != string(data) || resp.Header.Get("Content-Range") != "bytes 0-9/10" ||
		resp.Header.Get("Content-Length") != "10" {
		t.Errorf("single part 1: %d %q %v", resp.StatusCode, resp.Data, resp.Header)
	}
	resp = e.signed("HEAD", "/bkt/single?partNumber=1", nil, nil)
	if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 0-9/10" {
		t.Errorf("HEAD single part 1: %d %v", resp.StatusCode, resp.Header)
	}
	// Empty parts and objects never produce a 206 without Content-Range.
	e.putObject("bkt", "mp-empty-last", data[:5], storage.ObjectMeta{
		ETag: "0123456789abcdef0123456789abcdef-2", PartSizes: []int64{5, 0},
	})
	e.putObject("bkt", "empty", nil, storage.ObjectMeta{})
	for _, p := range []string{"/bkt/mp-empty-last?partNumber=2", "/bkt/empty?partNumber=1"} {
		for _, m := range []string{"GET", "HEAD"} {
			resp = e.signed(m, p, nil, nil)
			if resp.StatusCode != 200 || resp.Header.Get("Content-Length") != "0" || resp.Header.Get("Content-Range") != "" || len(resp.Data) != 0 {
				t.Errorf("%s %s: %d %v", m, p, resp.StatusCode, resp.Header)
			}
		}
	}
	objExpect(t, e.signed("GET", "/bkt/single?partNumber=2", nil, nil), 416, "InvalidPartNumber")

	// The SDK sees the part count and part body.
	out, err := e.client.GetObject(context.Background(), &awss3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), PartNumber: aws.Int32(1)})
	if err != nil {
		t.Fatal(err)
	}
	if b := objBody(t, out); string(b) != "aaaaa" || aws.ToInt32(out.PartsCount) != 3 {
		t.Errorf("SDK part 1: %q parts %v", b, out.PartsCount)
	}
	// The whole multipart object in checksum mode reports the composite checksum.
	resp = e.signed("GET", "/bkt/mp", nil, objHdr("X-Amz-Checksum-Mode", "ENABLED"))
	if resp.Header.Get("X-Amz-Checksum-Crc32") != "AAAAAA==-3" || resp.Header.Get("X-Amz-Checksum-Type") != "COMPOSITE" {
		t.Errorf("whole mp checksum headers %v", resp.Header)
	}
}

func TestObjectGetResponseOverrides(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("bkt")
	e.putObject("bkt", "k", []byte("data"), storage.ObjectMeta{ContentType: "text/plain"})
	q := "?response-content-type=application%2Fjson&response-content-disposition=attachment%3B%20filename%3D%22a.json%22" +
		"&response-cache-control=no-cache&response-content-language=de&response-expires=Thu%2C%2001%20Dec%201994%2016%3A00%3A00%20GMT" +
		"&response-content-encoding=identity"
	resp := e.signed("GET", "/bkt/k"+q, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	for k, v := range map[string]string{
		"Content-Type": "application/json", "Content-Disposition": `attachment; filename="a.json"`,
		"Cache-Control": "no-cache", "Content-Language": "de", "Expires": "Thu, 01 Dec 1994 16:00:00 GMT",
		"Content-Encoding": "identity",
	} {
		if resp.Header.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, resp.Header.Get(k), v)
		}
	}
	// Anonymous requests may not override headers, even on public objects.
	e.putBucketConfig("bkt", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`)
	objExpect(t, e.anon("GET", "/bkt/k", nil, nil), 200, "")
	objExpect(t, e.anon("GET", "/bkt/k?response-content-type=x", nil, nil), 400, "InvalidRequest")
}

func TestObjectPresignedSDK(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	ctx := context.Background()
	pc := awss3.NewPresignClient(e.client)
	data := []byte("presigned upload")
	preq, err := pc.PresignPutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("p/k.txt")},
		awss3.WithPresignExpires(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(preq.Method, preq.URL, bytes.NewReader(data))
	for k, vs := range preq.SignedHeader {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp := e.do(req)
	objExpect(t, resp, 200, "")
	if resp.Header.Get("ETag") != `"`+md5Hex(data)+`"` {
		t.Errorf("ETag %q", resp.Header.Get("ETag"))
	}

	greq, err := pc.PresignGetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("p/k.txt"),
		ResponseContentDisposition: aws.String("attachment")})
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(greq.Method, greq.URL, nil)
	resp = e.do(req)
	if resp.StatusCode != 200 || !bytes.Equal(resp.Data, data) || resp.Header.Get("Content-Disposition") != "attachment" {
		t.Errorf("presigned GET %d %q %v", resp.StatusCode, resp.Data, resp.Header)
	}
	// Tampering with the URL breaks the signature.
	req, _ = http.NewRequest("GET", strings.Replace(greq.URL, "p/k.txt", "p/other.txt", 1), nil)
	objExpect(t, e.do(req), 403, "SignatureDoesNotMatch")
}

func TestObjectChecksumsSDK(t *testing.T) {
	data := bytes.Repeat([]byte("checksum-data-"), 5000)
	algos := map[types.ChecksumAlgorithm]string{
		types.ChecksumAlgorithmCrc32:     ChecksumCRC32,
		types.ChecksumAlgorithmCrc32c:    ChecksumCRC32C,
		types.ChecksumAlgorithmCrc64nvme: ChecksumCRC64NVME,
		types.ChecksumAlgorithmSha1:      ChecksumSHA1,
		types.ChecksumAlgorithmSha256:    ChecksumSHA256,
	}
	for _, tls := range []bool{false, true} {
		var opts []testEnvOption
		if tls {
			opts = append(opts, withTLS())
		}
		e := newTestEnv(t, opts...)
		e.createBucket("bkt")
		ctx := context.Background()
		for sdkAlgo, algo := range algos {
			name := fmt.Sprintf("%s/tls=%v", algo, tls)
			key := "obj-" + algo
			put, err := e.client.PutObject(ctx, &awss3.PutObjectInput{
				Bucket: aws.String("bkt"), Key: aws.String(key), Body: bytes.NewReader(data), ChecksumAlgorithm: sdkAlgo,
			})
			if err != nil {
				t.Errorf("%s: PutObject: %v", name, err)
				continue
			}
			want := computeChecksum(algo, data)
			info := objGetInfo(t, e, "bkt", key, "")
			if info.Checksums[algo] != want || len(info.Checksums) != 1 || info.ETag != md5Hex(data) || info.Size != int64(len(data)) {
				t.Errorf("%s: stored %v etag %s size %d", name, info.Checksums, info.ETag, info.Size)
			}
			_ = put
			get, err := e.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
			if err != nil {
				t.Errorf("%s: GetObject: %v", name, err)
				continue
			}
			body := objBody(t, get) // the SDK validates the checksum while reading
			if !bytes.Equal(body, data) {
				t.Errorf("%s: body mismatch", name)
			}
			got := map[string]*string{ChecksumCRC32: get.ChecksumCRC32, ChecksumCRC32C: get.ChecksumCRC32C,
				ChecksumCRC64NVME: get.ChecksumCRC64NVME, ChecksumSHA1: get.ChecksumSHA1, ChecksumSHA256: get.ChecksumSHA256}[algo]
			if aws.ToString(got) != want || get.ChecksumType != types.ChecksumTypeFullObject {
				t.Errorf("%s: GET checksum %v type %v", name, aws.ToString(got), get.ChecksumType)
			}
		}
	}
}

func TestObjectSDKTrailerUpload(t *testing.T) {
	// Over TLS the SDK uploads with STREAMING-UNSIGNED-PAYLOAD-TRAILER and a
	// trailing checksum; Content-Encoding must lose its aws-chunked token.
	e := newTestEnv(t, withTLS())
	e.createBucket("bkt")
	data := bytes.Repeat([]byte("z"), 300000)
	_, err := e.client.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("k"), Body: bytes.NewReader(data), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c,
		ContentEncoding: aws.String("gzip"),
	})
	if err != nil {
		t.Fatal(err)
	}
	info := objGetInfo(t, e, "bkt", "k", "")
	if info.Checksums[ChecksumCRC32C] != computeChecksum(ChecksumCRC32C, data) || info.Size != int64(len(data)) {
		t.Errorf("stored %v size %d", info.Checksums, info.Size)
	}
	if info.ContentEncoding != "gzip" {
		t.Errorf("content encoding %q (aws-chunked must be stripped)", info.ContentEncoding)
	}
}

func TestObjectRawAWSChunkedTrailer(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	data := []byte("hello aws-chunked world")
	send := func(key, trailer string, decodedLen int) *testResponse {
		var body bytes.Buffer
		fmt.Fprintf(&body, "%x\r\n", 10)
		body.Write(data[:10])
		fmt.Fprintf(&body, "\r\n%x\r\n", len(data)-10)
		body.Write(data[10:])
		body.WriteString("\r\n0\r\n")
		if trailer != "" {
			body.WriteString("x-amz-checksum-crc32:" + trailer + "\r\n")
		}
		body.WriteString("\r\n")
		req := e.newRequest("PUT", "/bkt/"+key, body.Bytes(), objHdr(
			"Content-Encoding", "aws-chunked",
			"X-Amz-Decoded-Content-Length", strconv.Itoa(decodedLen),
			"X-Amz-Trailer", "x-amz-checksum-crc32",
		))
		if err := auth.SignRequest(req, testRoot, "us-east-1", "s3", auth.PayloadStreamingUnsignedTrailer, time.Now()); err != nil {
			t.Fatal(err)
		}
		return e.do(req)
	}
	good := computeChecksum(ChecksumCRC32, data)
	resp := send("good", good, len(data))
	objExpect(t, resp, 200, "")
	if resp.Header.Get("X-Amz-Checksum-Crc32") != good {
		t.Errorf("checksum header %v", resp.Header)
	}
	info := objGetInfo(t, e, "bkt", "good", "")
	if info.Size != int64(len(data)) || info.Checksums[ChecksumCRC32] != good || info.ContentEncoding != "" {
		t.Errorf("stored %+v", info)
	}
	objExpect(t, send("bad", computeChecksum(ChecksumCRC32, []byte("x")), len(data)), 400, "BadDigest")
	if r := send("missing", "", len(data)); r.StatusCode != 400 {
		t.Errorf("missing trailer: %d %s", r.StatusCode, r.Data)
	}
	if r := send("short", good, len(data)+5); r.StatusCode != 400 {
		t.Errorf("wrong decoded length: %d %s", r.StatusCode, r.Data)
	}
	for _, k := range []string{"bad", "missing", "short"} {
		if _, err := e.backend.GetObjectInfo(context.Background(), "bkt", k, ""); err == nil {
			t.Errorf("%s stored", k)
		}
	}
}

// --- Versions and delete markers ---

func TestObjectDeleteMarkers(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	objVersioned(t, e, "bkt", storage.VersioningEnabled)
	v1 := e.signed("PUT", "/bkt/k", []byte("one"), nil).Header.Get("X-Amz-Version-Id")
	del := e.signed("DELETE", "/bkt/k", nil, nil)
	if del.StatusCode != 204 || del.Header.Get("X-Amz-Delete-Marker") != "true" {
		t.Fatalf("delete: %d %v", del.StatusCode, del.Header)
	}
	dm := del.Header.Get("X-Amz-Version-Id")
	if dm == "" || dm == v1 {
		t.Fatalf("delete marker version %q", dm)
	}
	for _, method := range []string{"GET", "HEAD"} {
		resp := e.signed(method, "/bkt/k", nil, nil)
		if resp.StatusCode != 404 || resp.Header.Get("X-Amz-Delete-Marker") != "true" || resp.Header.Get("X-Amz-Version-Id") != dm {
			t.Errorf("%s latest DM: %d %v", method, resp.StatusCode, resp.Header)
		}
		if method == "GET" && resp.code() != "NoSuchKey" {
			t.Errorf("code %q", resp.code())
		}
		resp = e.signed(method, "/bkt/k?versionId="+dm, nil, nil)
		if resp.StatusCode != 405 || resp.Header.Get("X-Amz-Delete-Marker") != "true" || resp.Header.Get("X-Amz-Version-Id") != dm {
			t.Errorf("%s addressed DM: %d %v", method, resp.StatusCode, resp.Header)
		}
		if method == "GET" && resp.code() != "MethodNotAllowed" {
			t.Errorf("code %q", resp.code())
		}
		resp = e.signed(method, "/bkt/k?versionId="+v1, nil, nil)
		if resp.StatusCode != 200 {
			t.Errorf("%s old version: %d", method, resp.StatusCode)
		}
	}
	objExpect(t, e.signed("GET", "/bkt/k?versionId=doesnotexist", nil, nil), 404, "NoSuchVersion")
	objExpect(t, e.signed("GET", "/bkt/other?versionId=doesnotexist", nil, nil), 404, "NoSuchVersion")
	objExpect(t, e.signed("GET", "/bkt/k?versionId=", nil, nil), 400, "InvalidArgument")
	objExpect(t, e.signed("GET", "/bkt/k?versionId=bad%20id", nil, nil), 400, "InvalidArgument")
	resp := e.signed("GET", "/bkt/k?versionId=doesnotexist", nil, nil)
	if !strings.Contains(string(resp.Data), "<VersionId>doesnotexist</VersionId>") || !strings.Contains(string(resp.Data), "<Key>k</Key>") {
		t.Errorf("NoSuchVersion body %s", resp.Data)
	}

	// Removing the delete marker restores the object.
	resp = e.signed("DELETE", "/bkt/k?versionId="+dm, nil, nil)
	if resp.StatusCode != 204 || resp.Header.Get("X-Amz-Delete-Marker") != "true" || resp.Header.Get("X-Amz-Version-Id") != dm {
		t.Errorf("delete DM: %d %v", resp.StatusCode, resp.Header)
	}
	if g := e.signed("GET", "/bkt/k", nil, nil); string(g.Data) != "one" {
		t.Errorf("restored: %d %q", g.StatusCode, g.Data)
	}
	// Deleting a specific version.
	resp = e.signed("DELETE", "/bkt/k?versionId="+v1, nil, nil)
	if resp.StatusCode != 204 || resp.Header.Get("X-Amz-Delete-Marker") != "" || resp.Header.Get("X-Amz-Version-Id") != v1 {
		t.Errorf("delete version: %d %v", resp.StatusCode, resp.Header)
	}
	objExpect(t, e.signed("GET", "/bkt/k", nil, nil), 404, "NoSuchKey")
	// Deleting a version that does not exist succeeds (idempotent).
	resp = e.signed("DELETE", "/bkt/k?versionId=gone", nil, nil)
	if resp.StatusCode != 204 {
		t.Errorf("delete missing version: %d %s", resp.StatusCode, resp.Data)
	}
	objExpect(t, e.signed("DELETE", "/bkt/k?versionId=", nil, nil), 400, "InvalidArgument")
}

func TestObjectDeleteUnversioned(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.putObject("bkt", "k", []byte("x"), storage.ObjectMeta{})
	ctx := context.Background()
	out, err := e.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil || out.VersionId != nil || aws.ToBool(out.DeleteMarker) {
		t.Fatalf("DeleteObject: %v %+v", err, out)
	}
	if _, err := e.backend.GetObjectInfo(ctx, "bkt", "k", ""); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Errorf("object still there: %v", err)
	}
	// Deleting a missing key is not an error.
	if _, err := e.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); err != nil {
		t.Errorf("delete missing: %v", err)
	}
	_, err = e.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String("none"), Key: aws.String("k")})
	objSDKErr(t, err, 404, "NoSuchBucket")

	// Conditional delete.
	info := e.putObject("bkt", "c", []byte("x"), storage.ObjectMeta{})
	objExpect(t, e.signed("DELETE", "/bkt/c", nil, objHdr("If-Match", `"other"`)), 412, "PreconditionFailed")
	objExpect(t, e.signed("DELETE", "/bkt/missing", nil, objHdr("If-Match", `"other"`)), 404, "NoSuchKey")
	if r := e.signed("DELETE", "/bkt/c", nil, objHdr("If-Match", `"`+info.ETag+`"`)); r.StatusCode != 204 {
		t.Errorf("conditional delete: %d %s", r.StatusCode, r.Data)
	}
}

func TestObjectDeleteObjectLock(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	objLockBucket(t, e, "lock")
	future := time.Now().Add(time.Hour)
	put := func(key string, m storage.ObjectMeta) string {
		return e.putObject("lock", key, []byte("x"), m).VersionID
	}
	hold := put("hold", storage.ObjectMeta{LegalHold: true})
	comp := put("comp", storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: future})
	gov := put("gov", storage.ObjectMeta{RetentionMode: "GOVERNANCE", RetainUntil: future})
	expired := put("expired", storage.ObjectMeta{RetentionMode: "COMPLIANCE", RetainUntil: time.Now().Add(-time.Hour)})

	objExpect(t, e.signed("DELETE", "/lock/hold?versionId="+hold, nil, nil), 403, "AccessDenied")
	objExpect(t, e.signed("DELETE", "/lock/comp?versionId="+comp, nil, objHdr("X-Amz-Bypass-Governance-Retention", "true")), 403, "AccessDenied")
	objExpect(t, e.signed("DELETE", "/lock/gov?versionId="+gov, nil, nil), 403, "AccessDenied")
	// A delete without version ID only adds a delete marker: always allowed.
	if r := e.signed("DELETE", "/lock/comp", nil, nil); r.StatusCode != 204 || r.Header.Get("X-Amz-Delete-Marker") != "true" {
		t.Errorf("delete marker on locked object: %d %v", r.StatusCode, r.Header)
	}
	// Governance bypass needs the header and s3:BypassGovernanceRetention.
	e.putPolicy("del", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:DeleteObject","s3:DeleteObjectVersion"],"Resource":["arn:aws:s3:::lock/*"]}]}`)
	u := e.createUser("deleter", "del")
	objExpect(t, e.signedAs(u, "DELETE", "/lock/gov?versionId="+gov, nil, objHdr("X-Amz-Bypass-Governance-Retention", "true")), 403, "AccessDenied")
	if r := e.signed("DELETE", "/lock/gov?versionId="+gov, nil, objHdr("X-Amz-Bypass-Governance-Retention", "true")); r.StatusCode != 204 {
		t.Errorf("root bypass: %d %s", r.StatusCode, r.Data)
	}
	if r := e.signed("DELETE", "/lock/expired?versionId="+expired, nil, nil); r.StatusCode != 204 {
		t.Errorf("expired retention: %d %s", r.StatusCode, r.Data)
	}
}

func TestObjectHeadNoBodyOnErrors(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	resp := e.signed("HEAD", "/bkt/missing", nil, nil)
	if resp.StatusCode != 404 || len(resp.Data) != 0 {
		t.Errorf("%d %q", resp.StatusCode, resp.Data)
	}
	resp = e.signed("HEAD", "/nobucket/missing", nil, nil)
	if resp.StatusCode != 404 || len(resp.Data) != 0 {
		t.Errorf("%d %q", resp.StatusCode, resp.Data)
	}
}

func TestObjectRestoreSelectNotImplemented(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.putObject("bkt", "k", []byte("x"), storage.ObjectMeta{})
	objExpect(t, e.signed("POST", "/bkt/k?restore", []byte("<RestoreRequest/>"), nil), 501, "NotImplemented")
	objExpect(t, e.signed("POST", "/bkt/k?select&select-type=2", []byte("<SelectObjectContentRequest/>"), nil), 501, "NotImplemented")
}

func TestObjectConcurrentPutGet(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "k" + strconv.Itoa(i%4)
			data := bytes.Repeat([]byte{byte('a' + i)}, 1000+i)
			if _, err := e.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
				t.Errorf("put: %v", err)
				return
			}
			out, err := e.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key)})
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			b, _ := io.ReadAll(out.Body)
			out.Body.Close()
			// Some writer won; the body must be consistent with its ETag.
			if sum := md5.Sum(b); `"`+fmt.Sprintf("%x", sum)+`"` != aws.ToString(out.ETag) {
				t.Errorf("torn read for %s", key)
			}
		}(i)
	}
	wg.Wait()
}

// --- unit tests ---

func TestParseWriteConditions(t *testing.T) {
	for _, tc := range []struct {
		hdr  http.Header
		want storage.CommitOptions
		code string
	}{
		{objHdr(), storage.CommitOptions{}, ""},
		{objHdr("If-None-Match", "*"), storage.CommitOptions{IfNoneMatch: true}, ""},
		{objHdr("If-Match", `"abc"`), storage.CommitOptions{IfMatch: "abc"}, ""},
		{objHdr("If-Match", `W/"abc"`), storage.CommitOptions{IfMatch: "abc"}, ""},
		{objHdr("If-None-Match", `"abc"`), storage.CommitOptions{}, "NotImplemented"},
		{objHdr("If-Match", "*"), storage.CommitOptions{}, "NotImplemented"},
		{objHdr("If-Match", `"a","b"`), storage.CommitOptions{}, "NotImplemented"},
		{objHdr("If-Match", `""`), storage.CommitOptions{}, "InvalidArgument"},
		{objHdr("If-Match", "a", "If-None-Match", "*"), storage.CommitOptions{}, "NotImplemented"},
	} {
		got, err := parseWriteConditions(tc.hdr)
		if codeOf(err) != tc.code || (err == nil && !reflect.DeepEqual(got, tc.want)) {
			t.Errorf("%v: %+v %v", tc.hdr, got, err)
		}
	}
}

func TestObjectVersionParam(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{"", "", true},
		{"versionId=abc", "abc", true},
		{"versionId=null", "null", true},
		{"versionId=", "", false},
		{"versionId=a%20b", "", false},
		{"versionId=" + strings.Repeat("a", 1025), "", false},
		{"versionId=%00", "", false},
		{"versionId=%C3%A9", "", false},
	} {
		q, _ := url.ParseQuery(tc.raw)
		got, err := objectVersionParam(q)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("%q: %q %v", tc.raw, got, err)
		}
		if err != nil && !errors.Is(err, s3err.ErrInvalidArgument) {
			t.Errorf("%q: error %v", tc.raw, err)
		}
	}
}
