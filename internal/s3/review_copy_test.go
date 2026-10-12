package s3

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// slowOpenBackend delays the first read of every opened object.
type slowOpenBackend struct {
	storage.Backend
	delay time.Duration
}

type slowReader struct {
	io.ReadSeekCloser
	delay time.Duration
	slept bool
}

func (r *slowReader) Read(p []byte) (int, error) {
	if !r.slept {
		r.slept = true
		time.Sleep(r.delay)
	}
	return r.ReadSeekCloser.Read(p)
}

func (b *slowOpenBackend) OpenObject(ctx context.Context, bucket, key, versionID string) (storage.ObjectInfo, io.ReadSeekCloser, error) {
	info, rd, err := b.Backend.OpenObject(ctx, bucket, key, versionID)
	if err != nil {
		return info, rd, err
	}
	return info, &slowReader{ReadSeekCloser: rd, delay: b.delay}, nil
}

// TestCopyObjectKeepAlive: a slow CopyObject answers 200 early and keeps
// the connection alive with whitespace, like AWS; the result (or an error)
// follows in the body and the new version ID as a trailer.
func TestCopyObjectKeepAlive(t *testing.T) {
	mpSetKeepAlive(t, 20*time.Millisecond, 10*time.Millisecond)
	e := newTestEnv(t, withOptions(func(o *Options) {
		o.Backend = &slowOpenBackend{Backend: o.Backend, delay: 150 * time.Millisecond}
	}))
	e.createBucket("cp")
	if err := e.backend.SetBucketVersioning(context.Background(), "cp", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	src := e.putObject("cp", "src", []byte("copy me"), storage.ObjectMeta{})

	resp := e.signed("PUT", "/cp/dst", nil, objHdr("X-Amz-Copy-Source", "/cp/src"))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Data)
	}
	rest, ok := strings.CutPrefix(string(resp.Data), xmlHeader)
	if !ok || !strings.HasPrefix(strings.TrimLeft(rest, " "), "<CopyObjectResult") || strings.TrimLeft(rest, " ") == rest {
		t.Fatalf("body %q", resp.Data)
	}
	var res copyObjectResult
	if err := xml.Unmarshal(resp.Data, &res); err != nil || res.ETag != `"`+md5Hex([]byte("copy me"))+`"` {
		t.Fatalf("result %+v %v", res, err)
	}
	if resp.Header.Get("X-Amz-Copy-Source-Version-Id") != src.VersionID || resp.Trailer.Get("X-Amz-Version-Id") == "" {
		t.Fatalf("headers %v trailer %v", resp.Header, resp.Trailer)
	}

	// The SDK handles the slow path, including an error in the 200 body.
	out, err := e.client.CopyObject(context.Background(), &awss3.CopyObjectInput{
		Bucket: aws.String("cp"), Key: aws.String("dst2"), CopySource: aws.String("cp/src")})
	if err != nil || aws.ToString(out.CopyObjectResult.ETag) != res.ETag {
		t.Fatalf("SDK copy: %+v %v", out, err)
	}
	_, err = e.client.CopyObject(context.Background(), &awss3.CopyObjectInput{
		Bucket: aws.String("cp"), Key: aws.String("dst2"), CopySource: aws.String("cp/src"), IfNoneMatch: aws.String("*")})
	if err == nil || !strings.Contains(err.Error(), "PreconditionFailed") {
		t.Fatalf("SDK conditional copy onto an existing key: %v", err)
	}
	raw := e.signed("PUT", "/cp/dst2", nil, objHdr("X-Amz-Copy-Source", "/cp/src", "If-None-Match", "*"))
	if raw.StatusCode != http.StatusOK || !strings.Contains(string(raw.Data), "<Code>PreconditionFailed</Code>") {
		t.Fatalf("embedded error: %d %s", raw.StatusCode, raw.Data)
	}
}
