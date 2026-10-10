package e2e

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func md5Hex(b []byte) string {
	s := md5.Sum(b)
	return hex.EncodeToString(s[:])
}

func quoted(s string) string { return `"` + s + `"` }

func TestObjectPutGetHeadDelete(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			data := randomData(1<<20+17, 1)
			expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
			put, err := c.PutObject(ctx(t), &s3.PutObjectInput{
				Bucket: aws.String(b), Key: aws.String("obj/one.bin"), Body: bytes.NewReader(data),
				ContentType:        aws.String("application/x-test"),
				CacheControl:       aws.String("max-age=60"),
				ContentDisposition: aws.String(`attachment; filename="one.bin"`),
				ContentEncoding:    aws.String("identity"),
				ContentLanguage:    aws.String("en"),
				Expires:            aws.Time(expires),
				Metadata:           map[string]string{"color": "blue", "multi-word": "Hello World"},
				Tagging:            aws.String("k1=v1&k2=v%202"),
			})
			must(t, err, "PutObject")
			if got := aws.ToString(put.ETag); got != quoted(md5Hex(data)) {
				t.Errorf("ETag = %s, want %s", got, quoted(md5Hex(data)))
			}
			if put.VersionId != nil {
				t.Errorf("unversioned bucket returned version id %q", aws.ToString(put.VersionId))
			}

			check := func(what string, ct, cc, cd, ce, cl *string, exp *string, meta map[string]string, etag *string, size *int64, lm *time.Time) {
				t.Helper()
				if aws.ToString(ct) != "application/x-test" || aws.ToString(cc) != "max-age=60" ||
					aws.ToString(cd) != `attachment; filename="one.bin"` || aws.ToString(ce) != "identity" || aws.ToString(cl) != "en" {
					t.Errorf("%s: headers ct=%q cc=%q cd=%q ce=%q cl=%q", what, aws.ToString(ct), aws.ToString(cc), aws.ToString(cd), aws.ToString(ce), aws.ToString(cl))
				}
				if exp == nil {
					t.Errorf("%s: no Expires", what)
				} else if pt, err := http.ParseTime(*exp); err != nil || !pt.Equal(expires) {
					t.Errorf("%s: Expires = %q", what, *exp)
				}
				if !reflect.DeepEqual(meta, map[string]string{"color": "blue", "multi-word": "Hello World"}) {
					t.Errorf("%s: metadata = %v", what, meta)
				}
				if aws.ToString(etag) != quoted(md5Hex(data)) || aws.ToInt64(size) != int64(len(data)) {
					t.Errorf("%s: etag %s size %d", what, aws.ToString(etag), aws.ToInt64(size))
				}
				if lm == nil || time.Since(*lm) > time.Minute || lm.Nanosecond() != 0 {
					t.Errorf("%s: LastModified = %v", what, lm)
				}
			}

			head, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("obj/one.bin")})
			must(t, err, "HeadObject")
			check("HEAD", head.ContentType, head.CacheControl, head.ContentDisposition, head.ContentEncoding, head.ContentLanguage,
				head.ExpiresString, head.Metadata, head.ETag, head.ContentLength, head.LastModified)

			get, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("obj/one.bin")})
			must(t, err, "GetObject")
			body := readAll(t, get.Body)
			if !bytes.Equal(body, data) {
				t.Fatalf("GET body differs (%d bytes)", len(body))
			}
			check("GET", get.ContentType, get.CacheControl, get.ContentDisposition, get.ContentEncoding, get.ContentLanguage,
				get.ExpiresString, get.Metadata, get.ETag, get.ContentLength, get.LastModified)
			if aws.ToInt32(get.TagCount) != 2 {
				t.Errorf("TagCount = %d", aws.ToInt32(get.TagCount))
			}
			if aws.ToString(get.AcceptRanges) != "bytes" {
				t.Errorf("Accept-Ranges = %q", aws.ToString(get.AcceptRanges))
			}

			tags, err := c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(b), Key: aws.String("obj/one.bin")})
			must(t, err, "GetObjectTagging")
			if got := tagMap(tags.TagSet); !reflect.DeepEqual(got, map[string]string{"k1": "v1", "k2": "v 2"}) {
				t.Errorf("tags = %v", got)
			}

			// Response header overrides (signed request).
			ov, err := c.GetObject(ctx(t), &s3.GetObjectInput{
				Bucket: aws.String(b), Key: aws.String("obj/one.bin"),
				ResponseContentType:        aws.String("text/plain"),
				ResponseContentDisposition: aws.String("inline"),
				ResponseCacheControl:       aws.String("no-store"),
			})
			must(t, err, "GetObject overrides")
			readAll(t, ov.Body)
			if aws.ToString(ov.ContentType) != "text/plain" || aws.ToString(ov.ContentDisposition) != "inline" || aws.ToString(ov.CacheControl) != "no-store" {
				t.Errorf("overrides: %q %q %q", aws.ToString(ov.ContentType), aws.ToString(ov.ContentDisposition), aws.ToString(ov.CacheControl))
			}

			// Empty object, default content type.
			e.put(t, b, "empty", nil)
			eh, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("empty")})
			must(t, err, "HeadObject empty")
			if aws.ToInt64(eh.ContentLength) != 0 || aws.ToString(eh.ETag) != quoted(md5Hex(nil)) {
				t.Errorf("empty: len %d etag %s", aws.ToInt64(eh.ContentLength), aws.ToString(eh.ETag))
			}

			// Overwrite.
			e.put(t, b, "obj/one.bin", []byte("v2"))
			if got := string(e.get(t, b, "obj/one.bin")); got != "v2" {
				t.Fatalf("overwrite read %q", got)
			}

			// Delete (twice: idempotent 204), then 404s.
			for i := 0; i < 2; i++ {
				_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(b), Key: aws.String("obj/one.bin")})
				must(t, err, "DeleteObject")
			}
			_, err = c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("obj/one.bin")})
			wantErr(t, err, "NoSuchKey", http.StatusNotFound)
			_, err = c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("obj/one.bin")})
			if httpStatus(err) != http.StatusNotFound {
				t.Fatalf("HEAD deleted: %v", err)
			}
			raw := e.signed(t, rootCreds, http.MethodGet, "/"+b+"/obj/one.bin", nil, nil)
			if raw.status != 404 || raw.code() != "NoSuchKey" || !strings.Contains(string(raw.body), "<Key>obj/one.bin</Key>") {
				t.Errorf("raw GET missing: %d %s", raw.status, raw.body)
			}
			if raw.header.Get("x-amz-request-id") == "" || raw.header.Get("Server") == "" {
				t.Errorf("missing common headers: %v", raw.header)
			}
			rawHead := e.signed(t, rootCreds, http.MethodHead, "/"+b+"/obj/one.bin", nil, nil)
			if rawHead.status != 404 || len(rawHead.body) != 0 {
				t.Errorf("raw HEAD missing: %d %q", rawHead.status, rawHead.body)
			}

			// Missing bucket.
			_, err = c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String("no-such-bucket-e2e"), Key: aws.String("k")})
			wantErr(t, err, "NoSuchBucket", http.StatusNotFound)
			_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String("no-such-bucket-e2e"), Key: aws.String("k"), Body: stringsReader("x")})
			wantErr(t, err, "NoSuchBucket", http.StatusNotFound)
		})
	}
}

