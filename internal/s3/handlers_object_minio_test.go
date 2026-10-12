package s3

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// objMinio returns a minio-go client for the test server (root).
func objMinio(t *testing.T, e *testEnv) *minio.Client {
	t.Helper()
	u, err := url.Parse(e.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:           miniocreds.NewStaticV4(testRoot.AccessKey, testRoot.SecretKey, ""),
		Secure:          u.Scheme == "https",
		Region:          "us-east-1",
		Transport:       e.httpc.Transport,
		TrailingHeaders: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestObjectMinioClient drives the object handlers with minio-go, which
// uploads with signed aws-chunked streaming payloads (with trailing
// checksums) over plain HTTP.
func TestObjectMinioClient(t *testing.T) {
	for _, tls := range []bool{false, true} {
		var opts []testEnvOption
		if tls {
			opts = append(opts, withTLS())
		}
		e := newTestEnv(t, opts...)
		e.createBucket("bkt")
		mc := objMinio(t, e)
		ctx := context.Background()
		data := bytes.Repeat([]byte("minio-payload-"), 40000) // > 512 KiB: several chunks

		for _, ck := range []minio.ChecksumType{minio.ChecksumNone, minio.ChecksumCRC32C, minio.ChecksumSHA256, minio.ChecksumCRC64NVME} {
			info, err := mc.PutObject(ctx, "bkt", "obj", bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
				ContentType: "application/x-test", UserMetadata: map[string]string{"Origin": "minio"},
				UserTags: map[string]string{"src": "minio"}, Checksum: ck,
			})
			if err != nil {
				t.Fatalf("tls=%v %v: PutObject: %v", tls, ck, err)
			}
			if info.ETag != md5Hex(data) {
				t.Errorf("tls=%v %v: ETag %s", tls, ck, info.ETag)
			}
			st := objGetInfo(t, e, "bkt", "obj", "")
			if st.Size != int64(len(data)) || st.ETag != md5Hex(data) || st.UserMeta["origin"] != "minio" || st.Tags["src"] != "minio" {
				t.Errorf("tls=%v %v: stored %+v", tls, ck, st.ObjectMeta)
			}
			if ck.IsSet() {
				if algo := ck.String(); st.Checksums[algo] != computeChecksum(algo, data) {
					t.Errorf("tls=%v %v: checksums %v", tls, ck, st.Checksums)
				}
			}
		}

		obj, err := mc.GetObject(ctx, "bkt", "obj", minio.GetObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(obj)
		obj.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("GetObject: %v (%d bytes)", err, len(got))
		}
		ro := minio.GetObjectOptions{}
		ro.SetRange(10, 19)
		obj, err = mc.GetObject(ctx, "bkt", "obj", ro)
		if err != nil {
			t.Fatal(err)
		}
		got, _ = io.ReadAll(obj)
		obj.Close()
		if !bytes.Equal(got, data[10:20]) {
			t.Errorf("range read %q", got)
		}
		st, err := mc.StatObject(ctx, "bkt", "obj", minio.StatObjectOptions{})
		if err != nil || st.Size != int64(len(data)) || st.ContentType != "application/x-test" || st.UserMetadata["Origin"] != "minio" {
			t.Errorf("StatObject: %v %+v", err, st)
		}

		// Copy, tagging and bulk delete.
		if _, err := mc.CopyObject(ctx, minio.CopyDestOptions{Bucket: "bkt", Object: "copy"}, minio.CopySrcOptions{Bucket: "bkt", Object: "obj"}); err != nil {
			t.Fatalf("CopyObject: %v", err)
		}
		tg, _ := tags.NewTags(map[string]string{"x": "1"}, true)
		if err := mc.PutObjectTagging(ctx, "bkt", "copy", tg, minio.PutObjectTaggingOptions{}); err != nil {
			t.Fatalf("PutObjectTagging: %v", err)
		}
		gt, err := mc.GetObjectTagging(ctx, "bkt", "copy", minio.GetObjectTaggingOptions{})
		if err != nil || gt.ToMap()["x"] != "1" {
			t.Errorf("GetObjectTagging: %v %v", err, gt)
		}
		ch := make(chan minio.ObjectInfo, 3)
		ch <- minio.ObjectInfo{Key: "obj"}
		ch <- minio.ObjectInfo{Key: "copy"}
		ch <- minio.ObjectInfo{Key: "never-existed"}
		close(ch)
		for re := range mc.RemoveObjects(ctx, "bkt", ch, minio.RemoveObjectsOptions{}) {
			t.Errorf("RemoveObjects: %+v", re)
		}
		if _, err := mc.StatObject(ctx, "bkt", "obj", minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
			t.Errorf("after RemoveObjects: %v", err)
		}
	}
}

// TestObjectMinioPresignedPost uploads through a minio-go presigned POST
// policy, as a browser would.
func TestObjectMinioPresignedPost(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	mc := objMinio(t, e)
	p := minio.NewPostPolicy()
	_ = p.SetBucket("bkt")
	_ = p.SetKey("forms/upload.bin")
	_ = p.SetExpires(time.Now().UTC().Add(10 * time.Minute))
	_ = p.SetContentType("application/octet-stream")
	_ = p.SetContentLengthRange(1, 1<<20)
	_ = p.SetUserMetadata("who", "browser")
	u, form, err := mc.PresignedPostPolicy(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var fields []objPostField
	for k, v := range form {
		fields = append(fields, objPostField{k, v})
	}
	file := []byte("posted by minio-go policy")
	body, ct := objPostBody(t, fields, "upload.bin", file)
	req, _ := http.NewRequest("POST", u.String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	resp := e.do(req)
	if resp.StatusCode != 204 {
		t.Fatalf("POST: %d %s", resp.StatusCode, resp.Data)
	}
	info := objGetInfo(t, e, "bkt", "forms/upload.bin", "")
	if info.Size != int64(len(file)) || info.ContentType != "application/octet-stream" || info.UserMeta["who"] != "browser" {
		t.Errorf("stored %+v", info.ObjectMeta)
	}
	if !strings.Contains(resp.Header.Get("Location"), "/bkt/forms/upload.bin") {
		t.Errorf("Location %q", resp.Header.Get("Location"))
	}
}
