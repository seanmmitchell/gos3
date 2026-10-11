package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestUsers(t *testing.T) {
	e := newEnv(t)

	// create with key
	r := e.root("POST", "/users", map[string]any{"name": "alice", "description": "A", "policies": []string{"readonly"}, "createKey": true})
	r.expectStatus(t, 201)
	var created createUserResponse
	r.json(t, &created)
	if created.User.Name != "alice" || created.Key == nil || created.Key.SecretKey == "" || created.Key.AccessKey == "" {
		t.Fatalf("created = %+v", created)
	}
	if len(created.User.Keys) != 1 || created.User.Keys[0].SecretKey != "" {
		t.Errorf("user keys = %+v", created.User.Keys)
	}
	// The new key works.
	alice := auth.Credentials{AccessKey: created.Key.AccessKey, SecretKey: created.Key.SecretKey}
	e.do(&alice, "GET", Prefix+"/whoami", nil).expectStatus(t, 200)

	// create without key: no "key" field
	r = e.root("POST", "/users", map[string]any{"name": "bob", "policies": []string{}})
	r.expectStatus(t, 201)
	if strings.Contains(string(r.body), `"key"`) {
		t.Errorf("unexpected key in %s", r.body)
	}

	e.root("POST", "/users", map[string]any{"name": "alice"}).expectError(t, 409, codeConflict)
	e.root("POST", "/users", map[string]any{"name": "bad name!"}).expectError(t, 400, codeInvalidName)
	e.root("POST", "/users", map[string]any{"name": "root"}).expectError(t, 400, codeInvalidName)
	e.root("POST", "/users", map[string]any{"name": "carol", "policies": []string{"nope"}}).expectError(t, 404, codeNotFound)

	// list
	r = e.root("GET", "/users", nil)
	r.expectStatus(t, 200)
	var list struct{ Users []iam.User }
	r.json(t, &list)
	if len(list.Users) != 2 || list.Users[0].Name != "alice" || list.Users[1].Name != "bob" {
		t.Errorf("users = %+v", list.Users)
	}
	if strings.Contains(string(r.body), created.Key.SecretKey) {
		t.Error("secret leaked in user list")
	}

	// get
	var u iam.User
	e.root("GET", "/users/alice", nil).json(t, &u)
	if u.Name != "alice" || u.Description != "A" {
		t.Errorf("user = %+v", u)
	}
	e.root("GET", "/users/nobody", nil).expectError(t, 404, codeNotFound)

	// patch
	r = e.root("PATCH", "/users/alice", map[string]any{"description": "B", "policies": []string{"readwrite", "diagnostics"}})
	r.expectStatus(t, 200)
	r.json(t, &u)
	if u.Description != "B" || len(u.Policies) != 2 {
		t.Errorf("patched = %+v", u)
	}
	e.root("PATCH", "/users/nobody", map[string]any{"description": "x"}).expectError(t, 404, codeNotFound)
	e.root("PATCH", "/users/alice", map[string]any{"policies": []string{"ghost"}}).expectError(t, 404, codeNotFound)
	e.root("PATCH", "/users/root", map[string]any{"description": "x"}).expectError(t, 400, codeInvalidRequest)
	e.root("PATCH", "/users/alice", map[string]any{"name": "x"}).expectError(t, 400, codeInvalidRequest)

	// access keys
	r = e.root("POST", "/users/bob/keys", map[string]any{"description": "ci", "accessKey": "BOBKEY1", "secretKey": "bob-secret-123"})
	r.expectStatus(t, 201)
	var k iam.AccessKey
	r.json(t, &k)
	if k.AccessKey != "BOBKEY1" || k.SecretKey != "bob-secret-123" || k.Status != iam.KeyActive || k.Description != "ci" {
		t.Errorf("key = %+v", k)
	}
	e.root("POST", "/users/bob/keys", map[string]any{"accessKey": "BOBKEY1"}).expectError(t, 409, codeConflict)
	e.root("POST", "/users/bob/keys", map[string]any{"accessKey": "x"}).expectError(t, 400, codeInvalidName)
	e.root("POST", "/users/nobody/keys", nil).expectError(t, 404, codeNotFound)
	e.root("POST", "/users/bob/keys", nil).expectStatus(t, 201) // body optional

	bob := auth.Credentials{AccessKey: "BOBKEY1", SecretKey: "bob-secret-123"}
	e.root("PATCH", "/keys/BOBKEY1", map[string]any{"status": "Inactive"}).expectStatus(t, 204)
	e.do(&bob, "GET", Prefix+"/whoami", nil).expectError(t, 401, codeUnauthenticated)
	e.root("PATCH", "/keys/BOBKEY1", map[string]any{"status": "Active"}).expectStatus(t, 204)
	e.do(&bob, "GET", Prefix+"/whoami", nil).expectStatus(t, 200)
	e.root("PATCH", "/keys/BOBKEY1", map[string]any{"status": "Bogus"}).expectError(t, 400, codeInvalidRequest)
	e.root("PATCH", "/keys/NOPE", map[string]any{"status": "Active"}).expectError(t, 404, codeNotFound)
	e.root("PATCH", "/keys/"+rootCreds.AccessKey, map[string]any{"status": "Inactive"}).expectError(t, 400, codeInvalidRequest)
	e.root("DELETE", "/keys/BOBKEY1", nil).expectStatus(t, 204)
	e.root("DELETE", "/keys/BOBKEY1", nil).expectError(t, 404, codeNotFound)

	// too many keys
	for range 9 {
		e.root("POST", "/users/bob/keys", nil).expectStatus(t, 201)
	}
	e.root("POST", "/users/bob/keys", nil).expectError(t, 400, codeInvalidRequest)

	// delete
	e.root("DELETE", "/users/alice", nil).expectStatus(t, 204)
	e.root("DELETE", "/users/alice", nil).expectError(t, 404, codeNotFound)
	e.root("DELETE", "/users/root", nil).expectError(t, 400, codeInvalidRequest)
	e.do(&alice, "GET", Prefix+"/whoami", nil).expectError(t, 401, codeUnauthenticated)
}