func tagMap(ts []s3types.Tag) map[string]string {
	m := map[string]string{}
	for _, t := range ts {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}

func TestObjectKeyLimits(t *testing.T) {
	t.Parallel()
	e := fsEnv
	b := e.newBucket(t)
	long := strings.Repeat("k", 1024)
	e.put(t, b, long, []byte("max"))
	if got := string(e.get(t, b, long)); got != "max" {
		t.Fatalf("1024-byte key read %q", got)
	}
	_, err := e.root().PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String(long + "k"), Body: stringsReader("x")})
	wantErr(t, err, "KeyTooLongError", http.StatusBadRequest)

	// User metadata over 2 KiB.
	_, err = e.root().PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("meta"), Body: stringsReader("x"),
		Metadata: map[string]string{"big": strings.Repeat("m", 2100)}})
	wantErr(t, err, "MetadataTooLarge", http.StatusBadRequest)

	// More than 10 tags.
	var tags []string
	for i := 0; i < 11; i++ {
		tags = append(tags, fmt.Sprintf("t%d=v", i))
	}
	_, err = e.root().PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("tags"), Body: stringsReader("x"),
		Tagging: aws.String(strings.Join(tags, "&"))})
	wantErr(t, err, "BadRequest", http.StatusBadRequest)
}

