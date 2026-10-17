package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
)

// TestChosenAccessKeyID checks admin-1 / iam-1: a delegated administrator
// allowed admin:CreateAccessKey only on user/team-* could mint a key whose
// ID is another user's name ("alice"), "root" or an account ID and so
// inherit the bucket-policy grants written for that principal. Choosing the
// key ID now needs admin:CreateAccessKey at service level, and even then
// the ID may not collide with a user name, a reserved name or an account
// ID. Caller supplied secrets must be at least 16 characters (iam-4).
func TestChosenAccessKeyID(t *testing.T) {
	e := newEnv(t)
	e.putPolicy("team-helpdesk", map[string]any{
		"Effect":   "Allow",
		"Action":   []string{"admin:GetUser", "admin:CreateAccessKey", "admin:UpdateAccessKey", "admin:DeleteAccessKey"},
		"Resource": "arn:gos3:admin:::user/team-*",
	})
	e.putPolicy("key-admin", map[string]any{
		"Effect": "Allow", "Action": "admin:CreateAccessKey", "Resource": "arn:gos3:admin:::*",
	})
	help := e.addUser("helpdesk", "team-helpdesk")
	keyAdmin := e.addUser("keyadmin", "key-admin")
	e.addUser("team-a")
	e.addUser("alice")

	t.Run("scoped caller cannot choose the ID", func(t *testing.T) {
		for _, ak := range []string{"alice", "root", "123456789012", "TEAMAKEY1"} {
			msg := e.do(&help, "POST", Prefix+"/users/team-a/keys", map[string]any{"accessKey": ak}).
				expectError(t, http.StatusForbidden, codeAccessDenied)
			if !strings.Contains(msg, "choose the access key ID") {
				t.Errorf("message = %q", msg)
			}
		}
		if _, err := e.iam.AccessKeyOwner(context.Background(), "TEAMAKEY1"); !errors.Is(err, iam.ErrKeyNotFound) {
			t.Fatalf("key created anyway: %v", err)
		}
		// Generated IDs are still fine.
		e.do(&help, "POST", Prefix+"/users/team-a/keys", map[string]any{"description": "ci"}).expectStatus(t, http.StatusCreated)
	})
	t.Run("service-level caller", func(t *testing.T) {
		r := e.do(&keyAdmin, "POST", Prefix+"/users/team-a/keys", map[string]any{"accessKey": "TEAMAKEY1"})
		r.expectStatus(t, http.StatusCreated)
		for _, ak := range []string{"alice", "root", "Anonymous", "123456789012"} {
			e.do(&keyAdmin, "POST", Prefix+"/users/team-a/keys", map[string]any{"accessKey": ak}).
				expectError(t, http.StatusBadRequest, codeInvalidName)
			e.root("POST", "/users/team-a/keys", map[string]any{"accessKey": ak}).
				expectError(t, http.StatusBadRequest, codeInvalidName)
		}
		// A user cannot take a key's ID either.
		e.root("POST", "/users", map[string]any{"name": "TEAMAKEY1"}).expectError(t, http.StatusConflict, codeConflict)
		e.root("POST", "/users", map[string]any{"name": "123456789012"}).expectError(t, http.StatusBadRequest, codeInvalidName)
	})
	t.Run("secret length", func(t *testing.T) {
		msg := e.root("POST", "/users/team-a/keys", map[string]any{"secretKey": "fifteen-chars-x"}).
			expectError(t, http.StatusBadRequest, codeInvalidName)
		if !strings.Contains(msg, "16-128") {
			t.Errorf("message = %q", msg)
		}
		e.root("POST", "/users/team-a/keys", map[string]any{"secretKey": "sixteen-chars-xy"}).expectStatus(t, http.StatusCreated)
	})
}

// hookBody runs hook before its first Read, simulating a request body that
// arrives (at the client's pace) after the route was authorized.
type hookBody struct {
	hook func()
	r    io.Reader
}

func (b *hookBody) Read(p []byte) (int, error) {
	if b.hook != nil {
		b.hook()
		b.hook = nil
	}
	return b.r.Read(p)
}

func (b *hookBody) Close() error { return nil }

// TestKeyRouteOwnerBinding checks gap-authz-toctou-mutations-and-iam-races-4:
// PATCH/DELETE /keys/{accessKey} authorize against the key's owner, then
// act. If the key is deleted and re-created under the same ID for a user
// outside the caller's scope in between (while a PATCH body is still
// arriving), the change must not reach the new key.
func TestKeyRouteOwnerBinding(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putPolicy("team-helpdesk", map[string]any{
		"Effect":   "Allow",
		"Action":   []string{"admin:UpdateAccessKey", "admin:DeleteAccessKey"},
		"Resource": "arn:gos3:admin:::user/team-*",
	})
	help := e.addUser("helpdesk", "team-helpdesk")
	e.addUser("team-a")
	e.addUser("boss", iam.PolicyAdmin)
	helpID := auth.Identity{User: "helpdesk", AccessKey: help.AccessKey}

	// moveKey re-creates SHAREDID for boss.
	moveKey := func() {
		if err := e.iam.DeleteAccessKey(ctx, "SHAREDID"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.iam.CreateAccessKey(ctx, "boss", "", "SHAREDID", ""); err != nil {
			t.Fatal(err)
		}
	}
	// authorized runs the route's resolution and authorization as the
	// endpoint does, for a key that team-a owns at that point.
	authorized := func(method, action string, body io.ReadCloser) *reqCtx {
		t.Helper()
		if _, err := e.iam.CreateAccessKey(ctx, "team-a", "", "SHAREDID", ""); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, Prefix+"/keys/SHAREDID", nil)
		r.SetPathValue("accessKey", "SHAREDID")
		rc := &reqCtx{r: r, ctx: ctx, auth: &auth.Result{Identity: helpID, Body: body}, id: helpID, action: action}
		if err := e.api.requireAction(rc, action, e.api.keyOwnerARN(rc), nil); err != nil {
			t.Fatalf("in-scope key not authorized: %v", err)
		}
		return rc
	}

	t.Run("update", func(t *testing.T) {
		body := &hookBody{hook: moveKey, r: strings.NewReader(`{"status":"Inactive"}`)}
		rc := authorized("PATCH", "admin:UpdateAccessKey", body)
		if _, _, err := e.api.updateAccessKey(rc); e.api.toAPIError(err).Status != http.StatusNotFound {
			t.Fatalf("updateAccessKey = %v, want 404", err)
		}
		if _, id, err := e.iam.LookupCredentials("SHAREDID"); err != nil || id.User != "boss" {
			t.Fatalf("boss's key was changed: %+v %v", id, err)
		}
		if err := e.iam.DeleteAccessKey(ctx, "SHAREDID"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		rc := authorized("DELETE", "admin:DeleteAccessKey", nil)
		moveKey()
		if _, _, err := e.api.deleteAccessKey(rc); e.api.toAPIError(err).Status != http.StatusNotFound {
			t.Fatalf("deleteAccessKey = %v, want 404", err)
		}
		if owner, err := e.iam.AccessKeyOwner(ctx, "SHAREDID"); err != nil || owner != "boss" {
			t.Fatalf("boss's key was deleted: %q %v", owner, err)
		}
	})
	t.Run("unchanged owner", func(t *testing.T) {
		if err := e.iam.DeleteAccessKey(ctx, "SHAREDID"); err != nil {
			t.Fatal(err)
		}
		rc := authorized("DELETE", "admin:DeleteAccessKey", nil)
		if _, _, err := e.api.deleteAccessKey(rc); err != nil {
			t.Fatalf("deleteAccessKey: %v", err)
		}
	})
}
