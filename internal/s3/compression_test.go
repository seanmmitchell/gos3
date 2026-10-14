package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/compression"
	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// compressibleText returns n bytes of compressible text.
func compressibleText(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "row %d,the quick brown fox,jumps over the lazy dog\n", i)
	}
	return b.Bytes()[:n]
}

func withCompressionMode(mode string) testEnvOption {
	return withSettings(func(s *config.Settings) { s.Compression.Mode = mode })
}

func (e *testEnv) enableBucketCompression(bucket string, on bool) {
	e.t.Helper()
	e.putBucketConfig(bucket, storage.ConfigCompression, string(compression.BucketSetting{Enabled: on}.Encode()))
}

func (e *testEnv) sdkPut(bucket, key string, data []byte, contentType string) *awss3.PutObjectOutput {
	e.t.Helper()
	in := &awss3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	out, err := e.client.PutObject(context.Background(), in)
	if err != nil {
		e.t.Fatalf("PutObject %s/%s: %v", bucket, key, err)
	}
	return out
}

// storedAs returns how the backend stores the latest version of key.
func (e *testEnv) storedAs(bucket, key string) storage.ObjectInfo {
	e.t.Helper()
	info, err := e.backend.GetObjectInfo(context.Background(), bucket, key, "")
	if err != nil {
		e.t.Fatalf("GetObjectInfo %s/%s: %v", bucket, key, err)
	}
	return info
}

func (e *testEnv) sdkGet(bucket, key, rng string) ([]byte, *awss3.GetObjectOutput) {
	e.t.Helper()
	in := &awss3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if rng != "" {
		in.Range = aws.String(rng)
	}
	out, err := e.client.GetObject(context.Background(), in)
	if err != nil {
		e.t.Fatalf("GetObject %s/%s: %v", bucket, key, err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		e.t.Fatalf("GetObject %s/%s: read: %v", bucket, key, err)
	}
	return data, out
}

func TestCompressionIsTransparent(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.enableBucketCompression("bkt", true)
	data := compressibleText(1 << 20)

	put := e.sdkPut("bkt", "data.csv", data, "text/csv")
	info := e.storedAs("bkt", "data.csv")
	if info.Compression != storage.CompressionDeflate || info.StoredSize >= info.Size/3 {
		t.Fatalf("stored as %q in %d of %d bytes", info.Compression, info.StoredSize, info.Size)
	}
	if want := `"` + md5Hex(data) + `"`; aws.ToString(put.ETag) != want {
		t.Errorf("ETag = %s, want %s (MD5 of the content)", aws.ToString(put.ETag), want)
	}

	got, out := e.sdkGet("bkt", "data.csv", "")
	if !bytes.Equal(got, data) || aws.ToInt64(out.ContentLength) != int64(len(data)) {
		t.Fatalf("GetObject: %d bytes (Content-Length %d), want %d", len(got), aws.ToInt64(out.ContentLength), len(data))
	}
	for _, r := range [][2]int{{0, 9}, {262140, 262150}, {700000, len(data) - 1}} {
		got, out := e.sdkGet("bkt", "data.csv", fmt.Sprintf("bytes=%d-%d", r[0], r[1]))
		if !bytes.Equal(got, data[r[0]:r[1]+1]) {
			t.Errorf("range %v: wrong content", r)
		}
		if want := fmt.Sprintf("bytes %d-%d/%d", r[0], r[1], len(data)); aws.ToString(out.ContentRange) != want {
			t.Errorf("Content-Range = %q, want %q", aws.ToString(out.ContentRange), want)
		}
	}
	head, err := e.client.HeadObject(context.Background(), &awss3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("data.csv")})
	if err != nil || aws.ToInt64(head.ContentLength) != int64(len(data)) {
		t.Fatalf("HeadObject = %v, %v", head, err)
	}
	list, err := e.client.ListObjectsV2(context.Background(), &awss3.ListObjectsV2Input{Bucket: aws.String("bkt")})
	if err != nil || len(list.Contents) != 1 || aws.ToInt64(list.Contents[0].Size) != int64(len(data)) {
		t.Fatalf("ListObjectsV2 = %+v, %v", list, err)
	}
}

