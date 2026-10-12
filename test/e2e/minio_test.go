package e2e

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	miniocreds "github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// minioRecording returns a root minio-go client that records its requests.
func (e *env) minioRecording(t *testing.T, trailing bool) (*minio.Client, *recordingTransport) {
	t.Helper()
	hc, rec := e.recordingClient()
	mc, err := minio.New(e.addr, &minio.Options{
		Creds: mcCredsFor(rootAccess, rootSecret), Secure: e.tls, Region: region, Transport: hc.Transport,
		BucketLookup: minio.BucketLookupPath, TrailingHeaders: trailing, MaxRetries: 1,
	})
	must(t, err, "minio.New")
	return mc, rec
}

func mcCredsFor(ak, sk string) *miniocreds.Credentials { return miniocreds.NewStaticV4(ak, sk, "") }

// TestMinioStreamingUploads checks the payload encodings minio-go uses:
// over HTTP signed aws-chunked (STREAMING-AWS4-HMAC-SHA256-PAYLOAD, with
// -TRAILER when trailing checksums are enabled), over HTTPS unsigned
// payloads/trailers; both for single PUTs and streamed multipart uploads
// of unknown size.
func TestMinioStreamingUploads(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		for _, trailing := range []bool{false, true} {
			name := e.name + map[bool]string{false: "/plain", true: "/trailing"}[trailing]
			t.Run(name, func(t *testing.T) {
				mc, rec := e.minioRecording(t, trailing)
				b := e.newBucket(t)
				data := randomData(3<<20+5, 30)
				info, err := mc.PutObject(ctx(t), b, "single", bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
					ContentType: "application/x-minio", UserMetadata: map[string]string{"k": "v"}, SendContentMd5: false,
					DisableMultipart: true, Checksum: minioChecksum(trailing),
				})
				must(t, err, "PutObject")
				if info.Size != int64(len(data)) || info.ETag != md5Hex(data) {
					t.Errorf("PutObject info: size %d etag %s", info.Size, info.ETag)
				}
				sent := rec.last(http.MethodPut)
				mode := sent.header.Get("X-Amz-Content-Sha256")
				switch {
				case !e.tls && !trailing && mode != "STREAMING-AWS4-HMAC-SHA256-PAYLOAD":
					t.Errorf("HTTP payload mode %q", mode)
				case !e.tls && trailing && mode != "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER":
					t.Errorf("HTTP trailing payload mode %q", mode)
				case e.tls && trailing && mode != "STREAMING-UNSIGNED-PAYLOAD-TRAILER":
					t.Errorf("HTTPS trailing payload mode %q", mode)
				}
				t.Logf("single PUT payload mode %s (content-encoding %q, trailer %q)", mode, sent.header.Get("Content-Encoding"), sent.header.Get("X-Amz-Trailer"))

				obj, err := mc.GetObject(ctx(t), b, "single", minio.GetObjectOptions{})
				must(t, err, "GetObject")
				got, err := io.ReadAll(obj)
				must(t, err, "read")
				if !bytes.Equal(got, data) {
					t.Fatal("single object differs")
				}
				st, err := obj.Stat()
				must(t, err, "Stat")
				if st.ContentType != "application/x-minio" || st.UserMetadata["K"] != "v" {
					t.Errorf("stat: %q %v", st.ContentType, st.UserMetadata)
				}
				// ReadAt / Seek ranges.
				buf := make([]byte, 10)
				n, err := obj.ReadAt(buf, 1000)
				if err != nil || n != 10 || !bytes.Equal(buf, data[1000:1010]) {
					t.Errorf("ReadAt: %d %v", n, err)
				}
				obj.Close()

				// Unknown size: minio-go streams a multipart upload.
				big := randomData(17<<20+3, 31)
				info, err = mc.PutObject(ctx(t), b, "streamed", onlyReader{bytes.NewReader(big)}, -1, minio.PutObjectOptions{
					// minio-go v7.3.0 shares one checksum hasher between its
					// concurrent stream workers (a data race in the client), so
					// parallel streaming is only used without checksums.
					PartSize: 5 << 20, Checksum: minioChecksum(trailing), ConcurrentStreamParts: !trailing, NumThreads: 3,
				})
				must(t, err, "PutObject unknown size")
				if info.Size != int64(len(big)) || !strings.HasSuffix(info.ETag, "-4") {
					t.Errorf("streamed upload info: size %d etag %s", info.Size, info.ETag)
				}
				obj, err = mc.GetObject(ctx(t), b, "streamed", minio.GetObjectOptions{})
				must(t, err, "GetObject streamed")
				got, err = io.ReadAll(obj)
				must(t, err, "read streamed")
				obj.Close()
				if !bytes.Equal(got, big) {
					t.Fatal("streamed object differs")
				}
			})
		}
	}
}

func minioChecksum(on bool) minio.ChecksumType {
	if on {
		return minio.ChecksumCRC32C
	}
	return minio.ChecksumNone
}

