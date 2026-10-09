package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// --- helpers ---

// bktAPICode returns the S3 error code of an SDK error ("" if none).
func bktAPICode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// bktExpect fails unless resp has the given status and error code.
func bktExpect(t *testing.T, resp *testResponse, status int, code string) {
	t.Helper()
	if resp.StatusCode != status || resp.code() != code {
		t.Fatalf("got %d %q, want %d %q; body: %s", resp.StatusCode, resp.code(), status, code, resp.Data)
	}
}

// bktVersioned enables versioning on a bucket directly in the backend.
func bktVersioned(t *testing.T, e *testEnv, bucket string) {
	t.Helper()
	if err := e.backend.SetBucketVersioning(context.Background(), bucket, storage.VersioningEnabled); err != nil {
		t.Fatalf("SetBucketVersioning: %v", err)
	}
}

// bktElementOrder returns the names of the direct children of the root
// element of an XML document, in order.
func bktElementOrder(t *testing.T, data []byte) []string {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(string(data)))
	depth := 0
	var names []string
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 {
				names = append(names, tk.Name.Local)
			}
		case xml.EndElement:
			depth--
		}
	}
	return names
}

// bktCompact removes consecutive duplicates from a list of names.
func bktCompact(names []string) []string {
	var out []string
	for _, n := range names {
		if len(out) == 0 || out[len(out)-1] != n {
			out = append(out, n)
		}
	}
	return out
}

// bktSpecialKeys are keys that need encoding-type=url to survive XML.
var bktSpecialKeys = []string{
	"plain",
	"with space",
	"plus+sign",
	"percent%41",
	"amp&lt<gt>",
	"ctrl\x01char",
	"tab\tand\nnewline",
	"unicode/ünï/日本",
	"dir/sub dir/file?.txt",
	"emoji-😀",
	"trailing/",
	"a//double",
}

// --- CreateBucket ---

func TestCreateBucketSDK(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	if _, err := e.client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("plain-bucket")}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	_, err := e.client.CreateBucket(ctx, &awss3.CreateBucketInput{
		Bucket: aws.String("eu-bucket"),
		CreateBucketConfiguration: &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraintEuWest1,
		},
	})
	if err != nil {
		t.Fatalf("CreateBucket eu: %v", err)
	}

	loc, err := e.client.GetBucketLocation(ctx, &awss3.GetBucketLocationInput{Bucket: aws.String("plain-bucket")})
	if err != nil {
		t.Fatalf("GetBucketLocation: %v", err)
	}
	if loc.LocationConstraint != "" {
		t.Errorf("us-east-1 location = %q, want empty", loc.LocationConstraint)
	}
	loc, err = e.client.GetBucketLocation(ctx, &awss3.GetBucketLocationInput{Bucket: aws.String("eu-bucket")})
	if err != nil {
		t.Fatalf("GetBucketLocation: %v", err)
	}
	if loc.LocationConstraint != types.BucketLocationConstraintEuWest1 {
		t.Errorf("eu location = %q", loc.LocationConstraint)
	}

	hb, err := e.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String("eu-bucket")})
	if err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}
	if aws.ToString(hb.BucketRegion) != "eu-west-1" || aws.ToBool(hb.AccessPointAlias) {
		t.Errorf("HeadBucket region=%q alias=%v", aws.ToString(hb.BucketRegion), aws.ToBool(hb.AccessPointAlias))
	}

	bi, err := e.backend.GetBucket(ctx, "plain-bucket")
	if err != nil || bi.Region != "us-east-1" || bi.Owner != "" || bi.ObjectLockEnabled {
		t.Errorf("stored bucket = %+v, %v", bi, err)
	}

	_, err = e.client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("plain-bucket")})
	if bktAPICode(err) != "BucketAlreadyOwnedByYou" {
		t.Errorf("re-create: %v", err)
	}
}

func TestCreateBucketRaw(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	cfg := func(region string) []byte {
		return []byte(`<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>` + region + `</LocationConstraint></CreateBucketConfiguration>`)
	}
	tests := []struct {
		name    string
		bucket  string
		body    []byte
		hdr     http.Header
		status  int
		code    string
		region  string
		lock    bool
		acl     string
		ownerXM string
	}{
		{name: "minimal", bucket: "b-minimal", status: 200, region: "us-east-1"},
		{name: "region", bucket: "b-region", body: cfg("ap-southeast-2"), status: 200, region: "ap-southeast-2"},
		{name: "custom region", bucket: "b-custom", body: cfg("garage"), status: 200, region: "garage"},
		{name: "empty constraint", bucket: "b-empty", body: cfg(""), status: 200, region: "us-east-1"},
		{name: "EU legacy", bucket: "b-eu", body: cfg("EU"), status: 200, region: "eu-west-1"},
		{name: "no namespace", bucket: "b-nons", body: []byte(`<CreateBucketConfiguration><LocationConstraint>us-west-2</LocationConstraint></CreateBucketConfiguration>`), status: 200, region: "us-west-2"},
		{name: "whitespace body", bucket: "b-ws", body: []byte("  \n"), status: 200, region: "us-east-1"},
		{name: "no constraint element", bucket: "b-noel", body: []byte(`<CreateBucketConfiguration/>`), status: 200, region: "us-east-1"},
		{name: "invalid region", bucket: "b-bad-region", body: cfg("Not A Region"), status: 400, code: "InvalidLocationConstraint"},
		{name: "region too long", bucket: "b-long-region", body: cfg(strings.Repeat("a", 65)), status: 400, code: "InvalidLocationConstraint"},
		{name: "malformed xml", bucket: "b-malformed", body: []byte(`<CreateBucketConfiguration><LocationConstraint>`), status: 400, code: "MalformedXML"},
		{name: "wrong root", bucket: "b-wrongroot", body: []byte(`<Foo/>`), status: 400, code: "MalformedXML"},
		{name: "directory bucket", bucket: "b-dir", body: []byte(`<CreateBucketConfiguration><Location><Name>usw2-az1</Name><Type>AvailabilityZone</Type></Location><Bucket><Type>Directory</Type></Bucket></CreateBucketConfiguration>`), status: 501, code: "NotImplemented"},
		{name: "too large body", bucket: "b-large", body: []byte(`<CreateBucketConfiguration>` + strings.Repeat(" ", maxCreateBucketBody) + `</CreateBucketConfiguration>`), status: 400, code: "MaxMessageLengthExceeded"},
		{name: "invalid name upper", bucket: "Bad-Name", status: 400, code: "InvalidBucketName"},
		{name: "invalid name short", bucket: "ab", status: 400, code: "InvalidBucketName"},
		{name: "invalid name ip", bucket: "192.168.1.1", status: 400, code: "InvalidBucketName"},
		{name: "invalid name underscore", bucket: "a_b_c", status: 400, code: "InvalidBucketName"},
		{name: "object lock", bucket: "b-lock", hdr: http.Header{"X-Amz-Bucket-Object-Lock-Enabled": {"True"}}, status: 200, region: "us-east-1", lock: true},
		{name: "object lock false", bucket: "b-nolock", hdr: http.Header{"X-Amz-Bucket-Object-Lock-Enabled": {"false"}}, status: 200, region: "us-east-1"},
		{name: "object lock invalid", bucket: "b-badlock", hdr: http.Header{"X-Amz-Bucket-Object-Lock-Enabled": {"yes"}}, status: 400, code: "InvalidArgument"},
		{name: "acl", bucket: "b-acl", hdr: http.Header{"X-Amz-Acl": {"public-read"}}, status: 200, region: "us-east-1", acl: "public-read"},
		{name: "acl private", bucket: "b-acl-private", hdr: http.Header{"X-Amz-Acl": {"private"}}, status: 200, region: "us-east-1"},
		{name: "acl log delivery", bucket: "b-acl-log", hdr: http.Header{"X-Amz-Acl": {"log-delivery-write"}}, status: 200, region: "us-east-1", acl: "log-delivery-write"},
		{name: "acl invalid", bucket: "b-acl-bad", hdr: http.Header{"X-Amz-Acl": {"everyone"}}, status: 400, code: "InvalidArgument"},
		{name: "grant header", bucket: "b-grant", hdr: http.Header{"X-Amz-Grant-Read": {"uri=http://acs.amazonaws.com/groups/global/AllUsers"}}, status: 501, code: "NotImplemented"},
		{name: "ownership", bucket: "b-own", hdr: http.Header{"X-Amz-Object-Ownership": {"BucketOwnerEnforced"}}, status: 200, region: "us-east-1", ownerXM: "BucketOwnerEnforced"},
		{name: "ownership writer", bucket: "b-own-w", hdr: http.Header{"X-Amz-Object-Ownership": {"ObjectWriter"}, "X-Amz-Acl": {"public-read"}}, status: 200, region: "us-east-1", acl: "public-read", ownerXM: "ObjectWriter"},
		{name: "ownership invalid", bucket: "b-own-bad", hdr: http.Header{"X-Amz-Object-Ownership": {"Nobody"}}, status: 400, code: "InvalidArgument"},
		{name: "ownership enforced with acl", bucket: "b-own-acl", hdr: http.Header{"X-Amz-Object-Ownership": {"BucketOwnerEnforced"}, "X-Amz-Acl": {"public-read"}}, status: 400, code: "InvalidBucketAclWithObjectOwnership"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.signed(http.MethodPut, "/"+tc.bucket, tc.body, tc.hdr)
			bktExpect(t, resp, tc.status, tc.code)
			bi, err := e.backend.GetBucket(ctx, tc.bucket)
			if tc.status != 200 {
				if err == nil {
					t.Fatalf("bucket %s was created despite the error", tc.bucket)
				}
				return
			}
			if got := resp.Header.Get("Location"); got != "/"+tc.bucket {
				t.Errorf("Location = %q", got)
			}
			if err != nil {
				t.Fatalf("GetBucket: %v", err)
			}
			if bi.Region != tc.region || bi.ObjectLockEnabled != tc.lock {
				t.Errorf("bucket = %+v, want region %q lock %v", bi, tc.region, tc.lock)
			}
			if tc.lock && bi.Versioning != storage.VersioningEnabled {
				t.Errorf("object lock bucket versioning = %q", bi.Versioning)
			}
			acl, err := e.backend.GetBucketConfig(ctx, tc.bucket, storage.ConfigACL)
			if tc.acl == "" {
				if !errors.Is(err, storage.ErrConfigNotFound) {
					t.Errorf("acl config = %q, %v; want none", acl, err)
				}
			} else if string(acl) != tc.acl || cannedACLFromConfig(acl) != tc.acl {
				t.Errorf("acl config = %q, %v; want %q", acl, err, tc.acl)
			}
			own, err := e.backend.GetBucketConfig(ctx, tc.bucket, storage.ConfigOwnership)
			if tc.ownerXM == "" {
				if !errors.Is(err, storage.ErrConfigNotFound) {
					t.Errorf("ownership config = %q, %v; want none", own, err)
				}
			} else {
				var doc struct {
					XMLName xml.Name
					Rule    []struct{ ObjectOwnership string }
				}
				if err != nil || xml.Unmarshal(own, &doc) != nil || doc.XMLName.Local != "OwnershipControls" ||
					doc.XMLName.Space != s3Namespace || len(doc.Rule) != 1 || doc.Rule[0].ObjectOwnership != tc.ownerXM {
					t.Errorf("ownership config = %s, %v", own, err)
				}
				if got := aclsDisabledByOwnership(own); got != (tc.ownerXM == "BucketOwnerEnforced") {
					t.Errorf("aclsDisabledByOwnership = %v", got)
				}
			}
		})
	}
}

