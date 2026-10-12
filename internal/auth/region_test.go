package auth

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestSetRegionStrict: with StrictRegion the region can be changed at
// runtime (settings update) and is enforced immediately (regression: the
// startup region was used until restart).
func TestSetRegionStrict(t *testing.T) {
	v := liveVerifier(func(o *Options) { o.StrictRegion = true })
	signed := func(region string) outcome {
		r := httptest.NewRequest("GET", "http://example.com/bucket", nil)
		if err := SignRequest(r, Credentials{AccessKey: testAK, SecretKey: testSK}, region, "s3", "", time.Now()); err != nil {
			t.Fatal(err)
		}
		return runAuth(v, r)
	}
	if o := signed("us-east-1"); o.Code != "" {
		t.Fatalf("startup region rejected: %+v", o)
	}
	if o := signed("eu-west-1"); o.Code != "AuthorizationHeaderMalformed" {
		t.Fatalf("other region: %+v", o)
	}
	v.SetRegion("eu-west-1")
	if o := signed("eu-west-1"); o.Code != "" {
		t.Fatalf("new region rejected: %+v", o)
	}
	if o := signed("us-east-1"); o.Code != "AuthorizationHeaderMalformed" || o.Message == "" {
		t.Fatalf("old region accepted: %+v", o)
	}

	// Concurrent updates and checks are race-free.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); v.SetRegion("eu-west-1") }()
		go func() { defer wg.Done(); signed("eu-west-1") }()
	}
	wg.Wait()

	// Without StrictRegion any region is accepted whatever is set.
	lax := liveVerifier()
	lax.SetRegion("ap-south-1")
	r := httptest.NewRequest("GET", "http://example.com/bucket", nil)
	if err := SignRequest(r, Credentials{AccessKey: testAK, SecretKey: testSK}, "us-west-2", "s3", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if o := runAuth(lax, r); o.Code != "" {
		t.Fatalf("lax verifier: %+v", o)
	}
}
