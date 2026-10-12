package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPayloadModeOnlyFromSignedData: Result.PayloadMode (the
// s3:x-amz-content-sha256 condition key) is never taken from an unsigned
// X-Amz-Content-Sha256 header. Anyone holding a presigned URL, and any
// anonymous client, can add that header, so it must not make an unsigned
// body look signed to a "require a signed payload" policy. The header
// still selects how the body is verified.
func TestPayloadModeOnlyFromSignedData(t *testing.T) {
	now := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(now)
	body := "attacker chosen body"
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	creds := Credentials{AccessKey: testAK, SecretKey: testSK}

	u, err := PresignURL("PUT", "http://example.com/bucket/key", creds, "us-east-1", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	authenticate := func(target, header, payload string) (*Result, error) {
		r := httptest.NewRequest("PUT", target, strings.NewReader(payload))
		r.Host = "example.com"
		if header != "" {
			r.Header.Set("X-Amz-Content-Sha256", header)
		}
		res, err := v.Authenticate(r)
		if err != nil {
			return nil, err
		}
		_, err = io.ReadAll(res.Body)
		return res, err
	}

	// Presigned URL with an unsigned payload, plus an unsigned hash header.
	res, err := authenticate(u, digest, body)
	if err != nil {
		t.Fatalf("presigned with hash header: %v", err)
	}
	if res.Method != MethodPresignedV4 || res.PayloadMode != "" {
		t.Errorf("presigned with an unsigned hash header: method %s, PayloadMode %q, want \"\"", res.Method, res.PayloadMode)
	}
	// The header still decides how the body is verified.
	if _, err := authenticate(u, digest, "another body"); errCode(err) != "XAmzContentSHA256Mismatch" {
		t.Errorf("presigned body not matching the declared hash: %v, want XAmzContentSHA256Mismatch", err)
	}

	// Anonymous request with a hash header.
	res, err = authenticate("http://example.com/bucket/key", digest, body)
	if err != nil {
		t.Fatalf("anonymous with hash header: %v", err)
	}
	if !res.Identity.IsAnonymous || res.PayloadMode != "" {
		t.Errorf("anonymous: PayloadMode %q, want \"\"", res.PayloadMode)
	}

	// A header-signed request's hash is signed.
	r := httptest.NewRequest("PUT", "http://example.com/bucket/key", strings.NewReader(body))
	if err := SignRequest(r, creds, "us-east-1", "s3", digest, now); err != nil {
		t.Fatal(err)
	}
	if res, err = v.Authenticate(r); err != nil {
		t.Fatalf("header-signed request: %v", err)
	}
	if res.PayloadMode != digest {
		t.Errorf("header-signed request: PayloadMode %q, want %q", res.PayloadMode, digest)
	}
}