func TestOwnKeys(t *testing.T) {
	e := newEnv(t)
	// Listing and deleting own keys needs no grant; creating them needs an
	// explicit Allow (TestCreateOwnKeyNeedsExplicitAllow).
	e.putPolicy("own-keys", map[string]any{"Effect": "Allow", "Action": "admin:CreateOwnAccessKey", "Resource": "*"})
	alice := e.addUser("alice", "own-keys")
	other := e.addUser("other")

	r := e.do(&alice, "GET", Prefix+"/whoami/keys", nil)
	r.expectStatus(t, 200)
	var list struct{ Keys []iam.AccessKey }
	r.json(t, &list)
	if len(list.Keys) != 1 || list.Keys[0].AccessKey != alice.AccessKey || list.Keys[0].SecretKey != "" {
		t.Errorf("keys = %+v", list.Keys)
	}

	r = e.do(&alice, "POST", Prefix+"/whoami/keys", map[string]any{"description": "laptop"})
	r.expectStatus(t, 201)
	var k iam.AccessKey
	r.json(t, &k)
	if k.SecretKey == "" || k.Description != "laptop" {
		t.Errorf("key = %+v", k)
	}
	e.do(&alice, "POST", Prefix+"/whoami/keys", map[string]any{"accessKey": "CHOSEN"}).expectError(t, 400, codeInvalidRequest)
	e.do(&alice, "POST", Prefix+"/whoami/keys", nil).expectStatus(t, 201)

	// cannot delete other users' keys or the signing key
	e.do(&alice, "DELETE", Prefix+"/whoami/keys/"+other.AccessKey, nil).expectError(t, 404, codeNotFound)
	e.do(&alice, "DELETE", Prefix+"/whoami/keys/"+alice.AccessKey, nil).expectError(t, 409, codeConflict)
	// can delete another own key
	e.do(&alice, "DELETE", Prefix+"/whoami/keys/"+k.AccessKey, nil).expectStatus(t, 204)
	if _, _, err := e.iam.LookupCredentials(k.AccessKey); err == nil {
		t.Error("key still valid after delete")
	}
	if _, _, err := e.iam.LookupCredentials(other.AccessKey); err != nil {
		t.Error("other user's key was deleted")
	}

	// root
	r = e.root("GET", "/whoami/keys", nil)
	r.expectStatus(t, 200)
	if strings.TrimSpace(string(r.body)) != `{"keys":[]}` {
		t.Errorf("root keys = %s", r.body)
	}
	e.root("POST", "/whoami/keys", nil).expectError(t, 400, codeInvalidRequest)
	e.root("DELETE", "/whoami/keys/"+rootCreds.AccessKey, nil).expectError(t, 400, codeInvalidRequest)
}