func TestCopyObject(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			src := e.newBucket(t)
			dst := e.newBucket(t)
			data := randomData(300_000, 2)
			srcKey := "src dir/ünï+code%key"
			put := e.put(t, src, srcKey, data, func(in *s3.PutObjectInput) {
				in.ContentType = aws.String("image/png")
				in.Metadata = map[string]string{"orig": "yes"}
				in.Tagging = aws.String("t=1")
			})
			copySource := src + "/" + url.PathEscape(srcKey)
			copySource = strings.ReplaceAll(copySource, "%2F", "/")

			// Default directives: COPY metadata and tags.
			out, err := c.CopyObject(ctx(t), &s3.CopyObjectInput{Bucket: aws.String(dst), Key: aws.String("copy1"), CopySource: aws.String(copySource)})
			must(t, err, "CopyObject")
			if out.CopyObjectResult == nil || aws.ToString(out.CopyObjectResult.ETag) != aws.ToString(put.ETag) || out.CopyObjectResult.LastModified == nil {
				t.Fatalf("CopyObjectResult = %+v", out.CopyObjectResult)
			}
			h, err := c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(dst), Key: aws.String("copy1")})
			must(t, err, "Head copy1")
			if aws.ToString(h.ContentType) != "image/png" || h.Metadata["orig"] != "yes" {
				t.Errorf("COPY directive lost metadata: %q %v", aws.ToString(h.ContentType), h.Metadata)
			}
			tg, err := c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(dst), Key: aws.String("copy1")})
			must(t, err, "GetObjectTagging copy1")
			if !reflect.DeepEqual(tagMap(tg.TagSet), map[string]string{"t": "1"}) {
				t.Errorf("COPY tagging = %v", tagMap(tg.TagSet))
			}
			if !bytes.Equal(e.get(t, dst, "copy1"), data) {
				t.Fatal("copy data differs")
			}

			// REPLACE directives.
			_, err = c.CopyObject(ctx(t), &s3.CopyObjectInput{
				Bucket: aws.String(dst), Key: aws.String("copy2"), CopySource: aws.String(copySource),
				MetadataDirective: s3types.MetadataDirectiveReplace, ContentType: aws.String("text/plain"),
				Metadata:         map[string]string{"new": "1"},
				TaggingDirective: s3types.TaggingDirectiveReplace, Tagging: aws.String("x=y&z=w"),
			})
			must(t, err, "CopyObject REPLACE")
			h, err = c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(dst), Key: aws.String("copy2")})
			must(t, err, "Head copy2")
			if aws.ToString(h.ContentType) != "text/plain" || !reflect.DeepEqual(h.Metadata, map[string]string{"new": "1"}) {
				t.Errorf("REPLACE: %q %v", aws.ToString(h.ContentType), h.Metadata)
			}
			tg, err = c.GetObjectTagging(ctx(t), &s3.GetObjectTaggingInput{Bucket: aws.String(dst), Key: aws.String("copy2")})
			must(t, err, "GetObjectTagging copy2")
			if !reflect.DeepEqual(tagMap(tg.TagSet), map[string]string{"x": "y", "z": "w"}) {
				t.Errorf("REPLACE tagging = %v", tagMap(tg.TagSet))
			}

			// Copy onto itself without changes is illegal; with REPLACE it is fine.
			self := dst + "/copy1"
			_, err = c.CopyObject(ctx(t), &s3.CopyObjectInput{Bucket: aws.String(dst), Key: aws.String("copy1"), CopySource: aws.String(self)})
			wantErr(t, err, "InvalidRequest", http.StatusBadRequest)
			_, err = c.CopyObject(ctx(t), &s3.CopyObjectInput{Bucket: aws.String(dst), Key: aws.String("copy1"), CopySource: aws.String(self),
				MetadataDirective: s3types.MetadataDirectiveReplace, Metadata: map[string]string{"self": "1"}})
			must(t, err, "self copy REPLACE")
			h, err = c.HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(dst), Key: aws.String("copy1")})
			must(t, err, "Head self copy")
			if !reflect.DeepEqual(h.Metadata, map[string]string{"self": "1"}) {
				t.Errorf("self copy metadata %v", h.Metadata)
			}

			// Conditional copies.
			etag := aws.ToString(put.ETag)
			past := time.Now().Add(-24 * time.Hour)
			future := time.Now().Add(24 * time.Hour)
			cond := []struct {
				name string
				in   s3.CopyObjectInput
				ok   bool
			}{
				{"if-match ok", s3.CopyObjectInput{CopySourceIfMatch: aws.String(etag)}, true},
				{"if-match bad", s3.CopyObjectInput{CopySourceIfMatch: aws.String(`"0123"`)}, false},
				{"if-none-match match", s3.CopyObjectInput{CopySourceIfNoneMatch: aws.String(etag)}, false},
				{"if-none-match other", s3.CopyObjectInput{CopySourceIfNoneMatch: aws.String(`"0123"`)}, true},
				{"if-modified-since past", s3.CopyObjectInput{CopySourceIfModifiedSince: aws.Time(past)}, true},
				{"if-modified-since future", s3.CopyObjectInput{CopySourceIfModifiedSince: aws.Time(future)}, false},
				{"if-unmodified-since future", s3.CopyObjectInput{CopySourceIfUnmodifiedSince: aws.Time(future)}, true},
				{"if-unmodified-since past", s3.CopyObjectInput{CopySourceIfUnmodifiedSince: aws.Time(past)}, false},
				// AWS: if-match true + if-unmodified-since false → copy happens.
				{"if-match + unmodified-since past", s3.CopyObjectInput{CopySourceIfMatch: aws.String(etag), CopySourceIfUnmodifiedSince: aws.Time(past)}, true},
				// AWS: if-none-match false + if-modified-since true → 412.
				{"if-none-match match + modified-since past", s3.CopyObjectInput{CopySourceIfNoneMatch: aws.String(etag), CopySourceIfModifiedSince: aws.Time(past)}, false},
			}
			for _, tc := range cond {
				in := tc.in
				in.Bucket, in.Key, in.CopySource = aws.String(dst), aws.String("cond"), aws.String(copySource)
				_, err := c.CopyObject(ctx(t), &in)
				if tc.ok {
					if err != nil {
						t.Errorf("%s: %v", tc.name, err)
					}
				} else if apiCode(err) != "PreconditionFailed" || httpStatus(err) != http.StatusPreconditionFailed {
					t.Errorf("%s: want 412 PreconditionFailed, got %v", tc.name, err)
				}
			}

			// Missing source.
			_, err = c.CopyObject(ctx(t), &s3.CopyObjectInput{Bucket: aws.String(dst), Key: aws.String("x"), CopySource: aws.String(src + "/missing")})
			wantErr(t, err, "NoSuchKey", http.StatusNotFound)
			_, err = c.CopyObject(ctx(t), &s3.CopyObjectInput{Bucket: aws.String(dst), Key: aws.String("x"), CopySource: aws.String("no-such-bucket-e2e/k")})
			wantErr(t, err, "NoSuchBucket", http.StatusNotFound)
		})
	}
}

