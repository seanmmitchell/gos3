package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/minio/minio-go/v7"
)

// genReader produces n deterministic pseudo-random bytes without holding
// them in memory, hashing what it produced.
type genReader struct {
	remaining int64
	x         uint64
	h         hash.Hash
}

func newGenReader(n int64, seed uint64) *genReader {
	return &genReader{remaining: n, x: seed*0x9E3779B97F4A7C15 + 1, h: sha256.New()}
}

func (g *genReader) Read(p []byte) (int, error) {
	if g.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.remaining {
		p = p[:g.remaining]
	}
	n := len(p) &^ 7
	if n == 0 {
		n = len(p)
	}
	for i := 0; i < n; i += 8 {
		g.x ^= g.x << 13
		g.x ^= g.x >> 7
		g.x ^= g.x << 17
		v := g.x
		for j := 0; j < 8 && i+j < n; j++ {
			p[i+j] = byte(v >> (8 * j))
		}
	}
	g.h.Write(p[:n])
	g.remaining -= int64(n)
	return n, nil
}

// digest returns the SHA-256 of everything read so far.
func (g *genReader) digest() []byte { return g.h.Sum(nil) }

// readDigest drains r and returns its size and SHA-256.
func readDigest(t *testing.T, r io.Reader) (int64, []byte) {
	t.Helper()
	h := sha256.New()
	n, err := io.Copy(h, r)
	must(t, err, "reading object")
	return n, h.Sum(nil)
}

const largeSize = 100 << 20

// TestLargeStreamedObjectSDK streams a 100 MiB object of unknown content
// (non-seekable body) through the SDK over HTTPS: aws-chunked with an
// unsigned SHA-256 trailer.
func TestLargeStreamedObjectSDK(t *testing.T) {
	if testing.Short() {
		t.Skip("100 MiB transfer skipped in -short mode")
	}
	e := memEnv
	hc, rec := e.recordingClient()
	c := e.root(func(o *s3.Options) { o.HTTPClient = hc })
	b := e.newBucket(t)
	gen := newGenReader(largeSize, 1)
	out, err := c.PutObject(ctx(t), &s3.PutObjectInput{
		Bucket: aws.String(b), Key: aws.String("large.bin"), Body: onlyReader{gen},
		ContentLength: aws.Int64(largeSize), ChecksumAlgorithm: s3types.ChecksumAlgorithmSha256,
	})
	must(t, err, "PutObject 100 MiB")
	sum := gen.digest()
	if got := aws.ToString(out.ChecksumSHA256); got != base64.StdEncoding.EncodeToString(sum) {
		t.Errorf("checksum %q", got)
	}
	sent := rec.last(http.MethodPut)
	if sent.header.Get("X-Amz-Content-Sha256") != "STREAMING-UNSIGNED-PAYLOAD-TRAILER" || sent.header.Get("X-Amz-Decoded-Content-Length") != fmt.Sprint(largeSize) {
		t.Errorf("upload encoding: %v", sent.header)
	}
	get, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("large.bin"), ChecksumMode: s3types.ChecksumModeEnabled})
	must(t, err, "GetObject 100 MiB")
	defer get.Body.Close()
	n, got := readDigest(t, get.Body)
	if n != largeSize || !bytes.Equal(got, sum) {
		t.Fatalf("read back %d bytes, digest equal %v", n, bytes.Equal(got, sum))
	}
	// A ranged read in the middle.
	rg, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("large.bin"), Range: aws.String("bytes=52428800-52428899")})
	must(t, err, "ranged GetObject")
	if data := readAll(t, rg.Body); len(data) != 100 {
		t.Errorf("range length %d", len(data))
	}
}

// TestLargeStreamedObjectMinio streams 100 MiB in one PUT with minio-go's
// signed aws-chunked encoding and a CRC32C trailer to the fs backend.
func TestLargeStreamedObjectMinio(t *testing.T) {
	if testing.Short() {
		t.Skip("100 MiB transfer skipped in -short mode")
	}
	e := fsEnv
	mc, rec := e.minioRecording(t, true)
	b := e.newBucket(t)
	gen := newGenReader(largeSize, 2)
	info, err := mc.PutObject(ctx(t), b, "large.bin", onlyReader{gen}, largeSize, minio.PutObjectOptions{
		DisableMultipart: true, Checksum: minio.ChecksumCRC32C,
	})
	must(t, err, "minio PutObject 100 MiB")
	if info.Size != largeSize {
		t.Errorf("size %d", info.Size)
	}
	if mode := rec.last(http.MethodPut).header.Get("X-Amz-Content-Sha256"); mode != "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER" {
		t.Errorf("payload mode %q", mode)
	}
	sum := gen.digest()
	obj, err := mc.GetObject(ctx(t), b, "large.bin", minio.GetObjectOptions{})
	must(t, err, "GetObject")
	defer obj.Close()
	n, got := readDigest(t, obj)
	if n != largeSize || !bytes.Equal(got, sum) {
		t.Fatalf("read back %d bytes, digest equal %v", n, bytes.Equal(got, sum))
	}
}

