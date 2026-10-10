package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/auth"
)

func TestBucketCRUD(t *testing.T) {
	for _, e := range []*env{fsEnv, memEnv} {
		t.Run(e.name, func(t *testing.T) {
			c := e.root()
			name := bucketName(t)
			_, err := c.HeadBucket(ctx(t), &s3.HeadBucketInput{Bucket: aws.String(name)})
			if httpStatus(err) != http.StatusNotFound {
				t.Fatalf("HeadBucket missing: want 404, got %v", err)
			}
			out, err := c.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(name)})
			must(t, err, "CreateBucket")
			if got := aws.ToString(out.Location); got != "/"+name {
				t.Errorf("Location = %q, want /%s", got, name)
			}
			_, err = c.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(name)})
			wantErr(t, err, "BucketAlreadyOwnedByYou", http.StatusConflict)

			hb, err := c.HeadBucket(ctx(t), &s3.HeadBucketInput{Bucket: aws.String(name)})
			must(t, err, "HeadBucket")
			if aws.ToString(hb.BucketRegion) != region {
				t.Errorf("HeadBucket region = %q", aws.ToString(hb.BucketRegion))
			}
			loc, err := c.GetBucketLocation(ctx(t), &s3.GetBucketLocationInput{Bucket: aws.String(name)})
			must(t, err, "GetBucketLocation")
			if loc.LocationConstraint != "" {
				t.Errorf("us-east-1 LocationConstraint = %q, want empty", loc.LocationConstraint)
			}

			lb, err := c.ListBuckets(ctx(t), &s3.ListBucketsInput{})
			must(t, err, "ListBuckets")
			found := false
			for _, b := range lb.Buckets {
				if aws.ToString(b.Name) == name {
					found = true
					if b.CreationDate == nil || b.CreationDate.IsZero() {
						t.Errorf("bucket without CreationDate")
					}
				}
			}
			if !found {
				t.Fatalf("bucket %s not listed", name)
			}

			// Non-empty bucket cannot be deleted.
			e.put(t, name, "k", []byte("x"))
			_, err = c.DeleteBucket(ctx(t), &s3.DeleteBucketInput{Bucket: aws.String(name)})
			wantErr(t, err, "BucketNotEmpty", http.StatusConflict)
			_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(name), Key: aws.String("k")})
			must(t, err, "DeleteObject")
			_, err = c.DeleteBucket(ctx(t), &s3.DeleteBucketInput{Bucket: aws.String(name)})
			must(t, err, "DeleteBucket")
			_, err = c.DeleteBucket(ctx(t), &s3.DeleteBucketInput{Bucket: aws.String(name)})
			wantErr(t, err, "NoSuchBucket", http.StatusNotFound)
			_, err = c.GetBucketLocation(ctx(t), &s3.GetBucketLocationInput{Bucket: aws.String(name)})
			wantErr(t, err, "NoSuchBucket", http.StatusNotFound)
		})
	}
}

func TestBucketLocationConstraint(t *testing.T) {
	c := fsEnv.root()
	name := bucketName(t)
	_, err := c.CreateBucket(ctx(t), &s3.CreateBucketInput{
		Bucket:                    aws.String(name),
		CreateBucketConfiguration: &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraintEuWest1},
	})
	must(t, err, "CreateBucket eu-west-1")
	loc, err := c.GetBucketLocation(ctx(t), &s3.GetBucketLocationInput{Bucket: aws.String(name)})
	must(t, err, "GetBucketLocation")
	if loc.LocationConstraint != s3types.BucketLocationConstraintEuWest1 {
		t.Errorf("LocationConstraint = %q", loc.LocationConstraint)
	}
	hb, err := c.HeadBucket(ctx(t), &s3.HeadBucketInput{Bucket: aws.String(name)})
	must(t, err, "HeadBucket")
	if aws.ToString(hb.BucketRegion) != "eu-west-1" {
		t.Errorf("HeadBucket region = %q", aws.ToString(hb.BucketRegion))
	}
}

