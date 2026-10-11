package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
)

// signedBodyMessage is the message of the 400 refusing an unsigned body.
const signedBodyMessage = "admin API requests with a body must sign the payload"

// signRaw builds a request for the admin API signed by root with an
// explicit x-amz-content-sha256 value.
func (e *testEnv) signRaw(t *testing.T, method, path, body, payloadHash string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+Prefix+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", payloadHash, time.Now()); err != nil {
		t.Fatal(err)
	}
	return req
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestUnsignedBodyReplayRefused is the auth-1 proof of concept: root signs
// PATCH /users/bob with UNSIGNED-PAYLOAD; an observer resends the captured
// headers with a different body. The signature never covered the body, so
// before the fix the replay escalated bob to admin.
func TestUnsignedBodyReplayRefused(t *testing.T) {
	e := newEnv(t)
	e.addUser("bob")

	benign := e.signRaw(t, "PATCH", "/users/bob", `{"description":"benign"}`, auth.PayloadUnsigned)
	msg := e.send(benign).expectError(t, http.StatusBadRequest, codeInvalidRequest)
	if !strings.Contains(msg, signedBodyMessage) || !strings.Contains(msg, "X-Amz-Content-Sha256") {
		t.Errorf("message %q", msg)
	}

	// Replay: same signed headers, attacker body.
	evil, err := http.NewRequest("PATCH", benign.URL.String(), strings.NewReader(`{"policies":["admin"]}`))
	if err != nil {
		t.Fatal(err)
	}
	evil.Header = benign.Header.Clone()
	e.send(evil).expectError(t, http.StatusBadRequest, codeInvalidRequest)

	u, err := e.iam.GetUser(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Policies) != 0 || u.Description != "" {
		t.Fatalf("bob changed by unsigned requests: %+v", u)
	}
}

// TestSignedBodyRequired covers every payload mode on body-carrying admin
// requests.
func TestSignedBodyRequired(t *testing.T) {
	e := newEnv(t)
	userBody := func(name string) string { return `{"name":"` + name + `","policies":[]}` }

	t.Run("signed body accepted", func(t *testing.T) {
		b := userBody("signed")
		e.send(e.signRaw(t, "POST", "/users", b, sha256Hex(b))).expectStatus(t, http.StatusCreated)
	})
	t.Run("tampered signed body refused", func(t *testing.T) {
		b := userBody("original")
		req := e.signRaw(t, "POST", "/users", b, sha256Hex(b))
		tampered := userBody("tampered")
		evil, _ := http.NewRequest("POST", req.URL.String(), strings.NewReader(tampered))
		evil.Header = req.Header.Clone()
		msg := e.send(evil).expectError(t, http.StatusBadRequest, codeInvalidRequest)
		if !strings.Contains(msg, "XAmzContentSHA256Mismatch") {
			t.Errorf("message %q", msg)
		}
		// Swapping the hash header too breaks the signature.
		evil, _ = http.NewRequest("POST", req.URL.String(), strings.NewReader(tampered))
		evil.Header = req.Header.Clone()
		evil.Header.Set("X-Amz-Content-Sha256", sha256Hex(tampered))
		e.send(evil).expectError(t, http.StatusUnauthorized, codeUnauthenticated)
		for _, n := range []string{"original", "tampered"} {
			if _, err := e.iam.GetUser(context.Background(), n); err == nil {
				t.Errorf("user %s created by a tampered request", n)
			}
		}
	})
	t.Run("unsigned payload refused", func(t *testing.T) {
		for _, m := range []string{"POST", "PUT", "PATCH"} {
			path := map[string]string{"POST": "/users", "PUT": "/policies/p", "PATCH": "/users/x"}[m]
			e.send(e.signRaw(t, m, path, `{}`, auth.PayloadUnsigned)).expectError(t, http.StatusBadRequest, codeInvalidRequest)
		}
		if _, err := e.iam.GetUser(context.Background(), "unsigned"); err == nil {
			t.Error("user created")
		}
	})
	t.Run("POST without body still needs a signed payload", func(t *testing.T) {
		// POST /whoami/keys takes an optional body; the empty-body hash is a
		// signed payload, UNSIGNED-PAYLOAD is not.
		e.send(e.signRaw(t, "POST", "/users/x/keys", "", auth.PayloadUnsigned)).expectError(t, http.StatusBadRequest, codeInvalidRequest)
	})
	t.Run("DELETE with a body", func(t *testing.T) {
		req := e.signRaw(t, "DELETE", "/users/nobody-here", `{"x":1}`, auth.PayloadUnsigned)
		e.send(req).expectError(t, http.StatusBadRequest, codeInvalidRequest)
	})
	t.Run("unsigned streaming refused", func(t *testing.T) {
		b := userBody("streamed")
		chunked := fmt.Sprintf("%x\r\n%s\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n", len(b), b)
		req, _ := http.NewRequest("POST", e.srv.URL+Prefix+"/users", strings.NewReader(chunked))
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(b)))
		req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", auth.PayloadStreamingUnsignedTrailer, time.Now()); err != nil {
			t.Fatal(err)
		}
		e.send(req).expectError(t, http.StatusBadRequest, codeInvalidRequest)
		if _, err := e.iam.GetUser(context.Background(), "streamed"); err == nil {
			t.Error("user created")
		}
	})
	t.Run("presigned POST refused", func(t *testing.T) {
		u, err := auth.PresignURL("POST", e.srv.URL+Prefix+"/users", rootCreds, "us-east-1", time.Minute, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		b := userBody("presigned")
		req, _ := http.NewRequest("POST", u, strings.NewReader(b))
		// Even a (not signed) payload hash header does not help.
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex(b))
		e.send(req).expectError(t, http.StatusBadRequest, codeInvalidRequest)
		if _, err := e.iam.GetUser(context.Background(), "presigned"); err == nil {
			t.Error("user created by a presigned POST")
		}
	})
	t.Run("bodiless GET and DELETE with UNSIGNED-PAYLOAD", func(t *testing.T) {
		e.send(e.signRaw(t, "GET", "/users", "", auth.PayloadUnsigned)).expectStatus(t, http.StatusOK)
		if _, err := e.iam.CreateUser(context.Background(), iam.UserSpec{Name: "gone", Policies: []string{}}); err != nil {
			t.Fatal(err)
		}
		e.send(e.signRaw(t, "DELETE", "/users/gone", "", auth.PayloadUnsigned)).expectStatus(t, http.StatusNoContent)
		users, _ := e.iam.ListUsers(context.Background())
		if slices.ContainsFunc(users, func(u iam.User) bool { return u.Name == "gone" }) {
			t.Error("user not deleted")
		}
	})
}
