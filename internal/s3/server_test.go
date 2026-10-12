package s3

import (
	"context"
	"encoding/xml"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestHarnessSDKClient checks the harness wiring with the real SDK client:
// a ListBuckets override rendering real S3 XML is parsed by the SDK.
func TestHarnessSDKClient(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("alpha")
	e.override("ListBuckets", func(s *Server, w http.ResponseWriter, r *http.Request, rc *reqContext) error {
		bs, err := s.backend.ListBuckets(r.Context())
		if err != nil {
			return err
		}
		type bucket struct {
			Name         string `xml:"Name"`
			CreationDate string `xml:"CreationDate"`
		}
		res := struct {
			XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
			Owner   Owner    `xml:"Owner"`
			Buckets []bucket `xml:"Buckets>Bucket"`
		}{Owner: ownerFor(rc.userName())}
		for _, b := range bs {
			res.Buckets = append(res.Buckets, bucket{b.Name, formatXMLTime(b.CreatedAt)})
		}
		return writeXML(w, http.StatusOK, res)
	})
	out, err := e.client.ListBuckets(context.Background(), &awss3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Buckets) != 1 || aws.ToString(out.Buckets[0].Name) != "alpha" || aws.ToString(out.Owner.DisplayName) != "root" {
		t.Fatalf("%+v", out)
	}
	// Unimplemented stubs surface as API errors with the S3 code.
	if _, err := e.client.GetBucketTagging(context.Background(), &awss3.GetBucketTaggingInput{Bucket: aws.String("alpha")}); err == nil {
		t.Fatal("stub returned success")
	}
}

// TestHarnessPresigned checks presigned URLs and IAM user clients.
func TestHarnessPresigned(t *testing.T) {
	e := newTestEnv(t)
	requireAuthz(t, e)
	e.createBucket("bkt")
	e.putObject("bkt", "k", []byte("data"), storage.ObjectMeta{})
	e.override("GetObject", okHandler)
	u, err := auth.PresignURL("GET", e.url("/bkt/k"), testRoot, "us-east-1", time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", u, nil)
	if resp := e.do(req); resp.StatusCode != 200 || string(resp.Data) != "GetObject bkt k" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Data)
	}
	alice := e.createUser("alice", "readonly")
	if resp := e.signedAs(alice, "GET", "/bkt/k", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("readonly user: %d %s", resp.StatusCode, resp.Data)
	}
}

// TestConcurrentRequests exercises the whole pipeline in parallel (-race).
func TestConcurrentRequests(t *testing.T) {
	e := newTestEnv(t)
	e.createBucket("bkt")
	e.putBucketConfig("bkt", storage.ConfigPolicy, `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`)
	e.override("GetObject", okHandler)
	e.override("PutObject", (&uploadRecorder{}).handler)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if r := e.anon("GET", "/bkt/k", nil, nil); r.StatusCode != 200 {
					errs <- "anon get " + r.Status
				}
				if r := e.signed("PUT", "/bkt/k", []byte("payload"), nil); r.StatusCode != 200 {
					errs <- "put " + r.Status + " " + string(r.Data)
				}
				h := http.Header{"Origin": {"http://x"}, "Access-Control-Request-Method": {"GET"}}
				_ = e.anon("OPTIONS", "/bkt/k", nil, h)
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if s := e.metrics.Snapshot(); s.Requests != 8*10*3 || s.InFlight != 0 {
		t.Fatalf("metrics %d in-flight %d", s.Requests, s.InFlight)
	}
}
