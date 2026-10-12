//lint:file-ignore SA1019 feature/s3/manager is deprecated upstream, but its successor (transfermanager) would bump the core SDK modules; it is test-only and still exercises the same wire behaviour.

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// onlyReader hides io.Seeker/io.ReaderAt so the uploader streams.
type onlyReader struct{ io.Reader }

func TestManagerUploadDownload(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			data := randomData(23<<20+11, 10) // 5 parts of 5 MiB (last one smaller)
			up := manager.NewUploader(c, func(u *manager.Uploader) {
				u.PartSize = 5 << 20
				u.Concurrency = 4
				u.LeavePartsOnError = false
			})
			for _, streaming := range []bool{false, true} {
				key := fmt.Sprintf("big/streaming=%v", streaming)
				var body io.Reader = bytes.NewReader(data)
				if streaming {
					body = onlyReader{bytes.NewReader(data)}
				}
				res, err := up.Upload(ctx(t), &s3.PutObjectInput{
					Bucket: aws.String(b), Key: aws.String(key), Body: body, ContentType: aws.String("application/x-big"),
					Metadata: map[string]string{"m": "v"},
				})
				must(t, err, "Upload")
				if res.UploadID == "" || len(res.CompletedParts) != 5 {
					t.Fatalf("upload id %q, %d parts", res.UploadID, len(res.CompletedParts))
				}
				if !strings.HasSuffix(aws.ToString(res.ETag), `-5"`) {
					t.Errorf("multipart ETag %s", aws.ToString(res.ETag))
				}
				// ETag = md5(concat(part md5s))-N.
				var cat []byte
				for off := 0; off < len(data); off += 5 << 20 {
					cat = append(cat, rawMD5(data[off:min(off+5<<20, len(data))])...)
				}
				if want := quoted(md5Hex(cat) + "-5"); aws.ToString(res.ETag) != want {
					t.Errorf("ETag %s, want %s", aws.ToString(res.ETag), want)
				}

				head, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String(key)})
				must(t, err, "HeadObject")
				if aws.ToInt64(head.ContentLength) != int64(len(data)) || aws.ToString(head.ContentType) != "application/x-big" || head.Metadata["m"] != "v" {
					t.Errorf("head: %d %q %v", aws.ToInt64(head.ContentLength), aws.ToString(head.ContentType), head.Metadata)
				}

				// Concurrent ranged download.
				dl := manager.NewDownloader(c, func(d *manager.Downloader) { d.PartSize = 3 << 20; d.Concurrency = 5 })
				buf := manager.NewWriteAtBuffer(make([]byte, 0, len(data)))
				n, err := dl.Download(ctx(t), buf, &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key)})
				must(t, err, "Download")
				if n != int64(len(data)) || !bytes.Equal(buf.Bytes(), data) {
					t.Fatalf("download: %d bytes, equal=%v", n, bytes.Equal(buf.Bytes(), data))
				}

				// GetObject partNumber.
				for pn := int32(1); pn <= 5; pn++ {
					out, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key), PartNumber: aws.Int32(pn)})
					must(t, err, "GetObject partNumber")
					got := readAll(t, out.Body)
					start := int(pn-1) * (5 << 20)
					want := data[start:min(start+5<<20, len(data))]
					if !bytes.Equal(got, want) || aws.ToInt32(out.PartsCount) != 5 {
						t.Errorf("part %d: %d bytes, parts count %d", pn, len(got), aws.ToInt32(out.PartsCount))
					}
					wantRange := fmt.Sprintf("bytes %d-%d/%d", start, start+len(want)-1, len(data))
					if aws.ToString(out.ContentRange) != wantRange {
						t.Errorf("part %d: Content-Range %q, want %q", pn, aws.ToString(out.ContentRange), wantRange)
					}
				}
				_, err = c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String(key), PartNumber: aws.Int32(6)})
				wantErr(t, err, "InvalidPartNumber", http.StatusRequestedRangeNotSatisfiable)
				hp, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String(key), PartNumber: aws.Int32(5)})
				must(t, err, "HeadObject partNumber")
				if aws.ToInt64(hp.ContentLength) != int64(len(data)-4*(5<<20)) {
					t.Errorf("HEAD part 5 length %d", aws.ToInt64(hp.ContentLength))
				}
			}
			// partNumber=1 of a simple object is the whole object.
			e.put(t, b, "simple", []byte("simple"))
			out, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("simple"), PartNumber: aws.Int32(1)})
			must(t, err, "GetObject partNumber=1 simple")
			if string(readAll(t, out.Body)) != "simple" {
				t.Error("simple partNumber=1 body")
			}
		})
	}
}