func TestCopyObjectVersionSource(t *testing.T) {
	t.Parallel()
	e := fsEnv
	c := e.root()
	b := e.newVersionedBucket(t)
	v1 := e.put(t, b, "k", []byte("first"))
	e.put(t, b, "k", []byte("second"))
	out, err := c.CopyObject(ctx(t), &s3.CopyObjectInput{
		Bucket: aws.String(b), Key: aws.String("restored"),
		CopySource: aws.String(b + "/k?versionId=" + aws.ToString(v1.VersionId)),
	})
	must(t, err, "CopyObject versionId")
	if aws.ToString(out.CopySourceVersionId) != aws.ToString(v1.VersionId) || aws.ToString(out.VersionId) == "" {
		t.Errorf("copy version ids: source %q new %q", aws.ToString(out.CopySourceVersionId), aws.ToString(out.VersionId))
	}
	if got := string(e.get(t, b, "restored")); got != "first" {
		t.Fatalf("restored = %q", got)
	}
}

func TestRanges(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			b := e.newBucket(t)
			data := []byte("0123456789abcdefghij") // 20 bytes
			e.put(t, b, "r", data)
			cases := []struct {
				rng          string
				status       int
				body         string
				contentRange string
			}{
				{"bytes=0-4", 206, "01234", "bytes 0-4/20"},
				{"bytes=5-", 206, "56789abcdefghij", "bytes 5-19/20"},
				{"bytes=-3", 206, "hij", "bytes 17-19/20"},
				{"bytes=18-1000", 206, "ij", "bytes 18-19/20"},
				{"bytes=-100", 206, string(data), "bytes 0-19/20"},
				{"bytes=19-19", 206, "j", "bytes 19-19/20"},
				{"bytes=20-30", 416, "", "bytes */20"},
				{"bytes=5-2", 200, string(data), ""},     // invalid: ignored
				{"bytes=0-1,3-4", 200, string(data), ""}, // multiple ranges: whole object
				{"items=0-1", 200, string(data), ""},     // unknown unit: ignored
			}
			for _, tc := range cases {
				resp := e.signed(t, rootCreds, http.MethodGet, "/"+b+"/r", nil, map[string]string{"Range": tc.rng})
				if resp.status != tc.status {
					t.Errorf("%s: status %d, want %d (%s)", tc.rng, resp.status, tc.status, resp.body)
					continue
				}
				if tc.status == 416 {
					if resp.code() != "InvalidRange" {
						t.Errorf("%s: code %q", tc.rng, resp.code())
					}
				} else if string(resp.body) != tc.body {
					t.Errorf("%s: body %q, want %q", tc.rng, resp.body, tc.body)
				}
				if got := resp.header.Get("Content-Range"); got != tc.contentRange {
					t.Errorf("%s: Content-Range %q, want %q", tc.rng, got, tc.contentRange)
				}
				if tc.status == 206 && resp.header.Get("Content-Length") != fmt.Sprint(len(tc.body)) {
					t.Errorf("%s: Content-Length %q", tc.rng, resp.header.Get("Content-Length"))
				}
			}
			// SDK range read.
			out, err := e.root().GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("r"), Range: aws.String("bytes=10-14")})
			must(t, err, "GetObject range")
			if got := string(readAll(t, out.Body)); got != "abcde" || aws.ToString(out.ContentRange) != "bytes 10-14/20" {
				t.Errorf("SDK range: %q %q", got, aws.ToString(out.ContentRange))
			}
			// HEAD with range.
			resp := e.signed(t, rootCreds, http.MethodHead, "/"+b+"/r", nil, map[string]string{"Range": "bytes=2-3"})
			if resp.status != 206 || resp.header.Get("Content-Length") != "2" || resp.header.Get("Content-Range") != "bytes 2-3/20" {
				t.Errorf("HEAD range: %d %v", resp.status, resp.header)
			}
		})
	}
}

