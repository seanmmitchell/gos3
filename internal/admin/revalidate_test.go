package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
)

// slowRequest sends a request signed with creds whose body runs hook
// before it is first read, i.e. after the endpoint authenticated and
// authorized the request, like a body that is still arriving.
func (e *testEnv) slowRequest(creds auth.Credentials, method, path string, body any, hook func()) response {
	e.t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest(method, Prefix+path, &hookBody{hook: hook, r: strings.NewReader(string(data))})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", strconv.Itoa(len(data)))
	req.ContentLength = int64(len(data))
	sum := sha256.Sum256(data)
	if err := auth.SignRequest(req, creds, "us-east-1", "s3", hex.EncodeToString(sum[:]), time.Now()); err != nil {
		e.t.Fatalf("SignRequest: %v", err)
	}
	rec := httptest.NewRecorder()
	e.api.ServeHTTP(rec, req)
	return response{status: rec.Code, header: rec.Header(), body: rec.Body.Bytes()}
}

// TestRevalidateAfterBody checks that an admin request authorized before
// its body arrived does not act once the caller's access was revoked
// while the body was being read: a deleted or deactivated key, a disabled
// user, or (for requests in flight longer than reauthorizeAfter) a policy
// that no longer allows the route's action.
func TestRevalidateAfterBody(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addUser("team-a")
	newBoss := func(name string) auth.Credentials {
		return e.addUser(name, iam.PolicyAdmin)
	}
	keyCount := func() int {
		u, err := e.iam.GetUser(ctx, "team-a")
		if err != nil {
			t.Fatal(err)
		}
		return len(u.Keys)
	}

	for _, tc := range []struct {
		name   string
		revoke func(name string, c auth.Credentials)
	}{
		{"key deleted", func(_ string, c auth.Credentials) {
			if err := e.iam.DeleteAccessKey(ctx, c.AccessKey); err != nil {
				t.Fatal(err)
			}
		}},
		{"key deactivated", func(_ string, c auth.Credentials) {
			if err := e.iam.UpdateAccessKey(ctx, c.AccessKey, iam.KeyInactive); err != nil {
				t.Fatal(err)
			}
		}},
		{"user disabled", func(name string, _ auth.Credentials) {
			off := true
			if _, err := e.iam.UpdateUser(ctx, name, iam.UserUpdate{Disabled: &off}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "boss-" + strings.ReplaceAll(tc.name, " ", "-")
			c := newBoss(name)
			before := keyCount()
			r := e.slowRequest(c, "POST", "/users/team-a/keys", map[string]any{"description": "late"}, func() { tc.revoke(name, c) })
			msg := r.expectError(t, http.StatusForbidden, codeAccessDenied)
			if !strings.Contains(msg, "revoked") {
				t.Errorf("message = %q", msg)
			}
			if n := keyCount(); n != before {
				t.Fatalf("key created anyway: %d keys, had %d", n, before)
			}
		})
	}

	t.Run("policy detached after reauthorizeAfter", func(t *testing.T) {
		c := newBoss("boss-detached")
		r := e.slowRequest(c, "PUT", "/policies/late", map[string]any{
			"document": map[string]any{"Statement": []map[string]any{{"Effect": "Allow", "Action": "s3:*", "Resource": "*"}}},
		}, func() {
			empty := []string{}
			if _, err := e.iam.UpdateUser(ctx, "boss-detached", iam.UserUpdate{Policies: &empty}); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(reauthorizeAfter + time.Second)
		})
		r.expectError(t, http.StatusForbidden, codeAccessDenied)
		if _, err := e.iam.GetPolicy(ctx, "late"); !errors.Is(err, iam.ErrPolicyNotFound) {
			t.Fatalf("policy stored anyway: %v", err)
		}
	})
	t.Run("policy detached on a short request", func(t *testing.T) {
		// Within reauthorizeAfter the decision taken before the body is
		// kept (only the credentials are re-checked).
		c := newBoss("boss-quick")
		r := e.slowRequest(c, "PUT", "/policies/quick", map[string]any{
			"document": map[string]any{"Statement": []map[string]any{{"Effect": "Allow", "Action": "s3:*", "Resource": "*"}}},
		}, func() {
			empty := []string{}
			if _, err := e.iam.UpdateUser(ctx, "boss-quick", iam.UserUpdate{Policies: &empty}); err != nil {
				t.Fatal(err)
			}
		})
		r.expectStatus(t, http.StatusOK)
	})
	t.Run("slow request still allowed", func(t *testing.T) {
		c := newBoss("boss-slow")
		r := e.slowRequest(c, "POST", "/users/team-a/keys", map[string]any{"description": "slow"}, func() {
			e.clock.Advance(time.Minute)
		})
		r.expectStatus(t, http.StatusCreated)
	})
	t.Run("time conditions as of the request start", func(t *testing.T) {
		// A grant that expires while the body arrives still applies: the
		// request is judged as it was made. A fresh env keeps the API's
		// clock in step with the real clock IAM evaluates with.
		e := newEnv(t)
		e.addUser("team-a")
		until := time.Now().Add(1500 * time.Millisecond).UTC().Format(time.RFC3339) // >= 0.5 s ahead
		e.putPolicy("until", map[string]any{
			"Effect": "Allow", "Action": "admin:CreateAccessKey", "Resource": "arn:gos3:admin:::user/team-*",
			"Condition": map[string]any{"DateLessThan": map[string]any{"aws:CurrentTime": until}},
		})
		c := e.addUser("timed", "until")
		r := e.slowRequest(c, "POST", "/users/team-a/keys", map[string]any{"description": "timed"}, func() {
			e.clock.Advance(reauthorizeAfter + time.Second)
			time.Sleep(1600 * time.Millisecond) // the real clock IAM uses passes "until"
		})
		r.expectStatus(t, http.StatusCreated)
	})
}