func TestMultipartListAbortErrors(t *testing.T) {
	t.Parallel()
	e := fsEnv
	c := e.root()
	b := e.newBucket(t)
	keys := []string{"a/one", "a/two", "b", "c d+e"}
	ids := map[string][]string{}
	for _, k := range keys {
		for i := 0; i < 2; i++ {
			cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String(k)})
			must(t, err, "CreateMultipartUpload")
			ids[k] = append(ids[k], aws.ToString(cr.UploadId))
		}
	}

	// Paginate ListMultipartUploads with max-uploads=3.
	type upl struct{ key, id string }
	var got []upl
	var km, um *string
	for n := 0; ; n++ {
		out, err := c.ListMultipartUploads(ctx(t), &s3.ListMultipartUploadsInput{Bucket: aws.String(b), MaxUploads: aws.Int32(3), KeyMarker: km, UploadIdMarker: um})
		must(t, err, "ListMultipartUploads")
		if len(out.Uploads) > 3 {
			t.Fatalf("%d uploads in a page", len(out.Uploads))
		}
		for _, u := range out.Uploads {
			got = append(got, upl{aws.ToString(u.Key), aws.ToString(u.UploadId)})
			if u.Initiated == nil || u.Owner == nil || u.Initiator == nil {
				t.Errorf("incomplete upload entry %+v", u)
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		km, um = out.NextKeyMarker, out.NextUploadIdMarker
		if n > 10 {
			t.Fatal("no termination")
		}
	}
	if len(got) != 8 {
		t.Fatalf("listed %d uploads: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].key > got[i].key {
			t.Errorf("uploads not sorted by key: %v", got)
		}
	}
	for _, k := range keys {
		var listed []string
		for _, u := range got {
			if u.key == k {
				listed = append(listed, u.id)
			}
		}
		if !slices.Equal(sortedCopy(listed), sortedCopy(ids[k])) {
			t.Errorf("uploads of %q: %v, want %v", k, listed, ids[k])
		}
	}
	// Delimiter + prefix.
	out, err := c.ListMultipartUploads(ctx(t), &s3.ListMultipartUploadsInput{Bucket: aws.String(b), Delimiter: aws.String("/")})
	must(t, err, "ListMultipartUploads delimiter")
	if len(out.CommonPrefixes) != 1 || aws.ToString(out.CommonPrefixes[0].Prefix) != "a/" || len(out.Uploads) != 4 {
		t.Errorf("delimiter: prefixes %v, %d uploads", out.CommonPrefixes, len(out.Uploads))
	}
	out, err = c.ListMultipartUploads(ctx(t), &s3.ListMultipartUploadsInput{Bucket: aws.String(b), Prefix: aws.String("a/")})
	must(t, err, "ListMultipartUploads prefix")
	if len(out.Uploads) != 4 {
		t.Errorf("prefix a/: %d uploads", len(out.Uploads))
	}

	// ListParts pagination.
	k, id := "b", ids["b"][0]
	var etags []string
	for pn := int32(1); pn <= 5; pn++ {
		up, err := c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String(k), UploadId: aws.String(id),
			PartNumber: aws.Int32(pn), Body: bytes.NewReader(randomData(5<<20, byte(pn)))})
		must(t, err, "UploadPart")
		etags = append(etags, aws.ToString(up.ETag))
	}
	var parts []int32
	var marker *string
	for {
		lp, err := c.ListParts(ctx(t), &s3.ListPartsInput{Bucket: aws.String(b), Key: aws.String(k), UploadId: aws.String(id), MaxParts: aws.Int32(2), PartNumberMarker: marker})
		must(t, err, "ListParts")
		for _, p := range lp.Parts {
			parts = append(parts, aws.ToInt32(p.PartNumber))
			if aws.ToString(p.ETag) != etags[aws.ToInt32(p.PartNumber)-1] || aws.ToInt64(p.Size) != 5<<20 {
				t.Errorf("part %d: etag %s size %d", aws.ToInt32(p.PartNumber), aws.ToString(p.ETag), aws.ToInt64(p.Size))
			}
		}
		if !aws.ToBool(lp.IsTruncated) {
			break
		}
		marker = lp.NextPartNumberMarker
	}
	if !slices.Equal(parts, []int32{1, 2, 3, 4, 5}) {
		t.Errorf("ListParts = %v", parts)
	}

	// Completion errors.
	complete := func(ps ...s3types.CompletedPart) error {
		_, err := c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String(k), UploadId: aws.String(id),
			MultipartUpload: &s3types.CompletedMultipartUpload{Parts: ps}})
		return err
	}
	part := func(n int32, etag string) s3types.CompletedPart {
		return s3types.CompletedPart{PartNumber: aws.Int32(n), ETag: aws.String(etag)}
	}
	wantErr(t, complete(part(2, etags[1]), part(1, etags[0])), "InvalidPartOrder", 400)
	wantErr(t, complete(part(1, etags[0]), part(2, `"00000000000000000000000000000000"`)), "InvalidPart", 400)
	wantErr(t, complete(part(1, etags[0]), part(7, etags[1])), "InvalidPart", 400)
	_, err = c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String(k), UploadId: aws.String(id)})
	if apiCode(err) != "MalformedXML" && apiCode(err) != "InvalidRequest" {
		t.Errorf("complete without body: %v", err)
	}
	// A small part that is not the last one.
	small, err := c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String(k), UploadId: aws.String(id),
		PartNumber: aws.Int32(6), Body: bytes.NewReader([]byte("tiny"))})
	must(t, err, "UploadPart small")
	if len(aws.ToString(small.ETag)) != 34 {
		t.Errorf("small part ETag %s", aws.ToString(small.ETag))
	}
	// Completing with a subset of the parts.
	must(t, complete(part(1, etags[0]), part(2, etags[1])), "complete subset of parts")
	if got := e.get(t, b, k); len(got) != 10<<20 {
		t.Errorf("completed object has %d bytes", len(got))
	}
	// Parts not listed are discarded; the upload is gone.
	_, err = c.ListParts(ctx(t), &s3.ListPartsInput{Bucket: aws.String(b), Key: aws.String(k), UploadId: aws.String(id)})
	wantErr(t, err, "NoSuchUpload", http.StatusNotFound)

	// Undersized non-last part: EntityTooSmall.
	cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("small")})
	must(t, err, "CreateMultipartUpload small")
	var sp []s3types.CompletedPart
	for pn := int32(1); pn <= 2; pn++ {
		up, err := c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String("small"), UploadId: cr.UploadId,
			PartNumber: aws.Int32(pn), Body: bytes.NewReader([]byte("small part"))})
		must(t, err, "UploadPart")
		sp = append(sp, s3types.CompletedPart{PartNumber: aws.Int32(pn), ETag: up.ETag})
	}
	_, err = c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("small"), UploadId: cr.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: sp}})
	wantErr(t, err, "EntityTooSmall", 400)

	// Abort: the upload disappears, and further use is NoSuchUpload.
	abortID := ids["c d+e"][0]
	_, err = c.AbortMultipartUpload(ctx(t), &s3.AbortMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("c d+e"), UploadId: aws.String(abortID)})
	must(t, err, "AbortMultipartUpload")
	_, err = c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String("c d+e"), UploadId: aws.String(abortID),
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("x"))})
	wantErr(t, err, "NoSuchUpload", http.StatusNotFound)
	_, err = c.AbortMultipartUpload(ctx(t), &s3.AbortMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("c d+e"), UploadId: aws.String(abortID)})
	wantErr(t, err, "NoSuchUpload", http.StatusNotFound)
	// Upload ID used with another key.
	_, err = c.ListParts(ctx(t), &s3.ListPartsInput{Bucket: aws.String(b), Key: aws.String("other"), UploadId: aws.String(ids["a/one"][0])})
	wantErr(t, err, "NoSuchUpload", http.StatusNotFound)
	// Invalid part numbers.
	for _, pn := range []int32{0, 10001} {
		_, err = c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String("a/one"), UploadId: aws.String(ids["a/one"][0]),
			PartNumber: aws.Int32(pn), Body: bytes.NewReader([]byte("x"))})
		wantErr(t, err, "InvalidArgument", 400)
	}
}