func TestCreateBucketVirtualHostLocation(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) { s.Domains = []string{"s3.example.test"} }))
	resp := e.signed(http.MethodPut, "/", nil, http.Header{"Host": {"vhost-bucket.s3.example.test"}})
	bktExpect(t, resp, 200, "")
	if got := resp.Header.Get("Location"); got != "http://vhost-bucket.s3.example.test/" {
		t.Errorf("Location = %q", got)
	}
	if _, err := e.backend.GetBucket(context.Background(), "vhost-bucket"); err != nil {
		t.Errorf("bucket not created: %v", err)
	}
}

func TestCreateBucketOwnership(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	alice := e.createUser("alice", "readwrite")
	bob := e.createUser("bob", "readwrite")

	bktExpect(t, e.signedAs(alice, http.MethodPut, "/alice-bucket", nil, nil), 200, "")
	bi, err := e.backend.GetBucket(context.Background(), "alice-bucket")
	if err != nil || bi.Owner != "alice" {
		t.Fatalf("owner = %q, %v", bi.Owner, err)
	}
	bktExpect(t, e.signedAs(alice, http.MethodPut, "/alice-bucket", nil, nil), 409, "BucketAlreadyOwnedByYou")
	resp := e.signedAs(bob, http.MethodPut, "/alice-bucket", nil, nil)
	bktExpect(t, resp, 409, "BucketAlreadyExists")
	if !strings.Contains(string(resp.Data), "<BucketName>alice-bucket</BucketName>") {
		t.Errorf("missing BucketName: %s", resp.Data)
	}
	// Root owns the account and therefore every bucket.
	bktExpect(t, e.signed(http.MethodPut, "/alice-bucket", nil, nil), 409, "BucketAlreadyOwnedByYou")
	e.createBucket("root-bucket")
	bktExpect(t, e.signedAs(alice, http.MethodPut, "/root-bucket", nil, nil), 409, "BucketAlreadyExists")

	// Anonymous callers cannot create buckets.
	bktExpect(t, e.anon(http.MethodPut, "/anon-bucket", nil, nil), 403, "AccessDenied")
}

