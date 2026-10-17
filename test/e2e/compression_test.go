package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/seanmmitchell/gos3/internal/config"
)

// TestCompression runs compression end to end on the fs backend: a bucket
// opts in through the admin API, objects read back unchanged (whole, by
// range and after a restart), the bucket usage reports the saving, and
// the administrator's mode overrides the bucket's setting.
func TestCompression(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := baseConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = dir
	e := startPrivate(t, cfg)
	c := e.root()
	b := bucketName(t)
	_, err := c.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(b)})
	must(t, err, "CreateBucket")

	var doc struct {
		Enabled, Effective bool
		ServerMode         string
	}
	if r := e.admin(t, rootCreds, http.MethodPut, "/buckets/"+b+"/compression", map[string]any{"enabled": true}, &doc); r.status != 200 || !doc.Effective {
		t.Fatalf("PUT compression: %d %+v", r.status, doc)
	}

	var text bytes.Buffer
	for i := 0; text.Len() < 3<<20; i++ {
		fmt.Fprintf(&text, "%08d,sensor-%d,temperature,%d.%d\n", i, i%17, 20+i%9, i%10)
	}
	data := text.Bytes()
	put, err := c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("log.csv"),
		Body: bytes.NewReader(data), ContentType: aws.String("text/csv")})
	must(t, err, "PutObject")
	wantETag := fmt.Sprintf(`"%x"`, md5Sum(data))
	if aws.ToString(put.ETag) != wantETag {
		t.Fatalf("ETag = %s, want %s", aws.ToString(put.ETag), wantETag)
	}

	check := func(e *env) {
		t.Helper()
		c := e.root()
		out, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("log.csv")})
		must(t, err, "GetObject")
		got, err := io.ReadAll(out.Body)
		_ = out.Body.Close()
		must(t, err, "reading GetObject")
		if !bytes.Equal(got, data) || aws.ToString(out.ETag) != wantETag {
			t.Fatalf("GetObject: %d bytes, ETag %s", len(got), aws.ToString(out.ETag))
		}
		out, err = c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("log.csv"),
			Range: aws.String("bytes=1000000-1000099")})
		must(t, err, "ranged GetObject")
		got, _ = io.ReadAll(out.Body)
		_ = out.Body.Close()
		if !bytes.Equal(got, data[1000000:1000100]) {
			t.Fatal("ranged GetObject: wrong content")
		}
		var sum struct {
			Compression struct{ Enabled, Effective bool }
			Usage       struct {
				Bytes, StoredBytes, CompressedVersions int64
			}
		}
		if r := e.admin(t, rootCreds, http.MethodGet, "/buckets/"+b, nil, &sum); r.status != 200 {
			t.Fatalf("GET bucket: %d", r.status)
		}
		if u := sum.Usage; u.Bytes != int64(len(data)) || u.CompressedVersions != 1 || u.StoredBytes <= 0 || u.StoredBytes > u.Bytes/3 {
			t.Fatalf("usage = %+v", u)
		}
	}
	check(e)

	// Restart on the same data directory.
	must(t, e.stop(), "stopping the server")
	e = startPrivate(t, cfg)
	check(e)

	// The administrator disables compression: the bucket cannot turn it
	// on again, new objects are stored as is and old ones stay readable.
	var settings map[string]any
	if r := e.admin(t, rootCreds, http.MethodGet, "/config", nil, &settings); r.status != 200 {
		t.Fatalf("GET config: %d", r.status)
	}
	settings["compression"].(map[string]any)["mode"] = config.CompressionDisabled
	if r := e.admin(t, rootCreds, http.MethodPut, "/config", settings, nil); r.status != 200 {
		t.Fatalf("PUT config: %d %s", r.status, r.body)
	}
	if r := e.admin(t, rootCreds, http.MethodPut, "/buckets/"+b+"/compression", map[string]any{"enabled": true}, nil); r.status != http.StatusConflict {
		t.Fatalf("enabling while disabled: %d", r.status)
	}
	if r := e.admin(t, rootCreds, http.MethodGet, "/buckets/"+b+"/compression", nil, &doc); r.status != 200 || !doc.Enabled || doc.Effective || doc.ServerMode != "disabled" {
		t.Fatalf("GET compression while disabled: %d %+v", r.status, doc)
	}
	_, err = e.root().PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("new.csv"), Body: bytes.NewReader(data)})
	must(t, err, "PutObject while disabled")
	check(e) // still exactly one compressed version
}