func TestUploadPartCopy(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			src := randomData(12<<20, 11)
			e.put(t, b, "source obj", src)
			cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("assembled")})
			must(t, err, "CreateMultipartUpload")
			// Parts 1, 2 and 4 are ranges; part 3 copies the whole object.
			ranges := []string{"bytes=0-5242879", "bytes=5242880-10485759", "", "bytes=10485760-12582911"}
			var parts []s3types.CompletedPart
			for i, r := range ranges {
				in := &s3.UploadPartCopyInput{
					Bucket: aws.String(b), Key: aws.String("assembled"), UploadId: cr.UploadId, PartNumber: aws.Int32(int32(i + 1)),
					CopySource: aws.String(b + "/source%20obj"),
				}
				if r != "" {
					in.CopySourceRange = aws.String(r)
				}
				out, err := c.UploadPartCopy(ctx(t), in)
				must(t, err, "UploadPartCopy")
				if out.CopyPartResult == nil || aws.ToString(out.CopyPartResult.ETag) == "" || out.CopyPartResult.LastModified == nil {
					t.Fatalf("CopyPartResult %+v", out.CopyPartResult)
				}
				parts = append(parts, s3types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.CopyPartResult.ETag})
			}
			// Conditional copy failure and an invalid range.
			_, err = c.UploadPartCopy(ctx(t), &s3.UploadPartCopyInput{Bucket: aws.String(b), Key: aws.String("assembled"), UploadId: cr.UploadId,
				PartNumber: aws.Int32(5), CopySource: aws.String(b + "/source%20obj"), CopySourceIfMatch: aws.String(`"nope"`)})
			wantErr(t, err, "PreconditionFailed", http.StatusPreconditionFailed)
			_, err = c.UploadPartCopy(ctx(t), &s3.UploadPartCopyInput{Bucket: aws.String(b), Key: aws.String("assembled"), UploadId: cr.UploadId,
				PartNumber: aws.Int32(5), CopySource: aws.String(b + "/source%20obj"), CopySourceRange: aws.String("bytes=0-99999999")})
			// AWS answers 400 InvalidArgument "Range specified is not valid for
			// source object of size: N" (not 416 InvalidRange as GetObject does).
			wantErr(t, err, "InvalidArgument", http.StatusBadRequest)

			_, err = c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String("assembled"), UploadId: cr.UploadId,
				MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts}})
			must(t, err, "CompleteMultipartUpload")
			want := slices.Concat(src[:10<<20], src, src[10<<20:])
			if got := e.get(t, b, "assembled"); !bytes.Equal(got, want) {
				t.Fatalf("assembled object differs (%d bytes)", len(got))
			}
		})
	}
}