func TestCreateBucketExtraPermissions(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.putPolicy("create-only", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:CreateBucket"],"Resource":["arn:aws:s3:::*"]}]}`)
	e.putPolicy("create-acl", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:CreateBucket","s3:PutBucketAcl"],"Resource":["arn:aws:s3:::*"]}]}`)
	limited := e.createUser("limited", "create-only")
	withACL := e.createUser("withacl", "create-acl")
	ctx := context.Background()

	bktExpect(t, e.signedAs(limited, http.MethodPut, "/limited-plain", nil, nil), 200, "")
	for _, hdr := range []http.Header{
		{"X-Amz-Acl": {"public-read"}},
		{"X-Amz-Bucket-Object-Lock-Enabled": {"true"}},
		{"X-Amz-Object-Ownership": {"ObjectWriter"}},
	} {
		bktExpect(t, e.signedAs(limited, http.MethodPut, "/limited-extra", nil, hdr), 403, "AccessDenied")
		if _, err := e.backend.GetBucket(ctx, "limited-extra"); err == nil {
			t.Fatalf("bucket created despite AccessDenied (%v)", hdr)
		}
	}
	bktExpect(t, e.signedAs(withACL, http.MethodPut, "/withacl-bucket", nil, http.Header{"X-Amz-Acl": {"public-read"}}), 200, "")
}

func TestCreateBucketConcurrent(t *testing.T) {
	e := newTestEnv(t)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.client.CreateBucket(context.Background(), &awss3.CreateBucketInput{Bucket: aws.String("race-bucket")})
			switch {
			case err == nil:
				codes[i] = 200
			case bktAPICode(err) == "BucketAlreadyOwnedByYou":
				codes[i] = 409
			default:
				t.Errorf("CreateBucket: %v", err)
			}
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == 200 {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d creations succeeded, want 1 (%v)", ok, codes)
	}
}

// --- DeleteBucket ---

func TestDeleteBucket(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	t.Run("empty", func(t *testing.T) {
		e.createBucket("del-empty")
		e.putBucketConfig("del-empty", storage.ConfigACL, "public-read")
		resp := e.signed(http.MethodDelete, "/del-empty", nil, nil)
		bktExpect(t, resp, 204, "")
		if _, err := e.backend.GetBucket(ctx, "del-empty"); !errors.Is(err, storage.ErrBucketNotFound) {
			t.Errorf("bucket still exists: %v", err)
		}
		// Re-creating starts with no configuration.
		e.createBucket("del-empty")
		if _, err := e.backend.GetBucketConfig(ctx, "del-empty", storage.ConfigACL); !errors.Is(err, storage.ErrConfigNotFound) {
			t.Errorf("config survived deletion: %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		resp := e.signed(http.MethodDelete, "/del-missing", nil, nil)
		bktExpect(t, resp, 404, "NoSuchBucket")
		if !strings.Contains(string(resp.Data), "<BucketName>del-missing</BucketName>") {
			t.Errorf("missing BucketName: %s", resp.Data)
		}
	})
	t.Run("object", func(t *testing.T) {
		e.createBucket("del-object")
		e.putObject("del-object", "k", []byte("x"), storage.ObjectMeta{})
		bktExpect(t, e.signed(http.MethodDelete, "/del-object", nil, nil), 409, "BucketNotEmpty")
		if _, err := e.backend.DeleteObject(ctx, "del-object", "k", ""); err != nil {
			t.Fatal(err)
		}
		bktExpect(t, e.signed(http.MethodDelete, "/del-object", nil, nil), 204, "")
	})
	t.Run("noncurrent versions and delete markers", func(t *testing.T) {
		e.createBucket("del-versions")
		bktVersioned(t, e, "del-versions")
		v1 := e.putObject("del-versions", "k", []byte("1"), storage.ObjectMeta{})
		dm, err := e.backend.DeleteObject(ctx, "del-versions", "k", "")
		if err != nil || !dm.DeleteMarker {
			t.Fatalf("delete marker: %+v %v", dm, err)
		}
		// Only a noncurrent version and a delete marker remain: no current
		// objects, but the bucket is still not empty.
		bktExpect(t, e.signed(http.MethodDelete, "/del-versions", nil, nil), 409, "BucketNotEmpty")
		if _, err := e.backend.DeleteObject(ctx, "del-versions", "k", v1.VersionID); err != nil {
			t.Fatal(err)
		}
		// Only the delete marker remains.
		bktExpect(t, e.signed(http.MethodDelete, "/del-versions", nil, nil), 409, "BucketNotEmpty")
		if _, err := e.backend.DeleteObject(ctx, "del-versions", "k", dm.VersionID); err != nil {
			t.Fatal(err)
		}
		bktExpect(t, e.signed(http.MethodDelete, "/del-versions", nil, nil), 204, "")
	})
	t.Run("multipart upload", func(t *testing.T) {
		e.createBucket("del-upload")
		up, err := e.backend.CreateMultipartUpload(ctx, "del-upload", "k", storage.ObjectMeta{}, "")
		if err != nil {
			t.Fatal(err)
		}
		bktExpect(t, e.signed(http.MethodDelete, "/del-upload", nil, nil), 409, "BucketNotEmpty")
		if err := e.backend.AbortMultipartUpload(ctx, "del-upload", "k", up.UploadID); err != nil {
			t.Fatal(err)
		}
		bktExpect(t, e.signed(http.MethodDelete, "/del-upload", nil, nil), 204, "")
	})
	t.Run("sdk", func(t *testing.T) {
		e.createBucket("del-sdk")
		if _, err := e.client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String("del-sdk")}); err != nil {
			t.Fatalf("DeleteBucket: %v", err)
		}
		_, err := e.client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String("del-sdk")})
		if bktAPICode(err) != "NoSuchBucket" {
			t.Errorf("second delete: %v", err)
		}
	})
}

// --- HeadBucket / GetBucketLocation ---

func TestHeadBucket(t *testing.T) {
	e := newTestEnv(t, withSettings(func(s *config.Settings) { s.Region = "eu-central-1" }))
	ctx := context.Background()
	e.createBucket("head-bucket") // region us-east-1
	if err := e.backend.CreateBucket(ctx, "head-noregion", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}

	resp := e.signed(http.MethodHead, "/head-bucket", nil, nil)
	bktExpect(t, resp, 200, "")
	if got := resp.Header.Get("X-Amz-Bucket-Region"); got != "us-east-1" {
		t.Errorf("region = %q", got)
	}
	if got := resp.Header.Get("X-Amz-Access-Point-Alias"); got != "false" {
		t.Errorf("alias = %q", got)
	}
	if len(resp.Data) != 0 {
		t.Errorf("HEAD body: %q", resp.Data)
	}
	// A bucket without a recorded region is in the server region.
	resp = e.signed(http.MethodHead, "/head-noregion", nil, nil)
	if got := resp.Header.Get("X-Amz-Bucket-Region"); resp.StatusCode != 200 || got != "eu-central-1" {
		t.Errorf("noregion: %d %q", resp.StatusCode, got)
	}

	resp = e.signed(http.MethodHead, "/head-missing", nil, nil)
	if resp.StatusCode != 404 || len(resp.Data) != 0 {
		t.Errorf("missing: %d %q", resp.StatusCode, resp.Data)
	}
	_, err := e.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String("head-missing")})
	var nf *types.NotFound
	if !errors.As(err, &nf) {
		t.Errorf("SDK HeadBucket missing: %v", err)
	}

	requireAuthz(t, e)
	resp = e.anon(http.MethodHead, "/head-bucket", nil, nil)
	if resp.StatusCode != 403 || len(resp.Data) != 0 {
		t.Errorf("anonymous: %d %q", resp.StatusCode, resp.Data)
	}
}