func TestMinioOperations(t *testing.T) {
	t.Parallel()
	e := fsEnv
	mc := e.minioClient(t, rootCreds, true)
	b := bucketName(t)
	must(t, mc.MakeBucket(ctx(t), b, minio.MakeBucketOptions{Region: region}), "MakeBucket")
	ok, err := mc.BucketExists(ctx(t), b)
	if err != nil || !ok {
		t.Fatalf("BucketExists: %v %v", ok, err)
	}
	if ok, err := mc.BucketExists(ctx(t), "no-such-bucket-minio"); err != nil || ok {
		t.Errorf("BucketExists missing: %v %v", ok, err)
	}
	keys := []string{"a/1", "a/2", "b/ü 3", "c"}
	for _, k := range keys {
		_, err := mc.PutObject(ctx(t), b, k, strings.NewReader(k), int64(len(k)), minio.PutObjectOptions{})
		must(t, err, "PutObject "+k)
	}
	var listed []string
	for o := range mc.ListObjects(ctx(t), b, minio.ListObjectsOptions{Recursive: true, MaxKeys: 2}) {
		must(t, o.Err, "ListObjects")
		listed = append(listed, o.Key)
	}
	if !slices.Equal(listed, keys) {
		t.Errorf("recursive listing %v", listed)
	}
	listed = nil
	for o := range mc.ListObjects(ctx(t), b, minio.ListObjectsOptions{}) {
		must(t, o.Err, "ListObjects")
		listed = append(listed, o.Key)
	}
	// minio-go yields a page's objects before its common prefixes.
	slices.Sort(listed)
	if !slices.Equal(listed, []string{"a/", "b/", "c"}) {
		t.Errorf("delimited listing %v", listed)
	}
	listed = nil
	for o := range mc.ListObjects(ctx(t), b, minio.ListObjectsOptions{Recursive: true, UseV1: true, MaxKeys: 1}) {
		must(t, o.Err, "ListObjects V1")
		listed = append(listed, o.Key)
	}
	if !slices.Equal(listed, keys) {
		t.Errorf("V1 listing %v", listed)
	}

	// Compose (server-side multipart copy with UploadPartCopy).
	part := randomData(5<<20, 32)
	_, err = mc.PutObject(ctx(t), b, "part", bytes.NewReader(part), int64(len(part)), minio.PutObjectOptions{})
	must(t, err, "PutObject part")
	_, err = mc.ComposeObject(ctx(t), minio.CopyDestOptions{Bucket: b, Object: "composed"},
		minio.CopySrcOptions{Bucket: b, Object: "part"}, minio.CopySrcOptions{Bucket: b, Object: "part"}, minio.CopySrcOptions{Bucket: b, Object: "c"})
	must(t, err, "ComposeObject")
	obj, err := mc.GetObject(ctx(t), b, "composed", minio.GetObjectOptions{})
	must(t, err, "GetObject composed")
	got, err := io.ReadAll(obj)
	must(t, err, "read composed")
	if want := slices.Concat(part, part, []byte("c")); !bytes.Equal(got, want) {
		t.Errorf("composed object: %d bytes", len(got))
	}

	// Tagging.
	tg, err := tags.NewTags(map[string]string{"t1": "v1", "t2": "v 2"}, true)
	must(t, err, "NewTags")
	must(t, mc.PutObjectTagging(ctx(t), b, "c", tg, minio.PutObjectTaggingOptions{}), "PutObjectTagging")
	gt, err := mc.GetObjectTagging(ctx(t), b, "c", minio.GetObjectTaggingOptions{})
	must(t, err, "GetObjectTagging")
	if m := gt.ToMap(); m["t1"] != "v1" || m["t2"] != "v 2" || len(m) != 2 {
		t.Errorf("tags %v", m)
	}

	// Versioning + versioned listing.
	must(t, mc.EnableVersioning(ctx(t), b), "EnableVersioning")
	_, err = mc.PutObject(ctx(t), b, "c", strings.NewReader("c2"), 2, minio.PutObjectOptions{})
	must(t, err, "PutObject c2")
	must(t, mc.RemoveObject(ctx(t), b, "c", minio.RemoveObjectOptions{}), "RemoveObject")
	var versions, markers int
	for o := range mc.ListObjects(ctx(t), b, minio.ListObjectsOptions{Prefix: "c", WithVersions: true, Recursive: true}) {
		must(t, o.Err, "ListObjects versions")
		if o.Key != "c" {
			continue
		}
		if o.IsDeleteMarker {
			markers++
		} else {
			versions++
		}
	}
	if versions != 2 || markers != 1 {
		t.Errorf("versions %d markers %d", versions, markers)
	}

	// RemoveObjects (DeleteObjects) of everything with versions.
	ch := make(chan minio.ObjectInfo)
	go func() {
		defer close(ch)
		for o := range mc.ListObjects(ctx(t), b, minio.ListObjectsOptions{WithVersions: true, Recursive: true}) {
			ch <- o
		}
	}()
	for re := range mc.RemoveObjects(ctx(t), b, ch, minio.RemoveObjectsOptions{}) {
		t.Errorf("RemoveObjects error: %v", re.Err)
	}
	must(t, mc.RemoveBucket(ctx(t), b), "RemoveBucket")
}

// TestMinioErrors checks error responses as minio-go decodes them.
func TestMinioErrors(t *testing.T) {
	t.Parallel()
	e := memEnv
	mc := e.minioClient(t, rootCreds, false)
	b := e.newBucket(t)
	_, err := mc.StatObject(ctx(t), b, "missing", minio.StatObjectOptions{})
	if er := minio.ToErrorResponse(err); er.Code != "NoSuchKey" || er.StatusCode != 404 {
		t.Errorf("StatObject missing: %+v", er)
	}
	_, err = mc.GetObjectACL(ctx(t), "no-such-bucket-minio", "x")
	if er := minio.ToErrorResponse(err); er.Code != "NoSuchBucket" {
		t.Errorf("missing bucket: %+v", er)
	}
	bad, err := minio.New(e.addr, &minio.Options{Creds: mcCredsFor("nobody", "nobody-secret"), Secure: true, Region: region,
		Transport: e.httpc.Transport, BucketLookup: minio.BucketLookupPath, MaxRetries: 1})
	must(t, err, "minio.New")
	_, err = bad.ListBuckets(ctx(t))
	if er := minio.ToErrorResponse(err); er.Code != "InvalidAccessKeyId" || er.StatusCode != 403 {
		t.Errorf("unknown key: %+v", er)
	}
}