// TestMultipartConditionalComplete checks If-None-Match on completion.
func TestMultipartConditionalComplete(t *testing.T) {
	t.Parallel()
	e := memEnv
	c := e.root()
	b := e.newBucket(t)
	e.put(t, b, "exists", []byte("x"))
	for _, key := range []string{"exists", "fresh"} {
		cr, err := c.CreateMultipartUpload(ctx(t), &s3.CreateMultipartUploadInput{Bucket: aws.String(b), Key: aws.String(key)})
		must(t, err, "CreateMultipartUpload")
		up, err := c.UploadPart(ctx(t), &s3.UploadPartInput{Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId,
			PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("part"))})
		must(t, err, "UploadPart")
		_, err = c.CompleteMultipartUpload(ctx(t), &s3.CompleteMultipartUploadInput{Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId,
			IfNoneMatch:     aws.String("*"),
			MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: up.ETag}}}})
		if key == "exists" {
			wantErr(t, err, "PreconditionFailed", http.StatusPreconditionFailed)
			// The upload is kept after a failed precondition.
			_, err = c.ListParts(ctx(t), &s3.ListPartsInput{Bucket: aws.String(b), Key: aws.String(key), UploadId: cr.UploadId})
			must(t, err, "ListParts after failed completion")
		} else {
			must(t, err, "conditional completion")
		}
	}
}