func TestGetBucketLocationRaw(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.createBucket("loc-east")
	if err := e.backend.CreateBucket(ctx, "loc-west", storage.CreateBucketOptions{Region: "us-west-2"}); err != nil {
		t.Fatal(err)
	}
	resp := e.signed(http.MethodGet, "/loc-east?location", nil, nil)
	bktExpect(t, resp, 200, "")
	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`
	if string(resp.Data) != want {
		t.Errorf("body = %s", resp.Data)
	}
	resp = e.signed(http.MethodGet, "/loc-west?location", nil, nil)
	if !strings.Contains(string(resp.Data), `>us-west-2</LocationConstraint>`) {
		t.Errorf("body = %s", resp.Data)
	}
	bktExpect(t, e.signed(http.MethodGet, "/loc-missing?location", nil, nil), 404, "NoSuchBucket")
}

// --- ListBuckets ---

func TestListBuckets(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	var names []string
	for i := 0; i < 23; i++ {
		name := fmt.Sprintf("list-%02d", i)
		region := "us-east-1"
		if i%3 == 0 {
			region = "eu-west-1"
		}
		if err := e.backend.CreateBucket(ctx, name, storage.CreateBucketOptions{Region: region}); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	e.createBucket("other-bucket")
	all := append(append([]string{}, names...), "other-bucket")
	sort.Strings(all)

	out, err := e.client.ListBuckets(ctx, &awss3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	var got []string
	for _, b := range out.Buckets {
		got = append(got, aws.ToString(b.Name))
		if b.CreationDate == nil || b.CreationDate.IsZero() || aws.ToString(b.BucketRegion) == "" {
			t.Errorf("bucket %+v incomplete", b)
		}
	}
	if strings.Join(got, ",") != strings.Join(all, ",") {
		t.Errorf("buckets = %v, want %v", got, all)
	}
	if out.Owner == nil || aws.ToString(out.Owner.DisplayName) != "root" || aws.ToString(out.Owner.ID) != ownerFor("").ID {
		t.Errorf("owner = %+v", out.Owner)
	}
	if out.ContinuationToken != nil {
		t.Errorf("unexpected token %q", aws.ToString(out.ContinuationToken))
	}

	for _, page := range []int32{1, 2, 5, 7, 23, 24, 100} {
		for _, filter := range []struct{ prefix, region string }{{"", ""}, {"list-1", ""}, {"", "eu-west-1"}, {"list-", "EU-WEST-1"}} {
			var want []string
			for _, n := range all {
				bi, _ := e.backend.GetBucket(ctx, n)
				if strings.HasPrefix(n, filter.prefix) && (filter.region == "" || strings.EqualFold(bi.Region, filter.region)) {
					want = append(want, n)
				}
			}
			in := &awss3.ListBucketsInput{MaxBuckets: aws.Int32(page)}
			if filter.prefix != "" {
				in.Prefix = aws.String(filter.prefix)
			}
			if filter.region != "" {
				in.BucketRegion = aws.String(filter.region)
			}
			p := awss3.NewListBucketsPaginator(e.client, in)
			var got []string
			pages := 0
			for p.HasMorePages() {
				out, err := p.NextPage(ctx)
				if err != nil {
					t.Fatalf("page: %v", err)
				}
				pages++
				if len(out.Buckets) > int(page) {
					t.Fatalf("page of %d > max %d", len(out.Buckets), page)
				}
				if aws.ToString(out.Prefix) != filter.prefix {
					t.Errorf("Prefix = %q", aws.ToString(out.Prefix))
				}
				for _, b := range out.Buckets {
					got = append(got, aws.ToString(b.Name))
				}
				if pages > 100 {
					t.Fatal("pagination does not terminate")
				}
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("max=%d %+v: got %v, want %v", page, filter, got, want)
			}
			if wantPages := (len(want) + int(page) - 1) / int(page); len(want) > 0 && pages != wantPages {
				t.Errorf("max=%d %+v: %d pages, want %d", page, filter, pages, wantPages)
			}
		}
	}
}

func TestListBucketsRaw(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("raw-a")
	e.createBucket("raw-b")
	resp := e.signed(http.MethodGet, "/?max-buckets=1", nil, nil)
	bktExpect(t, resp, 200, "")
	if got := bktElementOrder(t, resp.Data); strings.Join(got, ",") != "Owner,Buckets,ContinuationToken" {
		t.Errorf("elements = %v; body %s", got, resp.Data)
	}
	var doc struct {
		XMLName xml.Name
		Buckets []struct {
			Name, CreationDate, BucketRegion string
		} `xml:"Buckets>Bucket"`
		ContinuationToken string
	}
	if err := xml.Unmarshal(resp.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.XMLName.Space != s3Namespace || doc.XMLName.Local != "ListAllMyBucketsResult" {
		t.Errorf("root = %v", doc.XMLName)
	}
	if len(doc.Buckets) != 1 || doc.Buckets[0].Name != "raw-a" || doc.Buckets[0].BucketRegion != "us-east-1" ||
		!strings.HasSuffix(doc.Buckets[0].CreationDate, "Z") || len(doc.Buckets[0].CreationDate) != len("2006-01-02T15:04:05.000Z") {
		t.Errorf("buckets = %+v", doc.Buckets)
	}
	resp = e.signed(http.MethodGet, "/?max-buckets=1&continuation-token="+url.QueryEscape(doc.ContinuationToken), nil, nil)
	if !strings.Contains(string(resp.Data), "<Name>raw-b</Name>") || strings.Contains(string(resp.Data), "ContinuationToken") {
		t.Errorf("second page: %s", resp.Data)
	}

	// No buckets: an empty <Buckets> element.
	e2 := newTestEnv(t)
	resp = e2.signed(http.MethodGet, "/", nil, nil)
	if !strings.Contains(string(resp.Data), "<Buckets></Buckets>") {
		t.Errorf("empty list: %s", resp.Data)
	}

	for _, q := range []string{"max-buckets=0", "max-buckets=10001", "max-buckets=abc", "max-buckets=-1", "continuation-token=", "continuation-token=%21%21%21"} {
		bktExpect(t, e.signed(http.MethodGet, "/?"+q, nil, nil), 400, "InvalidArgument")
	}

	requireAuthz(t, e)
	bktExpect(t, e.anon(http.MethodGet, "/", nil, nil), 403, "AccessDenied")
}

func TestContinuationTokenRoundTrip(t *testing.T) {
	for _, s := range append([]string{"a", "x/y/", "\xff\xfe", strings.Repeat("k", 1024)}, bktSpecialKeys...) {
		tok := encodeContinuationToken(s)
		if strings.ContainsAny(tok, "+/= ") {
			t.Errorf("token %q is not URL-safe", tok)
		}
		got, err := decodeContinuationToken(tok)
		if err != nil || got != s {
			t.Errorf("round trip %q: %q, %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "!!!", strings.Repeat("A", maxContinuationToken+1), "a"} {
		if _, err := decodeContinuationToken(bad); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	// Standard base64 is accepted as well.
	if got, err := decodeContinuationToken("aGk/Pz8="); err != nil || got != "hi???" {
		t.Errorf("std base64: %q, %v", got, err)
	}
}

func TestValidLocationConstraint(t *testing.T) {
	for v, want := range map[string]bool{
		"us-east-1": true, "eu-west-1": true, "garage": true, "a": true, "cn-north-1": true,
		"": false, "1region": false, "us-east-": false, "US-EAST-1": false, "us_east_1": false,
		"us east": false, strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
	} {
		if got := validLocationConstraint(v); got != want {
			t.Errorf("validLocationConstraint(%q) = %v", v, got)
		}
	}
}

// --- ListObjects (V1) ---

// bktPopulate creates keys in a fresh bucket.
func bktPopulate(t *testing.T, e *testEnv, bucket string, keys []string) {
	t.Helper()
	e.createBucket(bucket)
	for _, k := range keys {
		e.putObject(bucket, k, []byte("data:"+k), storage.ObjectMeta{})
	}
}

// bktTreeKeys is a directory-like key set.
var bktTreeKeys = []string{
	"a.txt", "b.txt",
	"dir1/x", "dir1/y", "dir1/sub/z",
	"dir2/x",
	"dir3/", "dir3/a", "dir3/b/c",
	"photos/2024/jan/1.jpg", "photos/2024/feb/2.jpg", "photos/2025/mar/3.jpg",
	"z-last",
}

// bktExpected computes the listing S3 should return for keys.
func bktExpected(keys []string, prefix, delimiter, after string) (objs []string, cps []string) {
	sorted := append([]string{}, keys...)
	sort.Strings(sorted)
	seen := map[string]bool{}
	for _, k := range sorted {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if cp, ok := storage.CommonPrefix(k, prefix, delimiter); ok {
			if cp > after && !seen[cp] && !strings.HasPrefix(after, cp) {
				seen[cp] = true
				cps = append(cps, cp)
			}
			continue
		}
		if k > after {
			objs = append(objs, k)
		}
	}
	return objs, cps
}

func TestListObjectsV1Pagination(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	bktPopulate(t, e, "v1-bucket", bktTreeKeys)

	for _, delim := range []string{"", "/"} {
		for _, prefix := range []string{"", "dir", "dir1/", "photos/", "nothing"} {
			wantObjs, wantCPs := bktExpected(bktTreeKeys, prefix, delim, "")
			for _, maxKeys := range []int32{1, 2, 3, 1000} {
				var gotObjs, gotCPs []string
				marker := ""
				for i := 0; ; i++ {
					if i > 50 {
						t.Fatal("pagination does not terminate")
					}
					in := &awss3.ListObjectsInput{Bucket: aws.String("v1-bucket"), MaxKeys: aws.Int32(maxKeys)}
					if prefix != "" {
						in.Prefix = aws.String(prefix)
					}
					if delim != "" {
						in.Delimiter = aws.String(delim)
					}
					if marker != "" {
						in.Marker = aws.String(marker)
					}
					out, err := e.client.ListObjects(ctx, in)
					if err != nil {
						t.Fatalf("ListObjects: %v", err)
					}
					if n := len(out.Contents) + len(out.CommonPrefixes); n > int(maxKeys) {
						t.Fatalf("%d entries > max-keys %d", n, maxKeys)
					}
					if aws.ToString(out.Marker) != marker || aws.ToString(out.Prefix) != prefix || aws.ToInt32(out.MaxKeys) != maxKeys {
						t.Errorf("echo: marker %q prefix %q max %d", aws.ToString(out.Marker), aws.ToString(out.Prefix), aws.ToInt32(out.MaxKeys))
					}
					last := ""
					for _, o := range out.Contents {
						gotObjs = append(gotObjs, aws.ToString(o.Key))
						last = aws.ToString(o.Key)
						if o.Owner == nil || aws.ToString(o.Owner.DisplayName) != "root" {
							t.Errorf("V1 owner missing: %+v", o.Owner)
						}
						if o.StorageClass != types.ObjectStorageClassStandard || aws.ToInt64(o.Size) != int64(len("data:"+aws.ToString(o.Key))) {
							t.Errorf("object %+v", o)
						}
						if want := `"` + md5Hex([]byte("data:"+aws.ToString(o.Key))) + `"`; aws.ToString(o.ETag) != want {
							t.Errorf("ETag = %s, want %s", aws.ToString(o.ETag), want)
						}
					}
					for _, cp := range out.CommonPrefixes {
						gotCPs = append(gotCPs, aws.ToString(cp.Prefix))
						if aws.ToString(cp.Prefix) > last {
							last = aws.ToString(cp.Prefix)
						}
					}
					truncated := aws.ToBool(out.IsTruncated)
					switch {
					case !truncated && out.NextMarker != nil:
						t.Errorf("NextMarker %q on untruncated page", aws.ToString(out.NextMarker))
					case truncated && delim == "" && out.NextMarker != nil:
						t.Errorf("NextMarker without delimiter")
					case truncated && delim != "" && aws.ToString(out.NextMarker) != last:
						t.Errorf("NextMarker = %q, want %q", aws.ToString(out.NextMarker), last)
					}
					if !truncated {
						break
					}
					marker = last
				}
				if strings.Join(gotObjs, ",") != strings.Join(wantObjs, ",") || strings.Join(gotCPs, ",") != strings.Join(wantCPs, ",") {
					t.Errorf("prefix=%q delim=%q max=%d: got %v %v, want %v %v", prefix, delim, maxKeys, gotObjs, gotCPs, wantObjs, wantCPs)
				}
			}
		}
	}
}

func TestListObjectsV1Raw(t *testing.T) {
	e := newTestEnv(t)
	bktPopulate(t, e, "v1-raw", []string{"a/1", "a/2", "b", "c"})

	resp := e.signed(http.MethodGet, "/v1-raw?delimiter=/&max-keys=2", nil, nil)
	bktExpect(t, resp, 200, "")
	order := bktCompact(bktElementOrder(t, resp.Data))
	if want := "Name,Prefix,Marker,NextMarker,MaxKeys,Delimiter,IsTruncated,Contents,CommonPrefixes"; strings.Join(order, ",") != want {
		// Contents (b) sorts after the common prefix a/, so the order of
		// the two groups is fixed by the struct: Contents then CommonPrefixes.
		t.Errorf("elements = %v", order)
	}
	var doc struct {
		XMLName     xml.Name
		NextMarker  string
		IsTruncated bool
		MaxKeys     int
		Contents    []struct {
			Key, ETag, StorageClass string
			Owner                   *struct{ ID, DisplayName string }
		}
		CommonPrefixes []struct{ Prefix string }
	}
	if err := xml.Unmarshal(resp.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.XMLName.Space != s3Namespace || !doc.IsTruncated || doc.NextMarker != "b" || doc.MaxKeys != 2 ||
		len(doc.Contents) != 1 || doc.Contents[0].Key != "b" || doc.Contents[0].Owner == nil ||
		len(doc.CommonPrefixes) != 1 || doc.CommonPrefixes[0].Prefix != "a/" {
		t.Errorf("doc = %+v", doc)
	}

	// Without delimiter: no NextMarker, no Delimiter element.
	resp = e.signed(http.MethodGet, "/v1-raw?max-keys=1", nil, nil)
	if s := string(resp.Data); strings.Contains(s, "NextMarker") || strings.Contains(s, "<Delimiter>") || !strings.Contains(s, "<IsTruncated>true</IsTruncated>") {
		t.Errorf("no-delimiter page: %s", s)
	}
	// Contents element order.
	if s := string(resp.Data); !bktInOrder(s, "<Key>", "<LastModified>", "<ETag>", "<Size>", "<Owner>", "<StorageClass>") {
		t.Errorf("Contents element order: %s", s)
	}

	// max-keys=0: empty and not truncated; the bucket must still exist.
	resp = e.signed(http.MethodGet, "/v1-raw?max-keys=0", nil, nil)
	if s := string(resp.Data); resp.StatusCode != 200 || strings.Contains(s, "<Contents>") || !strings.Contains(s, "<IsTruncated>false</IsTruncated>") || !strings.Contains(s, "<MaxKeys>0</MaxKeys>") {
		t.Errorf("max-keys=0: %s", s)
	}
	bktExpect(t, e.signed(http.MethodGet, "/missing-bucket?max-keys=0", nil, nil), 404, "NoSuchBucket")
	// max-keys above 1000 is clamped.
	resp = e.signed(http.MethodGet, "/v1-raw?max-keys=5000", nil, nil)
	if !strings.Contains(string(resp.Data), "<MaxKeys>1000</MaxKeys>") {
		t.Errorf("clamp: %s", resp.Data)
	}
	for _, q := range []string{"max-keys=-1", "max-keys=abc", "max-keys=1.5", "encoding-type=base64"} {
		bktExpect(t, e.signed(http.MethodGet, "/v1-raw?"+q, nil, nil), 400, "InvalidArgument")
	}
	// Empty bucket.
	e.createBucket("v1-empty")
	resp = e.signed(http.MethodGet, "/v1-empty", nil, nil)
	if s := string(resp.Data); strings.Contains(s, "<Contents>") || !strings.Contains(s, "<Name>v1-empty</Name><Prefix></Prefix><Marker></Marker><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>") {
		t.Errorf("empty bucket: %s", s)
	}
}

// bktInOrder reports whether the substrings appear in s in order.
func bktInOrder(s string, parts ...string) bool {
	pos := 0
	for _, p := range parts {
		i := strings.Index(s[pos:], p)
		if i < 0 {
			return false
		}
		pos += i + len(p)
	}
	return true
}

// --- encoding-type=url ---

func TestListEncodingTypeURL(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	bktPopulate(t, e, "enc-bucket", bktSpecialKeys)
	bktVersioned(t, e, "enc-bucket")
	sorted := append([]string{}, bktSpecialKeys...)
	sort.Strings(sorted)

	unescape := func(s string) string {
		t.Helper()
		u, err := url.QueryUnescape(s)
		if err != nil {
			t.Fatalf("unescape %q: %v", s, err)
		}
		return u
	}

	t.Run("v2", func(t *testing.T) {
		var got []string
		p := awss3.NewListObjectsV2Paginator(e.client, &awss3.ListObjectsV2Input{
			Bucket: aws.String("enc-bucket"), EncodingType: types.EncodingTypeUrl, MaxKeys: aws.Int32(3),
		})
		for p.HasMorePages() {
			out, err := p.NextPage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if out.EncodingType != types.EncodingTypeUrl {
				t.Errorf("EncodingType = %q", out.EncodingType)
			}
			for _, o := range out.Contents {
				k := aws.ToString(o.Key)
				if strings.ContainsAny(k, " \t\n\x01&<>") {
					t.Errorf("key %q not encoded", k)
				}
				got = append(got, unescape(k))
			}
		}
		if strings.Join(got, "|") != strings.Join(sorted, "|") {
			t.Errorf("keys = %q\nwant %q", got, sorted)
		}
	})
	t.Run("v1", func(t *testing.T) {
		var got []string
		marker := ""
		for i := 0; i < 50; i++ {
			out, err := e.client.ListObjects(ctx, &awss3.ListObjectsInput{
				Bucket: aws.String("enc-bucket"), EncodingType: types.EncodingTypeUrl, MaxKeys: aws.Int32(4), Marker: aws.String(marker),
			})
			if err != nil {
				t.Fatal(err)
			}
			if m := aws.ToString(out.Marker); unescape(m) != marker {
				t.Errorf("Marker echo = %q for %q", m, marker)
			}
			for _, o := range out.Contents {
				got = append(got, unescape(aws.ToString(o.Key)))
			}
			if !aws.ToBool(out.IsTruncated) {
				break
			}
			marker = got[len(got)-1]
		}
		if strings.Join(got, "|") != strings.Join(sorted, "|") {
			t.Errorf("keys = %q\nwant %q", got, sorted)
		}
	})
	t.Run("versions", func(t *testing.T) {
		var got []string
		km, vm := "", ""
		for i := 0; i < 50; i++ {
			in := &awss3.ListObjectVersionsInput{Bucket: aws.String("enc-bucket"), EncodingType: types.EncodingTypeUrl, MaxKeys: aws.Int32(5)}
			if km != "" {
				in.KeyMarker, in.VersionIdMarker = aws.String(km), aws.String(vm)
			}
			out, err := e.client.ListObjectVersions(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if unescape(aws.ToString(out.KeyMarker)) != km {
				t.Errorf("KeyMarker echo = %q for %q", aws.ToString(out.KeyMarker), km)
			}
			for _, v := range out.Versions {
				got = append(got, unescape(aws.ToString(v.Key)))
				if aws.ToString(v.VersionId) != "null" {
					t.Errorf("version id = %q", aws.ToString(v.VersionId))
				}
			}
			if !aws.ToBool(out.IsTruncated) {
				break
			}
			km, vm = unescape(aws.ToString(out.NextKeyMarker)), aws.ToString(out.NextVersionIdMarker)
		}
		if strings.Join(got, "|") != strings.Join(sorted, "|") {
			t.Errorf("keys = %q\nwant %q", got, sorted)
		}
	})
	t.Run("raw elements", func(t *testing.T) {
		q := "?list-type=2&encoding-type=url&prefix=" + url.QueryEscape("dir/sub dir/") + "&delimiter=" + url.QueryEscape("?") + "&start-after=" + url.QueryEscape("dir/a b")
		resp := e.signed(http.MethodGet, "/enc-bucket"+q, nil, nil)
		bktExpect(t, resp, 200, "")
		s := string(resp.Data)
		for _, want := range []string{
			"<Prefix>dir/sub+dir/</Prefix>",
			"<Delimiter>%3F</Delimiter>",
			"<StartAfter>dir/a+b</StartAfter>",
			"<EncodingType>url</EncodingType>",
			"<CommonPrefixes><Prefix>dir/sub+dir/file%3F</Prefix></CommonPrefixes>",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in %s", want, s)
			}
		}
		resp = e.signed(http.MethodGet, "/enc-bucket?encoding-type=URL&prefix=ctrl&marker="+url.QueryEscape("c d"), nil, nil)
		s = string(resp.Data)
		for _, want := range []string{"<Key>ctrl%01char</Key>", "<Marker>c+d</Marker>", "<Prefix>ctrl</Prefix>", "<EncodingType>url</EncodingType>"} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in %s", want, s)
			}
		}
		resp = e.signed(http.MethodGet, "/enc-bucket?versions&encoding-type=url&prefix=amp&key-marker="+url.QueryEscape("a&b"), nil, nil)
		s = string(resp.Data)
		for _, want := range []string{"<Key>amp%26lt%3Cgt%3E</Key>", "<KeyMarker>a%26b</KeyMarker>", "<VersionIdMarker></VersionIdMarker>"} {
			if !strings.Contains(s, want) {
				t.Errorf("missing %s in %s", want, s)
			}
		}
	})
	t.Run("without encoding", func(t *testing.T) {
		out, err := e.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("enc-bucket"), Prefix: aws.String("amp")})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Contents) != 1 || aws.ToString(out.Contents[0].Key) != "amp&lt<gt>" {
			t.Errorf("contents = %+v", out.Contents)
		}
	})
}

// --- ListObjectsV2 ---

func TestListObjectsV2Pagination(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	bktPopulate(t, e, "v2-bucket", bktTreeKeys)

	for _, delim := range []string{"", "/", "/2"} {
		for _, prefix := range []string{"", "dir", "photos/", "photos/2024/"} {
			for _, startAfter := range []string{"", "b.txt", "dir1/", "dir2/x", "zzz"} {
				wantObjs, wantCPs := bktExpected(bktTreeKeys, prefix, delim, startAfter)
				for _, maxKeys := range []int32{1, 2, 4, 1000} {
					in := &awss3.ListObjectsV2Input{Bucket: aws.String("v2-bucket"), MaxKeys: aws.Int32(maxKeys)}
					if prefix != "" {
						in.Prefix = aws.String(prefix)
					}
					if delim != "" {
						in.Delimiter = aws.String(delim)
					}
					if startAfter != "" {
						in.StartAfter = aws.String(startAfter)
					}
					p := awss3.NewListObjectsV2Paginator(e.client, in)
					var gotObjs, gotCPs []string
					for pages := 0; p.HasMorePages(); pages++ {
						if pages > 50 {
							t.Fatal("pagination does not terminate")
						}
						out, err := p.NextPage(ctx)
						if err != nil {
							t.Fatalf("ListObjectsV2: %v", err)
						}
						n := len(out.Contents) + len(out.CommonPrefixes)
						if int(aws.ToInt32(out.KeyCount)) != n || n > int(maxKeys) {
							t.Fatalf("KeyCount %d, entries %d, max %d", aws.ToInt32(out.KeyCount), n, maxKeys)
						}
						if aws.ToBool(out.IsTruncated) != (out.NextContinuationToken != nil) {
							t.Errorf("IsTruncated %v with token %v", aws.ToBool(out.IsTruncated), out.NextContinuationToken)
						}
						if aws.ToString(out.StartAfter) != startAfter {
							t.Errorf("StartAfter echo %q", aws.ToString(out.StartAfter))
						}
						for _, o := range out.Contents {
							gotObjs = append(gotObjs, aws.ToString(o.Key))
							if o.Owner != nil {
								t.Errorf("Owner without fetch-owner")
							}
						}
						for _, cp := range out.CommonPrefixes {
							gotCPs = append(gotCPs, aws.ToString(cp.Prefix))
						}
					}
					if strings.Join(gotObjs, ",") != strings.Join(wantObjs, ",") || strings.Join(gotCPs, ",") != strings.Join(wantCPs, ",") {
						t.Errorf("prefix=%q delim=%q after=%q max=%d: got %v %v, want %v %v", prefix, delim, startAfter, maxKeys, gotObjs, gotCPs, wantObjs, wantCPs)
					}
				}
			}
		}
	}
}

func TestListObjectsV2Raw(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	bktPopulate(t, e, "v2-raw", []string{"a/1", "a/2", "b", "c"})

	resp := e.signed(http.MethodGet, "/v2-raw?list-type=2&delimiter=/&max-keys=2&fetch-owner=true&start-after=0", nil, nil)
	bktExpect(t, resp, 200, "")
	order := bktCompact(bktElementOrder(t, resp.Data))
	if want := "Name,Prefix,KeyCount,MaxKeys,Delimiter,IsTruncated,NextContinuationToken,StartAfter,Contents,CommonPrefixes"; strings.Join(order, ",") != want {
		t.Errorf("elements = %v", order)
	}
	var doc struct {
		KeyCount              int
		IsTruncated           bool
		NextContinuationToken string
		Contents              []struct {
			Key   string
			Owner *struct{ ID, DisplayName string }
		}
	}
	if err := xml.Unmarshal(resp.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.KeyCount != 2 || !doc.IsTruncated || doc.NextContinuationToken == "" || len(doc.Contents) != 1 ||
		doc.Contents[0].Owner == nil || doc.Contents[0].Owner.DisplayName != "root" {
		t.Errorf("doc = %+v", doc)
	}
	// The token resumes after the last entry and is echoed.
	tok := doc.NextContinuationToken
	resp = e.signed(http.MethodGet, "/v2-raw?list-type=2&delimiter=/&continuation-token="+url.QueryEscape(tok), nil, nil)
	s := string(resp.Data)
	if !strings.Contains(s, "<ContinuationToken>"+tok+"</ContinuationToken>") || !strings.Contains(s, "<Key>c</Key>") ||
		strings.Contains(s, "<Key>b</Key>") || strings.Contains(s, "NextContinuationToken") || !strings.Contains(s, "<KeyCount>1</KeyCount>") {
		t.Errorf("second page: %s", s)
	}
	// The token takes precedence over start-after.
	resp = e.signed(http.MethodGet, "/v2-raw?list-type=2&start-after=a&continuation-token="+url.QueryEscape(encodeContinuationToken("b")), nil, nil)
	if s := string(resp.Data); strings.Contains(s, "<Key>a/1</Key>") || !strings.Contains(s, "<Key>c</Key>") || !strings.Contains(s, "<StartAfter>a</StartAfter>") {
		t.Errorf("token vs start-after: %s", s)
	}

	for _, q := range []string{"continuation-token=", "continuation-token=%25%25", "fetch-owner=maybe", "max-keys=x", "encoding-type=xml"} {
		bktExpect(t, e.signed(http.MethodGet, "/v2-raw?list-type=2&"+q, nil, nil), 400, "InvalidArgument")
	}
	_, err := e.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("v2-raw"), ContinuationToken: aws.String("@@@")})
	if bktAPICode(err) != "InvalidArgument" {
		t.Errorf("bad token: %v", err)
	}

	resp = e.signed(http.MethodGet, "/v2-raw?list-type=2&max-keys=0", nil, nil)
	if s := string(resp.Data); !strings.Contains(s, "<KeyCount>0</KeyCount>") || !strings.Contains(s, "<IsTruncated>false</IsTruncated>") || strings.Contains(s, "<Contents>") {
		t.Errorf("max-keys=0: %s", s)
	}
	bktExpect(t, e.signed(http.MethodGet, "/v2-missing?list-type=2", nil, nil), 404, "NoSuchBucket")

	out, err := e.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("v2-raw"), FetchOwner: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out.Contents {
		if o.Owner == nil || aws.ToString(o.Owner.ID) != ownerFor("").ID {
			t.Errorf("fetch-owner: %+v", o.Owner)
		}
	}
}

func TestListObjectsChecksums(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.createBucket("ck-bucket")
	e.putObject("ck-bucket", "none", []byte("x"), storage.ObjectMeta{})
	e.putObject("ck-bucket", "single", []byte("x"), storage.ObjectMeta{Checksums: map[string]string{ChecksumCRC32: computeChecksum(ChecksumCRC32, []byte("x"))}})
	e.putObject("ck-bucket", "composite", []byte("x"), storage.ObjectMeta{
		ETag:         "0123456789abcdef0123456789abcdef-2",
		Checksums:    map[string]string{ChecksumSHA256: "AAAA-2"},
		ChecksumType: ChecksumTypeComposite,
		StorageClass: "GLACIER_IR",
	})
	e.putObject("ck-bucket", "multi", []byte("x"), storage.ObjectMeta{Checksums: map[string]string{
		ChecksumSHA1: computeChecksum(ChecksumSHA1, []byte("x")), ChecksumCRC64NVME: computeChecksum(ChecksumCRC64NVME, []byte("x")),
	}, ChecksumType: ChecksumTypeFullObject})

	out, err := e.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("ck-bucket")})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range out.Contents {
		var algos []string
		for _, a := range o.ChecksumAlgorithm {
			algos = append(algos, string(a))
		}
		got[aws.ToString(o.Key)] = strings.Join(algos, "+") + "/" + string(o.ChecksumType) + "/" + string(o.StorageClass) + "/" + aws.ToString(o.ETag)
	}
	want := map[string]string{
		"none":      `/` + `/STANDARD/"` + md5Hex([]byte("x")) + `"`,
		"single":    `CRC32/FULL_OBJECT/STANDARD/"` + md5Hex([]byte("x")) + `"`,
		"composite": `SHA256/COMPOSITE/GLACIER_IR/"0123456789abcdef0123456789abcdef-2"`,
		"multi":     `CRC64NVME+SHA1/FULL_OBJECT/STANDARD/"` + md5Hex([]byte("x")) + `"`,
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %s, want %s", k, got[k], w)
		}
	}
	resp := e.signed(http.MethodGet, "/ck-bucket?prefix=single", nil, nil)
	if !bktInOrder(string(resp.Data), "<Key>", "<LastModified>", "<ETag>", "<ChecksumAlgorithm>CRC32</ChecksumAlgorithm>", "<ChecksumType>FULL_OBJECT</ChecksumType>", "<Size>", "<Owner>", "<StorageClass>") {
		t.Errorf("element order: %s", resp.Data)
	}
}

// --- ListObjectVersions ---

// bktVersionRec is one expected version listing entry.
type bktVersionRec struct {
	key, vid string
	marker   bool
	latest   bool
}

func (v bktVersionRec) String() string {
	return fmt.Sprintf("%s@%s(dm=%v,latest=%v)", v.key, v.vid, v.marker, v.latest)
}

func TestListObjectVersionsPagination(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.createBucket("ver-bucket")
	// A null version written before versioning was enabled.
	e.putObject("ver-bucket", "a", []byte("a-null"), storage.ObjectMeta{})
	bktVersioned(t, e, "ver-bucket")
	for i := 0; i < 3; i++ {
		e.putObject("ver-bucket", "a", []byte(fmt.Sprintf("a%d", i)), storage.ObjectMeta{})
	}
	e.putObject("ver-bucket", "b/1", []byte("b1"), storage.ObjectMeta{})
	if _, err := e.backend.DeleteObject(ctx, "ver-bucket", "b/1", ""); err != nil {
		t.Fatal(err)
	}
	e.putObject("ver-bucket", "b/2", []byte("b2"), storage.ObjectMeta{})
	e.putObject("ver-bucket", "c", []byte("c"), storage.ObjectMeta{})
	if _, err := e.backend.DeleteObject(ctx, "ver-bucket", "c", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.backend.DeleteObject(ctx, "ver-bucket", "c", ""); err != nil {
		t.Fatal(err)
	}
	e.putObject("ver-bucket", "d", []byte("d"), storage.ObjectMeta{})

	// Reference listing straight from the backend.
	var all []bktVersionRec
	if err := e.backend.WalkVersions(ctx, "ver-bucket", "", "", "", func(o storage.ObjectInfo) error {
		all = append(all, bktVersionRec{o.Key, o.VersionID, o.DeleteMarker, o.IsLatest})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(all) != 11 {
		t.Fatalf("setup: %d versions: %v", len(all), all)
	}

	for _, delim := range []string{"", "/"} {
		var want []bktVersionRec
		var wantCPs []string
		for _, v := range all {
			if cp, ok := storage.CommonPrefix(v.key, "", delim); ok {
				if len(wantCPs) == 0 || wantCPs[len(wantCPs)-1] != cp {
					wantCPs = append(wantCPs, cp)
				}
				continue
			}
			want = append(want, v)
		}
		for _, maxKeys := range []int32{1, 2, 3, 1000} {
			var got []bktVersionRec
			var gotCPs []string
			km, vm := "", ""
			for i := 0; ; i++ {
				if i > 50 {
					t.Fatal("pagination does not terminate")
				}
				in := &awss3.ListObjectVersionsInput{Bucket: aws.String("ver-bucket"), MaxKeys: aws.Int32(maxKeys)}
				if delim != "" {
					in.Delimiter = aws.String(delim)
				}
				if km != "" {
					in.KeyMarker = aws.String(km)
					if vm != "" {
						in.VersionIdMarker = aws.String(vm)
					}
				}
				q := url.Values{"versions": {""}, "max-keys": {fmt.Sprint(maxKeys)}}
				if delim != "" {
					q.Set("delimiter", delim)
				}
				if km != "" {
					q.Set("key-marker", km)
				}
				if vm != "" {
					q.Set("version-id-marker", vm)
				}
				// The SDK splits versions and delete markers; read the raw
				// document to check that they are interleaved in order.
				resp := e.signed(http.MethodGet, "/ver-bucket?"+strings.ReplaceAll(q.Encode(), "versions=", "versions"), nil, nil)
				bktExpect(t, resp, 200, "")
				page := bktParseVersions(t, resp.Data)
				out, err := e.client.ListObjectVersions(ctx, in)
				if err != nil {
					t.Fatalf("ListObjectVersions: %v", err)
				}
				if len(out.Versions)+len(out.DeleteMarkers) != len(page.entries) {
					t.Fatalf("SDK saw %d+%d entries, raw %d", len(out.Versions), len(out.DeleteMarkers), len(page.entries))
				}
				if n := len(page.entries) + len(out.CommonPrefixes); n > int(maxKeys) {
					t.Fatalf("%d entries > max %d", n, maxKeys)
				}
				if aws.ToString(out.KeyMarker) != km || aws.ToString(out.VersionIdMarker) != vm {
					t.Errorf("marker echo %q %q", aws.ToString(out.KeyMarker), aws.ToString(out.VersionIdMarker))
				}
				for _, v := range out.Versions {
					if v.Owner == nil || v.StorageClass != types.ObjectVersionStorageClassStandard || v.ETag == nil || v.Size == nil {
						t.Errorf("version %+v incomplete", v)
					}
				}
				for _, dm := range out.DeleteMarkers {
					if dm.Owner == nil || dm.LastModified == nil {
						t.Errorf("delete marker %+v incomplete", dm)
					}
				}
				got = append(got, page.entries...)
				for _, cp := range out.CommonPrefixes {
					gotCPs = append(gotCPs, aws.ToString(cp.Prefix))
				}
				if !aws.ToBool(out.IsTruncated) {
					if out.NextKeyMarker != nil || out.NextVersionIdMarker != nil {
						t.Errorf("next markers on last page")
					}
					break
				}
				km, vm = aws.ToString(out.NextKeyMarker), aws.ToString(out.NextVersionIdMarker)
				if km == "" {
					t.Fatal("truncated without NextKeyMarker")
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) || strings.Join(gotCPs, ",") != strings.Join(wantCPs, ",") {
				t.Errorf("delim=%q max=%d:\n got %v %v\nwant %v %v", delim, maxKeys, got, gotCPs, want, wantCPs)
			}
		}
	}
}

// bktVersionsPage is a parsed raw ListVersionsResult.
type bktVersionsPage struct {
	entries []bktVersionRec
}

// bktParseVersions parses <Version>/<DeleteMarker> entries in document
// order and checks their element order.
func bktParseVersions(t *testing.T, data []byte) bktVersionsPage {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(string(data)))
	var page bktVersionsPage
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || (se.Name.Local != "Version" && se.Name.Local != "DeleteMarker") {
			continue
		}
		var raw struct {
			Inner []byte `xml:",innerxml"`
		}
		if err := d.DecodeElement(&raw, &se); err != nil {
			t.Fatal(err)
		}
		var v struct {
			Key, VersionId string
			IsLatest       bool
		}
		if err := xml.Unmarshal([]byte("<x>"+string(raw.Inner)+"</x>"), &v); err != nil {
			t.Fatal(err)
		}
		inner := string(raw.Inner)
		if se.Name.Local == "Version" {
			if !bktInOrder(inner, "<Key>", "<VersionId>", "<IsLatest>", "<LastModified>", "<ETag>", "<Size>", "<StorageClass>", "<Owner>") {
				t.Errorf("Version element order: %s", inner)
			}
		} else {
			if !bktInOrder(inner, "<Key>", "<VersionId>", "<IsLatest>", "<LastModified>", "<Owner>") || strings.Contains(inner, "<ETag>") || strings.Contains(inner, "<Size>") {
				t.Errorf("DeleteMarker elements: %s", inner)
			}
		}
		page.entries = append(page.entries, bktVersionRec{v.Key, v.VersionId, se.Name.Local == "DeleteMarker", v.IsLatest})
	}
	return page
}

func TestListObjectVersionsRaw(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.createBucket("verraw")
	e.putObject("verraw", "k", []byte("1"), storage.ObjectMeta{})
	resp := e.signed(http.MethodGet, "/verraw?versions", nil, nil)
	bktExpect(t, resp, 200, "")
	order := bktCompact(bktElementOrder(t, resp.Data))
	if want := "Name,Prefix,KeyMarker,VersionIdMarker,MaxKeys,IsTruncated,Version"; strings.Join(order, ",") != want {
		t.Errorf("elements = %v; %s", order, resp.Data)
	}
	if !strings.Contains(string(resp.Data), "<VersionId>null</VersionId><IsLatest>true</IsLatest>") {
		t.Errorf("null version: %s", resp.Data)
	}
	var root struct{ XMLName xml.Name }
	if err := xml.Unmarshal(resp.Data, &root); err != nil || root.XMLName.Space != s3Namespace || root.XMLName.Local != "ListVersionsResult" {
		t.Errorf("root = %v %v", root.XMLName, err)
	}

	bktVersioned(t, e, "verraw")
	v2 := e.putObject("verraw", "k", []byte("2"), storage.ObjectMeta{})
	resp = e.signed(http.MethodGet, "/verraw?versions&max-keys=1", nil, nil)
	order = bktCompact(bktElementOrder(t, resp.Data))
	if want := "Name,Prefix,KeyMarker,VersionIdMarker,NextKeyMarker,NextVersionIdMarker,MaxKeys,IsTruncated,Version"; strings.Join(order, ",") != want {
		t.Errorf("elements = %v", order)
	}
	if s := string(resp.Data); !strings.Contains(s, "<NextKeyMarker>k</NextKeyMarker><NextVersionIdMarker>"+v2.VersionID+"</NextVersionIdMarker>") {
		t.Errorf("next markers: %s", s)
	}
	resp = e.signed(http.MethodGet, "/verraw?versions&key-marker=k&version-id-marker="+url.QueryEscape(v2.VersionID), nil, nil)
	if s := string(resp.Data); !strings.Contains(s, "<VersionId>null</VersionId><IsLatest>false</IsLatest>") || strings.Contains(s, v2.VersionID+"</VersionId>") {
		t.Errorf("after marker: %s", s)
	}

	// version-id-marker requires key-marker.
	resp = e.signed(http.MethodGet, "/verraw?versions&version-id-marker=abc", nil, nil)
	bktExpect(t, resp, 400, "InvalidArgument")
	if !strings.Contains(string(resp.Data), "<ArgumentName>version-id-marker</ArgumentName>") {
		t.Errorf("argument name: %s", resp.Data)
	}
	_, err := e.client.ListObjectVersions(ctx, &awss3.ListObjectVersionsInput{Bucket: aws.String("verraw"), VersionIdMarker: aws.String("abc")})
	if bktAPICode(err) != "InvalidArgument" {
		t.Errorf("SDK: %v", err)
	}
	for _, q := range []string{"max-keys=-5", "encoding-type=gzip"} {
		bktExpect(t, e.signed(http.MethodGet, "/verraw?versions&"+q, nil, nil), 400, "InvalidArgument")
	}
	resp = e.signed(http.MethodGet, "/verraw?versions&max-keys=0", nil, nil)
	if s := string(resp.Data); strings.Contains(s, "<Version>") || !strings.Contains(s, "<IsTruncated>false</IsTruncated>") {
		t.Errorf("max-keys=0: %s", s)
	}
	bktExpect(t, e.signed(http.MethodGet, "/vermissing?versions", nil, nil), 404, "NoSuchBucket")

	// Common prefix as the last entry: NextKeyMarker is the prefix and no
	// NextVersionIdMarker is sent.
	e.putObject("verraw", "a/1", []byte("x"), storage.ObjectMeta{})
	resp = e.signed(http.MethodGet, "/verraw?versions&delimiter=/&max-keys=1", nil, nil)
	if s := string(resp.Data); !strings.Contains(s, "<NextKeyMarker>a/</NextKeyMarker>") || strings.Contains(s, "NextVersionIdMarker") ||
		!strings.Contains(s, "<CommonPrefixes><Prefix>a/</Prefix></CommonPrefixes>") {
		t.Errorf("common prefix page: %s", s)
	}
	resp = e.signed(http.MethodGet, "/verraw?versions&delimiter=/&key-marker=a/", nil, nil)
	if s := string(resp.Data); strings.Contains(s, "<Prefix>a/</Prefix></CommonPrefixes>") || !strings.Contains(s, "<Key>k</Key>") {
		t.Errorf("resume after common prefix: %s", s)
	}
}

func TestListObjectsAuthz(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	bktPopulate(t, e, "authz-list", []string{"pub/a", "priv/b"})
	e.putPolicy("list-pub", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::authz-list"],"Condition":{"StringLike":{"s3:prefix":["pub/*"]}}}]}`)
	u := e.createUser("lister", "list-pub")

	bktExpect(t, e.signedAs(u, http.MethodGet, "/authz-list?prefix=pub/", nil, nil), 200, "")
	bktExpect(t, e.signedAs(u, http.MethodGet, "/authz-list?list-type=2&prefix=pub/x", nil, nil), 200, "")
	bktExpect(t, e.signedAs(u, http.MethodGet, "/authz-list?prefix=priv/", nil, nil), 403, "AccessDenied")
	bktExpect(t, e.signedAs(u, http.MethodGet, "/authz-list?versions&prefix=pub/", nil, nil), 403, "AccessDenied")
	bktExpect(t, e.anon(http.MethodGet, "/authz-list", nil, nil), 403, "AccessDenied")
	bktExpect(t, e.anon(http.MethodGet, "/authz-missing", nil, nil), 404, "NoSuchBucket")

	e.putBucketConfig("authz-list", storage.ConfigPolicy, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::authz-list"]}]}`)
	resp := e.anon(http.MethodGet, "/authz-list?list-type=2", nil, nil)
	bktExpect(t, resp, 200, "")
	if !strings.Contains(string(resp.Data), "<Key>priv/b</Key>") {
		t.Errorf("public listing: %s", resp.Data)
	}
}