func TestConditionalGet(t *testing.T) {
	t.Parallel()
	e := memEnv
	b := e.newBucket(t)
	put := e.put(t, b, "c", []byte("conditional"))
	etag := aws.ToString(put.ETag)
	head, err := e.root().HeadObject(ctx(t), &s3.HeadObjectInput{Bucket: aws.String(b), Key: aws.String("c")})
	must(t, err, "HeadObject")
	lm := aws.ToTime(head.LastModified)
	past := lm.Add(-time.Hour).Format(http.TimeFormat)
	future := lm.Add(time.Hour).Format(http.TimeFormat)
	exact := lm.Format(http.TimeFormat)

	cases := []struct {
		name   string
		hdr    map[string]string
		status int
	}{
		{"if-match ok", map[string]string{"If-Match": etag}, 200},
		{"if-match star", map[string]string{"If-Match": "*"}, 200},
		{"if-match list", map[string]string{"If-Match": `"nope", ` + etag}, 200},
		{"if-match bad", map[string]string{"If-Match": `"nope"`}, 412},
		{"if-none-match match", map[string]string{"If-None-Match": etag}, 304},
		{"if-none-match star", map[string]string{"If-None-Match": "*"}, 304},
		{"if-none-match other", map[string]string{"If-None-Match": `"nope"`}, 200},
		{"if-modified-since past", map[string]string{"If-Modified-Since": past}, 200},
		{"if-modified-since exact", map[string]string{"If-Modified-Since": exact}, 304},
		{"if-modified-since future", map[string]string{"If-Modified-Since": future}, 304},
		{"if-unmodified-since future", map[string]string{"If-Unmodified-Since": future}, 200},
		{"if-unmodified-since exact", map[string]string{"If-Unmodified-Since": exact}, 200},
		{"if-unmodified-since past", map[string]string{"If-Unmodified-Since": past}, 412},
		// RFC 7232 / AWS precedence rules.
		{"if-match true + unmodified false", map[string]string{"If-Match": etag, "If-Unmodified-Since": past}, 200},
		{"if-none-match false + modified true", map[string]string{"If-None-Match": etag, "If-Modified-Since": past}, 304},
		{"if-none-match true + modified false", map[string]string{"If-None-Match": `"nope"`, "If-Modified-Since": future}, 200},
		{"if-match bad + none-match", map[string]string{"If-Match": `"nope"`, "If-None-Match": etag}, 412},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			resp := e.signed(t, rootCreds, method, "/"+b+"/c", nil, tc.hdr)
			if resp.status != tc.status {
				t.Errorf("%s %s: status %d, want %d", method, tc.name, resp.status, tc.status)
				continue
			}
			switch tc.status {
			case 304:
				if len(resp.body) != 0 || resp.header.Get("ETag") != etag || resp.header.Get("Last-Modified") == "" {
					t.Errorf("%s %s: 304 body %q etag %q", method, tc.name, resp.body, resp.header.Get("ETag"))
				}
			case 412:
				if method == http.MethodGet && resp.code() != "PreconditionFailed" {
					t.Errorf("%s %s: code %q", method, tc.name, resp.code())
				}
			case 200:
				if method == http.MethodGet && string(resp.body) != "conditional" {
					t.Errorf("%s %s: body %q", method, tc.name, resp.body)
				}
			}
		}
	}

	// SDK surfaces 304 and 412 as errors.
	_, err = e.root().GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("c"), IfNoneMatch: aws.String(etag)})
	if httpStatus(err) != http.StatusNotModified {
		t.Errorf("SDK If-None-Match: %v", err)
	}
	_, err = e.root().GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(b), Key: aws.String("c"), IfMatch: aws.String(`"nope"`)})
	wantErr(t, err, "PreconditionFailed", http.StatusPreconditionFailed)
}