func TestBucketInvalidNames(t *testing.T) {
	c := fsEnv.root()
	for _, name := range []string{"ab", "UPPER-case", "has_underscore", "-start", "end-", "a..b", "192.168.1.1", "xn--punycode", "bucket-s3alias"} {
		_, err := c.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(name)})
		if err == nil {
			t.Errorf("CreateBucket(%q) succeeded", name)
			continue
		}
		if code := apiCode(err); code != "InvalidBucketName" {
			t.Errorf("CreateBucket(%q): code %q (%v)", name, code, err)
		}
	}
}

func TestVirtualHostAddressing(t *testing.T) {
	e := fsEnv
	vh := e.vhost(rootCreds)
	name := bucketName(t)
	_, err := vh.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(name)})
	must(t, err, "vhost CreateBucket")
	_, err = vh.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(name), Key: aws.String("_gos3/info"), Body: stringsReader("vhost body")})
	must(t, err, "vhost PutObject")
	// The object is visible path-style, and a virtual-host request for a
	// key starting with _gos3/ is an object request, not a system route.
	if got := string(e.get(t, name, "_gos3/info")); got != "vhost body" {
		t.Fatalf("path-style read = %q", got)
	}
	out, err := vh.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String(name), Key: aws.String("_gos3/info")})
	must(t, err, "vhost GetObject")
	if got := string(readAll(t, out.Body)); got != "vhost body" {
		t.Fatalf("vhost read = %q", got)
	}
	l, err := vh.ListObjectsV2(ctx(t), &s3.ListObjectsV2Input{Bucket: aws.String(name)})
	must(t, err, "vhost ListObjectsV2")
	if len(l.Contents) != 1 || aws.ToString(l.Contents[0].Key) != "_gos3/info" {
		t.Fatalf("vhost listing = %+v", l.Contents)
	}
	// Bucket in the Host, key "/" path: raw signed request.
	resp := e.signed(t, rootCreds, http.MethodGet, "http://"+name+"."+vhostZone+"/?list-type=2", nil, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("raw vhost list: %d %s", resp.status, resp.body)
	}
	// The bare domain is path-style (ListBuckets).
	lb, err := vh.ListBuckets(ctx(t), &s3.ListBucketsInput{})
	must(t, err, "vhost ListBuckets")
	if len(lb.Buckets) == 0 {
		t.Error("vhost ListBuckets returned nothing")
	}
	// Dotted bucket names.
	dotted := "dotted." + bucketName(t)
	_, err = vh.CreateBucket(ctx(t), &s3.CreateBucketInput{Bucket: aws.String(dotted)})
	must(t, err, "vhost CreateBucket dotted")
	_, err = vh.PutObject(ctx(t), &s3.PutObjectInput{Bucket: aws.String(dotted), Key: aws.String("k"), Body: stringsReader("dot")})
	must(t, err, "vhost PutObject dotted")
	if got := string(e.get(t, dotted, "k")); got != "dot" {
		t.Errorf("dotted bucket object %q", got)
	}
	// Conditional delete (If-Match) through the virtual host.
	_, err = vh.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(dotted), Key: aws.String("k"), IfMatch: aws.String(`"0000"`)})
	wantErr(t, err, "PreconditionFailed", http.StatusPreconditionFailed)
	_, err = vh.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String(dotted), Key: aws.String("k"), IfMatch: aws.String(quoted(md5Hex([]byte("dot"))))})
	must(t, err, "conditional delete")
	// Browser POST upload to the bucket host.
	now := time.Now().UTC()
	pf, err := auth.SignPostPolicy(rootCreds, region, now, now.Add(10*time.Minute), []any{map[string]string{"bucket": name}, []any{"starts-with", "$key", ""}})
	must(t, err, "SignPostPolicy")
	var fields [][2]string
	for k, v := range pf {
		fields = append(fields, [2]string{k, v})
	}
	body, ct := postForm(t, append(fields, [2]string{"key", "posted"}), "f.txt", []byte("posted via vhost"))
	resp = e.post(t, "http://"+name+"."+vhostZone+"/", body, ct)
	if resp.status != http.StatusNoContent || resp.header.Get("Location") != "http://"+name+"."+vhostZone+"/posted" {
		t.Errorf("vhost POST: %d %q %s", resp.status, resp.header.Get("Location"), resp.body)
	}
	if got := string(e.get(t, name, "posted")); got != "posted via vhost" {
		t.Errorf("posted object %q", got)
	}
}
