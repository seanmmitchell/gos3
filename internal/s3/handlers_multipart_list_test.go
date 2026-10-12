package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
)

func TestListPartsPaging(t *testing.T) {
	mpSetMinPartSize(t, 1)
	e := newTestEnv(t)
	e.createBucket("mp")
	c := e.client
	ctx := context.Background()
	id := mpCreate(t, c, "mp", "k", func(in *awss3.CreateMultipartUploadInput) {
		in.StorageClass = types.StorageClassGlacierIr
	})
	nums := []int32{8, 1, 5, 2, 3}
	etags := map[int32]string{}
	for _, n := range nums {
		o := mpMustUploadPart(t, c, "mp", "k", id, n, []byte(strings.Repeat("x", int(n))), nil)
		etags[n] = aws.ToString(o.ETag)
	}

	var got []int32
	var marker *string
	pages := 0
	for {
		out, err := c.ListParts(ctx, &awss3.ListPartsInput{
			Bucket: aws.String("mp"), Key: aws.String("k"), UploadId: aws.String(id),
			MaxParts: aws.Int32(2), PartNumberMarker: marker,
		})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if aws.ToString(out.UploadId) != id || aws.ToString(out.Key) != "k" || aws.ToString(out.Bucket) != "mp" ||
			aws.ToInt32(out.MaxParts) != 2 || out.StorageClass != types.StorageClassGlacierIr {
			t.Fatalf("ListParts header fields = %+v", out)
		}
		if out.Initiator == nil || aws.ToString(out.Initiator.DisplayName) != "root" || out.Owner == nil || aws.ToString(out.Owner.ID) != ownerFor("").ID {
			t.Fatalf("Initiator/Owner = %+v %+v", out.Initiator, out.Owner)
		}
		for _, p := range out.Parts {
			n := aws.ToInt32(p.PartNumber)
			got = append(got, n)
			if aws.ToString(p.ETag) != etags[n] || aws.ToInt64(p.Size) != int64(n) || p.LastModified == nil {
				t.Fatalf("part %d = %+v", n, p)
			}
			// Without a declared algorithm parts carry the default CRC64NVME.
			if aws.ToString(p.ChecksumCRC64NVME) != computeChecksum(ChecksumCRC64NVME, []byte(strings.Repeat("x", int(n)))) {
				t.Fatalf("part %d CRC64NVME = %q", n, aws.ToString(p.ChecksumCRC64NVME))
			}
		}
		if !aws.ToBool(out.IsTruncated) {
			break
		}
		if len(out.Parts) == 0 {
			t.Fatal("truncated page without parts")
		}
		if want := fmt.Sprint(aws.ToInt32(out.Parts[len(out.Parts)-1].PartNumber)); aws.ToString(out.NextPartNumberMarker) != want {
			t.Fatalf("NextPartNumberMarker = %q, want %s", aws.ToString(out.NextPartNumberMarker), want)
		}
		marker = out.NextPartNumberMarker
	}
	if !reflect.DeepEqual(got, []int32{1, 2, 3, 5, 8}) || pages != 3 {
		t.Fatalf("parts = %v in %d pages", got, pages)
	}

	// Raw response: element order and markers.
	resp := e.signed(http.MethodGet, mpPath("mp", "k", mpQuery("uploadId", id, "part-number-marker", "3", "max-parts", "5000")), nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	body := string(resp.Data)
	for _, want := range []string{
		`<ListPartsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>mp</Bucket><Key>k</Key><UploadId>` + id + `</UploadId><Initiator>`,
		`<StorageClass>GLACIER_IR</StorageClass><PartNumberMarker>3</PartNumberMarker><NextPartNumberMarker>8</NextPartNumberMarker><MaxParts>1000</MaxParts><IsTruncated>false</IsTruncated><Part><PartNumber>5</PartNumber>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<ChecksumAlgorithm>") {
		t.Fatalf("upload without checksum algorithm lists one: %s", body)
	}
	// max-parts=0: empty, not truncated.
	resp = e.signed(http.MethodGet, mpPath("mp", "k", mpQuery("uploadId", id, "max-parts", "0")), nil, nil)
	if !strings.Contains(string(resp.Data), "<IsTruncated>false</IsTruncated></ListPartsResult>") {
		t.Fatalf("max-parts=0: %s", resp.Data)
	}
	// An upload without parts.
	id2 := mpCreate(t, c, "mp", "empty", nil)
	out, err := c.ListParts(ctx, &awss3.ListPartsInput{Bucket: aws.String("mp"), Key: aws.String("empty"), UploadId: aws.String(id2)})
	if err != nil || len(out.Parts) != 0 || aws.ToBool(out.IsTruncated) {
		t.Fatalf("empty upload: %+v %v", out, err)
	}
}

func TestListPartsInitiatorIAMUser(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("mp")
	creds := e.createUser("alice", "readwrite")
	c := e.clientFor(creds)
	id := mpCreate(t, c, "mp", "k", nil)
	out, err := c.ListParts(context.Background(), &awss3.ListPartsInput{Bucket: aws.String("mp"), Key: aws.String("k"), UploadId: aws.String(id)})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.Initiator.DisplayName) != "alice" || aws.ToString(out.Initiator.ID) != ownerFor("alice").ID {
		t.Fatalf("Initiator = %+v", out.Initiator)
	}
	if aws.ToString(out.Owner.DisplayName) != "root" {
		t.Fatalf("Owner = %+v", out.Owner)
	}
	up, err := e.backend.GetMultipartUpload(context.Background(), "mp", "k", id)
	if err != nil || up.Initiator != "alice" {
		t.Fatalf("stored initiator = %q (%v)", up.Initiator, err)
	}
	// Root uploads are recorded with the empty initiator.
	id2 := mpCreate(t, e.client, "mp", "k", nil)
	if up, _ := e.backend.GetMultipartUpload(context.Background(), "mp", "k", id2); up.Initiator != "" {
		t.Fatalf("root initiator = %q", up.Initiator)
	}
}

// mpUploadKey is a (key, upload ID) pair of a listing.
type mpUploadKey struct{ key, id string }

func TestListMultipartUploadsPaging(t *testing.T) {
	e, _ := newWalkOnlyEnv(t)
	e.createBucket("mp")
	c := e.client
	ctx := context.Background()
	keys := []string{"a/1", "a/2", "b", "b", "c/x/y", "d", "b"}
	for _, k := range keys {
		mpCreate(t, c, "mp", k, nil)
		time.Sleep(2 * time.Millisecond) // distinct initiation times
	}
	all, err := e.backend.ListMultipartUploads(ctx, "mp")
	if err != nil {
		t.Fatal(err)
	}
	var want []mpUploadKey
	for _, u := range all {
		want = append(want, mpUploadKey{u.Key, u.UploadID})
	}

	list := func(in *awss3.ListMultipartUploadsInput) (ups []mpUploadKey, prefixes []string, pages int) {
		t.Helper()
		in.Bucket = aws.String("mp")
		for {
			out, err := c.ListMultipartUploads(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			pages++
			for _, u := range out.Uploads {
				ups = append(ups, mpUploadKey{aws.ToString(u.Key), aws.ToString(u.UploadId)})
				if u.Initiator == nil || u.Owner == nil || u.Initiated == nil || u.StorageClass != types.StorageClassStandard {
					t.Fatalf("upload fields = %+v", u)
				}
			}
			for _, p := range out.CommonPrefixes {
				prefixes = append(prefixes, aws.ToString(p.Prefix))
			}
			if !aws.ToBool(out.IsTruncated) {
				return
			}
			if pages > 20 {
				t.Fatal("too many pages")
			}
			in.KeyMarker, in.UploadIdMarker = out.NextKeyMarker, out.NextUploadIdMarker
		}
	}

	ups, prefixes, pages := list(&awss3.ListMultipartUploadsInput{MaxUploads: aws.Int32(2)})
	if !reflect.DeepEqual(ups, want) || len(prefixes) != 0 || pages != 4 {
		t.Fatalf("paged = %v %v (%d pages), want %v", ups, prefixes, pages, want)
	}
	ups, prefixes, _ = list(&awss3.ListMultipartUploadsInput{MaxUploads: aws.Int32(1), Delimiter: aws.String("/")})
	var wantNoDirs []mpUploadKey
	for _, u := range want {
		if !strings.Contains(u.key, "/") {
			wantNoDirs = append(wantNoDirs, u)
		}
	}
	if !reflect.DeepEqual(ups, wantNoDirs) || !reflect.DeepEqual(prefixes, []string{"a/", "c/"}) {
		t.Fatalf("delimited = %v %v", ups, prefixes)
	}
	ups, prefixes, _ = list(&awss3.ListMultipartUploadsInput{Prefix: aws.String("c/"), Delimiter: aws.String("/")})
	if len(ups) != 0 || !reflect.DeepEqual(prefixes, []string{"c/x/"}) {
		t.Fatalf("prefix c/ = %v %v", ups, prefixes)
	}
	ups, _, _ = list(&awss3.ListMultipartUploadsInput{KeyMarker: aws.String("b")})
	if !reflect.DeepEqual(ups, want[5:]) {
		t.Fatalf("after key b = %v", ups)
	}
	ups, _, _ = list(&awss3.ListMultipartUploadsInput{KeyMarker: aws.String("b"), UploadIdMarker: aws.String(want[2].id)})
	if !reflect.DeepEqual(ups, want[3:]) {
		t.Fatalf("after first b upload = %v", ups)
	}

	// Raw document.
	resp := e.signed(http.MethodGet, "/mp?uploads&max-uploads=1&prefix=a/", nil, nil)
	body := string(resp.Data)
	for _, w := range []string{
		`<ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>mp</Bucket><KeyMarker></KeyMarker><UploadIdMarker></UploadIdMarker><NextKeyMarker>a/1</NextKeyMarker><NextUploadIdMarker>` + want[0].id + `</NextUploadIdMarker><Prefix>a/</Prefix><MaxUploads>1</MaxUploads><IsTruncated>true</IsTruncated><Upload><Key>a/1</Key>`,
		`<StorageClass>STANDARD</StorageClass><Initiated>`,
	} {
		if !strings.Contains(body, w) {
			t.Fatalf("body lacks %s:\n%s", w, body)
		}
	}
}

func TestListMultipartUploadsEncodingAndChecksum(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("mp")
	mpRawCreate(t, e, "mp", "dir/with space+plus", http.Header{"X-Amz-Checksum-Algorithm": {"CRC32C"}, "X-Amz-Checksum-Type": {"FULL_OBJECT"}})
	resp := e.signed(http.MethodGet, "/mp?uploads&encoding-type=url&delimiter=%2B&prefix=dir/", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	var res struct {
		Prefix         string
		Delimiter      string
		EncodingType   string
		CommonPrefixes []struct{ Prefix string }
	}
	if err := xml.Unmarshal(resp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Prefix != "dir/" || res.Delimiter != "%2B" || res.EncodingType != "url" ||
		len(res.CommonPrefixes) != 1 || res.CommonPrefixes[0].Prefix != "dir/with+space%2B" {
		t.Fatalf("encoded listing = %+v\n%s", res, resp.Data)
	}
	resp = e.signed(http.MethodGet, "/mp?uploads&encoding-type=url", nil, nil)
	if !strings.Contains(string(resp.Data), "<Key>dir/with+space%2Bplus</Key>") ||
		!strings.Contains(string(resp.Data), "<ChecksumAlgorithm>CRC32C</ChecksumAlgorithm><ChecksumType>FULL_OBJECT</ChecksumType></Upload>") {
		t.Fatalf("listing = %s", resp.Data)
	}
	// The SDK decodes url-encoded keys.
	out, err := e.client.ListMultipartUploads(context.Background(), &awss3.ListMultipartUploadsInput{Bucket: aws.String("mp"), EncodingType: types.EncodingTypeUrl})
	if err != nil || len(out.Uploads) != 1 || out.Uploads[0].ChecksumAlgorithm != types.ChecksumAlgorithmCrc32c {
		t.Fatalf("SDK listing = %+v %v", out, err)
	}
}

// walkOnlyBackend is a memory backend whose ListMultipartUploads (the
// whole-bucket listing) fails, and whose upload walker counts the uploads
// it yields: ListMultipartUploads must page through the walker.
type walkOnlyBackend struct {
	*memory.Backend
	yielded atomic.Int64
}

func (b *walkOnlyBackend) ListMultipartUploads(context.Context, string) ([]storage.UploadInfo, error) {
	return nil, errors.New("walkOnlyBackend: whole-bucket upload listing used")
}

func (b *walkOnlyBackend) WalkMultipartUploads(ctx context.Context, bucket, prefix string, after storage.UploadPosition, fn func(storage.UploadInfo) error) error {
	return b.Backend.WalkMultipartUploads(ctx, bucket, prefix, after, func(u storage.UploadInfo) error {
		b.yielded.Add(1)
		return fn(u)
	})
}

// newWalkOnlyEnv is a test env whose server uses a walkOnlyBackend over
// e.backend.
func newWalkOnlyEnv(t *testing.T) (*testEnv, *walkOnlyBackend) {
	wb := &walkOnlyBackend{}
	e := newTestEnv(t, withOptions(func(o *Options) {
		wb.Backend = o.Backend.(*memory.Backend)
		o.Backend = wb
	}))
	return e, wb
}

// TestListMultipartUploadsCostsAPage: a ListMultipartUploads page reads
// O(max-uploads) uploads, however many are pending in the bucket.
func TestListMultipartUploadsCostsAPage(t *testing.T) {
	e, wb := newWalkOnlyEnv(t)
	e.createBucket("mp")
	ctx := context.Background()
	var last storage.UploadInfo
	for i := range 3000 {
		u, err := e.backend.CreateMultipartUpload(ctx, "mp", fmt.Sprintf("dir%d/k%04d", i%3, i), storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		last = u
	}
	for _, q := range []string{
		"max-uploads=1",
		"max-uploads=5&key-marker=" + last.Key + "&upload-id-marker=" + last.UploadID,
		"max-uploads=5&key-marker=dir1/k2000",
		"max-uploads=2&delimiter=/",
		"max-uploads=3&prefix=dir2/",
	} {
		wb.yielded.Store(0)
		resp := e.signed(http.MethodGet, "/mp?uploads&"+q, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", q, resp.StatusCode, resp.Data)
		}
		if n := wb.yielded.Load(); n > 20 {
			t.Errorf("%s: read %d uploads for one page", q, n)
		}
		if !strings.Contains(q, "upload-id-marker") && !strings.Contains(string(resp.Data), "<Upload>") && !strings.Contains(string(resp.Data), "<CommonPrefixes>") {
			t.Errorf("%s: empty page %s", q, resp.Data)
		}
	}
}

// --- UploadPartCopy ---

func TestUploadPartCopy(t *testing.T) {
	mpSetMinPartSize(t, 1024)
	e := newTestEnv(t)
	e.createBucket("mp")
	e.createBucket("srcb")
	ctx := context.Background()
	if err := e.backend.SetBucketVersioning(ctx, "srcb", storage.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	c := e.client
	src := mpData(20, 3000)
	old := e.putObject("srcb", "dir/src obj", []byte("old version"), storage.ObjectMeta{})
	srcInfo := e.putObject("srcb", "dir/src obj", src, storage.ObjectMeta{})
	tail := []byte("tail")

	id := mpCreate(t, c, "mp", "dst", func(in *awss3.CreateMultipartUploadInput) { in.ChecksumAlgorithm = types.ChecksumAlgorithmSha256 })
	cp := func(n int32, rng string, mod func(*awss3.UploadPartCopyInput)) (*awss3.UploadPartCopyOutput, error) {
		in := &awss3.UploadPartCopyInput{
			Bucket: aws.String("mp"), Key: aws.String("dst"), UploadId: aws.String(id), PartNumber: aws.Int32(n),
			CopySource: aws.String("srcb/dir/src%20obj"),
		}
		if rng != "" {
			in.CopySourceRange = aws.String(rng)
		}
		if mod != nil {
			mod(in)
		}
		return c.UploadPartCopy(ctx, in)
	}
	o1, err := cp(1, "bytes=0-1023", nil)
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(o1.CopyPartResult.ETag) != `"`+md5Hex(src[:1024])+`"` ||
		aws.ToString(o1.CopyPartResult.ChecksumSHA256) != computeChecksum(ChecksumSHA256, src[:1024]) ||
		o1.CopyPartResult.LastModified == nil {
		t.Fatalf("CopyPartResult = %+v", o1.CopyPartResult)
	}
	if aws.ToString(o1.CopySourceVersionId) != srcInfo.VersionID {
		t.Fatalf("CopySourceVersionId = %q, want %q", aws.ToString(o1.CopySourceVersionId), srcInfo.VersionID)
	}
	o2, err := cp(2, "bytes=1024-2999", func(in *awss3.UploadPartCopyInput) { in.CopySourceIfMatch = aws.String(`"` + srcInfo.ETag + `"`) })
	if err != nil {
		t.Fatal(err)
	}
	// A specific (older) source version.
	o3, err := cp(3, "", func(in *awss3.UploadPartCopyInput) {
		in.CopySource = aws.String("srcb/dir/src%20obj?versionId=" + old.VersionID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(o3.CopySourceVersionId) != old.VersionID {
		t.Fatalf("version copy = %+v", o3)
	}
	o4 := mpMustUploadPart(t, c, "mp", "dst", id, 4, tail, func(in *awss3.UploadPartInput) { in.ChecksumAlgorithm = types.ChecksumAlgorithmSha256 })

	for _, tc := range []struct {
		name, rng string
		mod       func(*awss3.UploadPartCopyInput)
		code      string
	}{
		{"range beyond source", "bytes=0-3000", nil, "InvalidArgument"},
		{"range reversed", "bytes=10-5", nil, "InvalidArgument"},
		{"range open ended", "bytes=10-", nil, "InvalidArgument"},
		{"range without unit", "0-10", nil, "InvalidArgument"},
		{"if-match fails", "", func(in *awss3.UploadPartCopyInput) { in.CopySourceIfMatch = aws.String(`"nope"`) }, "PreconditionFailed"},
		{"if-none-match fails", "", func(in *awss3.UploadPartCopyInput) { in.CopySourceIfNoneMatch = aws.String(`"` + srcInfo.ETag + `"`) }, "PreconditionFailed"},
		{"missing source key", "", func(in *awss3.UploadPartCopyInput) { in.CopySource = aws.String("srcb/none") }, "NoSuchKey"},
		{"missing source version", "", func(in *awss3.UploadPartCopyInput) { in.CopySource = aws.String("srcb/dir/src%20obj?versionId=zzz") }, "NoSuchVersion"},
		{"missing source bucket", "", func(in *awss3.UploadPartCopyInput) { in.CopySource = aws.String("nobucket/x") }, "NoSuchBucket"},
		{"malformed source", "", func(in *awss3.UploadPartCopyInput) { in.CopySource = aws.String("justbucket") }, "InvalidArgument"},
		{"missing upload", "", func(in *awss3.UploadPartCopyInput) { in.UploadId = aws.String("nope") }, "NoSuchUpload"},
		{"bad part number", "", func(in *awss3.UploadPartCopyInput) { in.PartNumber = aws.Int32(0) }, "InvalidArgument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cp(9, tc.rng, tc.mod)
			mpExpectCode(t, err, tc.code)
		})
	}

	parts := []types.CompletedPart{
		{PartNumber: aws.Int32(1), ETag: o1.CopyPartResult.ETag, ChecksumSHA256: o1.CopyPartResult.ChecksumSHA256},
		{PartNumber: aws.Int32(2), ETag: o2.CopyPartResult.ETag, ChecksumSHA256: o2.CopyPartResult.ChecksumSHA256},
		{PartNumber: aws.Int32(3), ETag: o3.CopyPartResult.ETag, ChecksumSHA256: o3.CopyPartResult.ChecksumSHA256},
	}
	// Part 3 (11 bytes) is too small unless last.
	_, err = mpComplete(c, "mp", "dst", id, append(parts, types.CompletedPart{PartNumber: aws.Int32(4), ETag: o4.ETag, ChecksumSHA256: o4.ChecksumSHA256}), nil)
	mpExpectCode(t, err, "EntityTooSmall")
	out, err := mpComplete(c, "mp", "dst", id, parts, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, src...), []byte("old version")...)
	_, data := mpGet(t, c, "mp", "dst", nil)
	if string(data) != string(want) {
		t.Fatal("content differs")
	}
	if aws.ToString(out.ETag) != mpExpectedETag(src[:1024], src[1024:], []byte("old version")) {
		t.Fatalf("ETag = %s", aws.ToString(out.ETag))
	}
}

func TestUploadPartCopyAuthorization(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("mp")
	e.createBucket("secret")
	e.putObject("secret", "s", []byte("classified"), storage.ObjectMeta{})
	e.putObject("mp", "pub", []byte("public"), storage.ObjectMeta{})
	e.putPolicy("mp-only", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::mp","arn:aws:s3:::mp/*"]}]}`)
	creds := e.createUser("bob", "mp-only")
	c := e.clientFor(creds)
	id := mpCreate(t, c, "mp", "dst", nil)
	in := &awss3.UploadPartCopyInput{
		Bucket: aws.String("mp"), Key: aws.String("dst"), UploadId: aws.String(id), PartNumber: aws.Int32(1),
		CopySource: aws.String("secret/s"),
	}
	_, err := c.UploadPartCopy(context.Background(), in)
	mpExpectCode(t, err, "AccessDenied")
	in.CopySource = aws.String("mp/pub")
	if _, err := c.UploadPartCopy(context.Background(), in); err != nil {
		t.Fatalf("allowed copy: %v", err)
	}
	// Anonymous callers cannot touch uploads.
	resp := e.anon(http.MethodGet, mpPath("mp", "dst", mpQuery("uploadId", id)), nil, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous ListParts: %d %s", resp.StatusCode, resp.Data)
	}
}
