package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
)

// TestAdminBodySigning checks end to end that admin requests with a body
// must sign it (auth-1): UNSIGNED-PAYLOAD and a tampered body are refused
// and leave the user unchanged, a signed body and a bodiless UNSIGNED-PAYLOAD
// GET are accepted.
func TestAdminBodySigning(t *testing.T) {
	t.Parallel()
	e := fsEnv
	name := userName("bodysig")
	e.createUser(t, name)

	body, _ := json.Marshal(map[string]any{"policies": []string{"admin"}})
	hdr := map[string]string{"Content-Type": "application/json"}
	r := e.do(t, rawRequest{method: http.MethodPatch, path: adminPrefix + "/users/" + name, body: body, header: hdr,
		creds: &rootCreds, payloadHash: auth.PayloadUnsigned})
	if r.status != http.StatusBadRequest {
		t.Errorf("UNSIGNED-PAYLOAD PATCH: %d %s", r.status, r.body)
	}
	// Signed for one body, sent with another.
	r = e.do(t, rawRequest{method: http.MethodPatch, path: adminPrefix + "/users/" + name, body: body, header: hdr,
		creds: &rootCreds, payloadHash: hashHex(`{"description":"benign"}`)})
	if r.status != http.StatusBadRequest {
		t.Errorf("tampered PATCH: %d %s", r.status, r.body)
	}
	var u struct {
		Policies []string `json:"policies"`
	}
	if r := e.admin(t, rootCreds, http.MethodGet, "/users/"+name, nil, &u); r.status != 200 || len(u.Policies) != 0 {
		t.Fatalf("user after refused PATCHes: %d %+v", r.status, u)
	}
	if r := e.do(t, rawRequest{method: http.MethodGet, path: adminPrefix + "/users/" + name, creds: &rootCreds,
		payloadHash: auth.PayloadUnsigned}); r.status != 200 {
		t.Errorf("UNSIGNED-PAYLOAD GET: %d %s", r.status, r.body)
	}
	if r := e.admin(t, rootCreds, http.MethodPatch, "/users/"+name, map[string]any{"description": "signed"}, nil); r.status != 200 {
		t.Errorf("signed PATCH: %d %s", r.status, r.body)
	}
}

// TestAdminSelfServiceAndScoping checks end to end that self-service key
// creation needs an explicit Allow (auth-2) and that admin actions can be
// scoped to users (iam-1).
func TestAdminSelfServiceAndScoping(t *testing.T) {
	t.Parallel()
	e := fsEnv
	plain := e.createUser(t, userName("selfsvc"))
	if r := e.admin(t, plain, http.MethodPost, "/whoami/keys", map[string]any{}, nil); r.status != http.StatusForbidden {
		t.Errorf("default self-service key creation: %d %s", r.status, r.body)
	}
	pol := userName("ownkeys")
	e.putPolicy(t, pol, `{"Statement":[{"Effect":"Allow","Action":"admin:CreateOwnAccessKey","Resource":"*"}]}`)
	allowed := e.createUser(t, userName("selfsvc"), pol)
	if r := e.admin(t, allowed, http.MethodPost, "/whoami/keys", map[string]any{}, nil); r.status != http.StatusCreated {
		t.Errorf("allowed self-service key creation: %d %s", r.status, r.body)
	}

	scope := userName("scoped")
	e.putPolicy(t, scope, `{"Statement":[{"Effect":"Allow","Action":"admin:UpdateUser","Resource":"arn:gos3:admin:::user/`+scope+`-team-*",
		"Condition":{"ForAllValues:StringEquals":{"admin:PolicyName":["readonly"]}}}]}`)
	helper := e.createUser(t, userName("helper"), scope)
	target := scope + "-team-a"
	e.createUser(t, target)
	if r := e.admin(t, helper, http.MethodPatch, "/users/"+target, map[string]any{"policies": []string{"readonly"}}, nil); r.status != 200 {
		t.Errorf("in-scope readonly: %d %s", r.status, r.body)
	}
	if r := e.admin(t, helper, http.MethodPatch, "/users/"+target, map[string]any{"policies": []string{"admin"}}, nil); r.status != http.StatusForbidden {
		t.Errorf("in-scope admin: %d %s", r.status, r.body)
	}
	if r := e.admin(t, helper, http.MethodPatch, "/users/"+userName("other"), map[string]any{"policies": []string{"readonly"}}, nil); r.status != http.StatusForbidden {
		t.Errorf("out of scope: %d %s", r.status, r.body)
	}
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
