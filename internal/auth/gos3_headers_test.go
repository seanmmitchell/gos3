package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestGos3HeadersMustBeSigned(t *testing.T) {
	srv := newAuthServer(t, liveVerifier())
	for _, name := range []string{"X-Gos3-Backing", "X-Gos3-Encryption", "X-Gos3-Compression", "x-gos3-future-option"} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest("PUT", srv.URL+"/bucket", nil)
			u, headers := sdkPresign(t, req, time.Hour, time.Now())
			presigned, _ := http.NewRequest("PUT", u, nil)
			presigned.Header = headers.Clone()
			presigned.Header.Set(name, "injected")
			if got := send(t, srv.Client(), presigned); got.Code != "AccessDenied" {
				t.Fatalf("unsigned custom header accepted: %+v", got)
			}
			signed, _ := http.NewRequest("PUT", srv.URL+"/bucket", nil)
			signed.Header.Set(name, "intended")
			if err := SignRequest(signed, Credentials{AccessKey: testAK, SecretKey: testSK}, "us-east-1", "s3", "", time.Now()); err != nil {
				t.Fatal(err)
			}
			if got := send(t, srv.Client(), signed); got.Code != "" {
				t.Fatalf("signed custom header refused: %+v", got)
			}
			signed.Header.Set(name, "changed")
			if got := send(t, srv.Client(), signed); got.Code != "SignatureDoesNotMatch" {
				t.Fatalf("modified custom header accepted: %+v", got)
			}
		})
	}
	if err := checkUnsignedAmzHeaders(http.Header{"X-Gos3-Backing": {"safe"}}, map[string]bool{"x-gos3-backing": true}); err != nil {
		t.Fatalf("signed custom header rejected: %v", err)
	}
}