func TestConditionalPut(t *testing.T) {
	t.Parallel()
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			b := e.newBucket(t)
			// If-None-Match: * creates only.
			first, err := c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("once"), Body: stringsReader("1"), IfNoneMatch: aws.String("*")})
			must(t, err, "create-only PUT")
			_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("once"), Body: stringsReader("2"), IfNoneMatch: aws.String("*")})
			wantErr(t, err, "PreconditionFailed", http.StatusPreconditionFailed)
			if got := string(e.get(t, b, "once")); got != "1" {
				t.Fatalf("object overwritten: %q", got)
			}
			// If-Match.
			_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("once"), Body: stringsReader("3"), IfMatch: aws.String(`"0000"`)})
			wantErr(t, err, "PreconditionFailed", http.StatusPreconditionFailed)
			_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("once"), Body: stringsReader("4"), IfMatch: first.ETag})
			must(t, err, "If-Match PUT")
			if got := string(e.get(t, b, "once")); got != "4" {
				t.Fatalf("If-Match PUT result %q", got)
			}
			// If-Match on a missing key: 404 NoSuchKey.
			_, err = c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("missing"), Body: stringsReader("x"), IfMatch: first.ETag})
			wantErr(t, err, "NoSuchKey", http.StatusNotFound)

			// Concurrent create-only writers: exactly one wins.
			const n = 16
			errs := make(chan error, n)
			for i := 0; i < n; i++ {
				go func(i int) {
					_, err := c.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(b), Key: aws.String("race"),
						Body: stringsReader(fmt.Sprint(i)), IfNoneMatch: aws.String("*")})
					errs <- err
				}(i)
			}
			wins := 0
			for i := 0; i < n; i++ {
				err := <-errs
				switch {
				case err == nil:
					wins++
				case apiCode(err) == "PreconditionFailed" || apiCode(err) == "ConditionalRequestConflict":
				default:
					t.Errorf("racing create: %v", err)
				}
			}
			if wins != 1 {
				t.Fatalf("%d create-only writers succeeded, want 1", wins)
			}
		})
	}
}

func rawMD5(b []byte) []byte {
	s := md5.Sum(b)
	return s[:]
}