func TestPolicies(t *testing.T) {
	e := newEnv(t)
	r := e.root("GET", "/policies", nil)
	r.expectStatus(t, 200)
	var list struct{ Policies []iam.NamedPolicy }
	r.json(t, &list)
	if len(list.Policies) != 5 || list.Policies[0].Name != iam.PolicyAdmin || !list.Policies[0].Builtin {
		t.Errorf("policies = %+v", list.Policies)
	}

	doc := map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::photos/*",
	}}}
	r = e.root("PUT", "/policies/photos-read", map[string]any{"description": "read photos", "document": doc})
	r.expectStatus(t, 200)
	var p iam.NamedPolicy
	r.json(t, &p)
	if p.Name != "photos-read" || p.Description != "read photos" || p.Builtin || len(p.Document.Statement) != 1 {
		t.Errorf("policy = %+v", p)
	}
	// document as a JSON string
	e.root("PUT", "/policies/as-string", map[string]any{
		"document": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:ListBucket","Resource":"*"}]}`,
	}).expectStatus(t, 200)

	e.root("GET", "/policies/photos-read", nil).expectStatus(t, 200)
	e.root("GET", "/policies/readonly", nil).expectStatus(t, 200)
	e.root("GET", "/policies/nope", nil).expectError(t, 404, codeNotFound)

	e.root("PUT", "/policies/readonly", map[string]any{"document": doc}).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", "/policies/bad", map[string]any{"document": map[string]any{"Statement": []any{map[string]any{"Effect": "Maybe"}}}}).
		expectError(t, 400, codeInvalidPolicy)
	e.root("PUT", "/policies/bad", map[string]any{"document": "{not json"}).expectError(t, 400, codeInvalidPolicy)
	e.root("PUT", "/policies/bad", map[string]any{}).expectError(t, 400, codeInvalidPolicy)
	e.root("PUT", "/policies/bad%20name", map[string]any{"document": doc}).expectError(t, 400, codeInvalidName)

	if _, err := e.iam.CreateUser(context.Background(), iam.UserSpec{Name: "u", Policies: []string{"photos-read"}}); err != nil {
		t.Fatal(err)
	}
	msg := e.root("DELETE", "/policies/photos-read", nil).expectError(t, 409, codeConflict)
	if !strings.Contains(msg, "u") {
		t.Errorf("message %q", msg)
	}
	e.root("DELETE", "/policies/admin", nil).expectError(t, 400, codeInvalidRequest)
	e.root("DELETE", "/policies/as-string", nil).expectStatus(t, 204)
	e.root("DELETE", "/policies/as-string", nil).expectError(t, 404, codeNotFound)
}

func TestSimulate(t *testing.T) {
	e := newEnv(t)
	e.addUser("reader", iam.PolicyReadOnly)
	e.createBucket("photos", storage.CreateBucketOptions{})
	e.createBucket("pub", storage.CreateBucketOptions{})
	ctx := context.Background()
	if err := e.backend.PutBucketConfig(ctx, "pub", storage.ConfigPolicy, []byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/secret/*"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.backend.PutBucketConfig(ctx, "photos", storage.ConfigACL, []byte("public-read")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name               string
		req                map[string]any
		allowed, explicitD bool
	}{
		{"reader get", map[string]any{"user": "reader", "action": "s3:GetObject", "bucket": "photos", "key": "a.jpg"}, true, false},
		{"reader put", map[string]any{"user": "reader", "action": "s3:PutObject", "bucket": "photos", "key": "a.jpg"}, false, false},
		{"reader admin", map[string]any{"user": "reader", "action": "admin:ListUsers"}, false, false},
		{"root", map[string]any{"user": "root", "action": "admin:ListUsers"}, true, false},
		{"anon bucket policy", map[string]any{"user": "", "action": "s3:GetObject", "bucket": "pub", "key": "x"}, true, false},
		{"anon explicit deny", map[string]any{"user": "", "action": "s3:GetObject", "bucket": "pub", "key": "secret/x"}, false, true},
		{"anon acl", map[string]any{"user": "", "action": "s3:ListBucket", "bucket": "photos"}, true, false},
		{"anon put", map[string]any{"user": "", "action": "s3:PutObject", "bucket": "photos", "key": "x"}, false, false},
		{"conditions", map[string]any{"user": "reader", "action": "s3:GetObject", "bucket": "photos", "key": "k",
			"conditions": map[string]any{"aws:SourceIp": "10.0.0.1", "s3:prefix": []string{"a", "b"}}}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.root("POST", "/authz/simulate", tt.req)
			r.expectStatus(t, 200)
			var d decisionJSON
			r.json(t, &d)
			if d.Allowed != tt.allowed || d.ExplicitDeny != tt.explicitD || d.Reason == "" {
				t.Errorf("decision = %+v (body %s)", d, r.body)
			}
		})
	}
	e.root("POST", "/authz/simulate", map[string]any{"user": "ghost", "action": "s3:GetObject"}).expectError(t, 404, codeNotFound)
	e.root("POST", "/authz/simulate", map[string]any{"user": "reader", "action": "s3:GetObject", "bucket": "nope"}).
		expectError(t, 404, codeNotFound)
	e.root("POST", "/authz/simulate", map[string]any{"user": "reader"}).expectError(t, 400, codeInvalidRequest)
	e.root("POST", "/authz/simulate", map[string]any{"user": "reader", "action": "s3:GetObject", "key": "k"}).
		expectError(t, 400, codeInvalidRequest)
}