func TestCompressionPolicy(t *testing.T) {
	text := compressibleText(64 << 10)
	for _, c := range []struct {
		name, mode  string
		bucket      *bool // nil: no setting
		key, ctype  string
		data        []byte
		wantCompres bool
	}{
		{"optional, bucket on", config.CompressionOptional, aws.Bool(true), "a.txt", "text/plain", text, true},
		{"optional, bucket off", config.CompressionOptional, aws.Bool(false), "a.txt", "text/plain", text, false},
		{"optional, no setting", config.CompressionOptional, nil, "a.txt", "text/plain", text, false},
		{"enforced, no setting", config.CompressionEnforced, nil, "a.txt", "text/plain", text, true},
		{"enforced, bucket off", config.CompressionEnforced, aws.Bool(false), "a.txt", "text/plain", text, true},
		{"disabled, bucket on", config.CompressionDisabled, aws.Bool(true), "a.txt", "text/plain", text, false},
		{"skipped type", config.CompressionEnforced, nil, "v.mp4", "video/mp4", text, false},
		{"below min size", config.CompressionEnforced, nil, "s.txt", "text/plain", text[:4095], false},
		{"min size", config.CompressionEnforced, nil, "s.txt", "text/plain", text[:4096], true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t, withCompressionMode(c.mode))
			e.createBucket("bkt")
			if c.bucket != nil {
				e.enableBucketCompression("bkt", *c.bucket)
			}
			e.sdkPut("bkt", c.key, c.data, c.ctype)
			if got := e.storedAs("bkt", c.key).Compression != ""; got != c.wantCompres {
				t.Fatalf("compressed = %v, want %v", got, c.wantCompres)
			}
			if got, _ := e.sdkGet("bkt", c.key, ""); !bytes.Equal(got, c.data) {
				t.Fatal("content differs")
			}
		})
	}
}

// TestCompressionPolicyChangesApply checks that a settings change applies
// to the next write, and that objects written under the old policy stay
// readable.
func TestCompressionPolicyChangesApply(t *testing.T) {
	e := newTestEnv(t, withCompressionMode(config.CompressionEnforced))
	e.createBucket("bkt")
	text := compressibleText(100_000)
	e.sdkPut("bkt", "old", text, "")
	if _, err := e.settings.Update(func(s *config.Settings) error {
		s.Compression.Mode = config.CompressionDisabled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.sdkPut("bkt", "new", text, "")
	if e.storedAs("bkt", "old").Compression == "" || e.storedAs("bkt", "new").Compression != "" {
		t.Fatal("the policy change did not apply to the next write only")
	}
	for _, k := range []string{"old", "new"} {
		if got, _ := e.sdkGet("bkt", k, "bytes=10-99"); !bytes.Equal(got, text[10:100]) {
			t.Fatalf("%s: wrong content", k)
		}
	}
}

func TestCompressionMultipartAndCopy(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.createBucket("zip")
	e.createBucket("raw")
	e.enableBucketCompression("zip", true)

	// Multipart: the completed object is compressed, the parts are not
	// visible as anything but the concatenation.
	p1, p2 := compressibleText(5<<20), compressibleText(1000)
	id := mpCreate(t, e.client, "zip", "mp.csv", func(in *awss3.CreateMultipartUploadInput) { in.ContentType = aws.String("text/csv") })
	o1 := mpMustUploadPart(t, e.client, "zip", "mp.csv", id, 1, p1, nil)
	o2 := mpMustUploadPart(t, e.client, "zip", "mp.csv", id, 2, p2, nil)
	if _, err := mpComplete(e.client, "zip", "mp.csv", id, []types.CompletedPart{mpPart(1, aws.ToString(o1.ETag)), mpPart(2, aws.ToString(o2.ETag))}, nil); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	whole := append(bytes.Clone(p1), p2...)
	if info := e.storedAs("zip", "mp.csv"); info.Compression == "" || info.Size != int64(len(whole)) {
		t.Fatalf("multipart object stored as %+v", info)
	}
	if got, _ := e.sdkGet("zip", "mp.csv", ""); !bytes.Equal(got, whole) {
		t.Fatal("multipart content differs")
	}
	if got, _ := e.sdkGet("zip", "mp.csv", fmt.Sprintf("bytes=%d-%d", len(p1)-5, len(p1)+5)); !bytes.Equal(got, whole[len(p1)-5:len(p1)+6]) {
		t.Fatal("multipart range across the part boundary differs")
	}

	// Copies follow the destination bucket's setting.
	for _, c := range []struct{ src, dst string }{{"zip/mp.csv", "raw"}, {"zip/mp.csv", "zip"}} {
		_, err := e.client.CopyObject(ctx, &awss3.CopyObjectInput{
			Bucket: aws.String(c.dst), Key: aws.String("copy.csv"), CopySource: aws.String(c.src),
		})
		if err != nil {
			t.Fatalf("CopyObject to %s: %v", c.dst, err)
		}
		if compressed := e.storedAs(c.dst, "copy.csv").Compression != ""; compressed != (c.dst == "zip") {
			t.Errorf("copy to %s: compressed = %v", c.dst, compressed)
		}
		if got, _ := e.sdkGet(c.dst, "copy.csv", ""); !bytes.Equal(got, whole) {
			t.Errorf("copy to %s: content differs", c.dst)
		}
	}

	// Data already compressed by the client is stored as is.
	gz := compressibleText(50_000)
	resp := e.signed(http.MethodPut, "/zip/pre.gz", gz, http.Header{"Content-Encoding": {"gzip"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT with Content-Encoding: %d %s", resp.StatusCode, resp.Data)
	}
	if e.storedAs("zip", "pre.gz").Compression != "" {
		t.Error("an object with Content-Encoding: gzip was compressed again")
	}
	if !strings.Contains(e.storedAs("zip", "pre.gz").ContentEncoding, "gzip") {
		t.Error("Content-Encoding not stored")
	}
}