// TestConcurrency runs many goroutines doing mixed operations on distinct
// and shared keys and checks the results are consistent.
func TestConcurrency(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newVersionedBucket(t)
			const workers, ops = 24, 15
			var wg sync.WaitGroup
			var failures atomic.Int64
			fail := func(format string, args ...any) {
				failures.Add(1)
				t.Errorf(format, args...)
			}
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < ops; i++ {
						key := fmt.Sprintf("w%02d/k%02d", w, i)
						data := randomData(1000+w*37+i, byte(w*ops+i))
						if _, err := c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
							fail("put %s: %v", key, err)
							continue
						}
						out, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key)})
						if err != nil {
							fail("get %s: %v", key, err)
							continue
						}
						got, err := io.ReadAll(out.Body)
						out.Body.Close()
						if err != nil || !bytes.Equal(got, data) {
							fail("get %s: data mismatch (%v)", key, err)
						}
						// Everyone also hammers one shared key.
						if _, err := c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("shared"),
							Body: bytes.NewReader([]byte(fmt.Sprintf("%d-%d", w, i)))}); err != nil {
							fail("put shared: %v", err)
						}
						if i%5 == 4 {
							if _, err := c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String(key)}); err != nil {
								fail("delete %s: %v", key, err)
							}
						}
						if i%3 == 0 {
							if _, err := c.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(b), Prefix: aws.String(fmt.Sprintf("w%02d/", w))}); err != nil {
								fail("list: %v", err)
							}
						}
					}
				}(w)
			}
			wg.Wait()
			if failures.Load() > 0 {
				t.FailNow()
			}
			// Final state: each worker has ops - ops/5 live keys.
			var keys int
			p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(b), Prefix: aws.String("w")})
			for p.HasMorePages() {
				out, err := p.NextPage(ctx(t))
				must(t, err, "list page")
				keys += len(out.Contents)
			}
			if want := workers * (ops - ops/5); keys != want {
				t.Errorf("%d live keys, want %d", keys, want)
			}
			// The shared key has one version per write, exactly one latest.
			var versions, latest int
			vp := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: aws.String(b), Prefix: aws.String("shared")})
			for vp.HasMorePages() {
				out, err := vp.NextPage(ctx(t))
				must(t, err, "versions page")
				for _, v := range out.Versions {
					versions++
					if aws.ToBool(v.IsLatest) {
						latest++
					}
				}
			}
			if versions != workers*ops || latest != 1 {
				t.Errorf("shared key: %d versions (%d latest), want %d (1)", versions, latest, workers*ops)
			}

			// Concurrent multipart uploads of the same key: the last completion wins
			// and the object is one of the uploads, intact.
			const uploads = 6
			payloads := make([][]byte, uploads)
			var mwg sync.WaitGroup
			for u := 0; u < uploads; u++ {
				payloads[u] = randomData(5<<20+u, byte(100+u))
				mwg.Add(1)
				go func(u int) {
					defer mwg.Done()
					cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("mp-shared")})
					if err != nil {
						fail("create: %v", err)
						return
					}
					up, err := c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String("mp-shared"), UploadId: cr.UploadId,
						PartNumber: aws.Int32(1), Body: bytes.NewReader(payloads[u])})
					if err != nil {
						fail("upload part: %v", err)
						return
					}
					if _, err := c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("mp-shared"), UploadId: cr.UploadId,
						MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: up.ETag}}}}); err != nil {
						fail("complete: %v", err)
					}
				}(u)
			}
			mwg.Wait()
			got := e.get(t, b, "mp-shared")
			found := false
			for _, p := range payloads {
				found = found || bytes.Equal(got, p)
			}
			if !found {
				t.Errorf("mp-shared content (%d bytes) is none of the uploads", len(got))
			}
			lu, err := c.ListMultipartUploads(ctx(t), &s3.ListMultipartUploadsInput{Bucket: aws.String(b)})
			must(t, err, "ListMultipartUploads")
			if len(lu.Uploads) != 0 {
				t.Errorf("%d uploads left", len(lu.Uploads))
			}
		})
	}
}
