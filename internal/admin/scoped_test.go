package admin

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestScopedUserAdmin checks iam-1: admin actions on users are evaluated
// against arn:gos3:admin:::user/<name>, so a delegated administrator can be
// confined to some users. Previously admin:UpdateUser on any resource let
// the holder attach "admin" to itself.
func TestScopedUserAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putPolicy("team-helpdesk", map[string]any{
		"Effect":   "Allow",
		"Action":   []string{"admin:GetUser", "admin:UpdateUser", "admin:DeleteUser", "admin:CreateAccessKey", "admin:UpdateAccessKey", "admin:DeleteAccessKey", "admin:CreateUser"},
		"Resource": "arn:gos3:admin:::user/team-*",
	}, map[string]any{
		"Effect": "Allow", "Action": "admin:ListUsers", "Resource": "arn:gos3:admin:::*",
	})
	help := e.addUser("helpdesk", "team-helpdesk")
	e.addUser("team-a")
	bob := e.addUser("bob")
	boss := e.addUser("boss", iam.PolicyAdmin)

	t.Run("whoami", func(t *testing.T) {
		// Creating users is scoped, so manageUsers (service level) is
		// false; listUsers tells the UI to show the users page anyway.
		var w whoAmI
		e.do(&help, "GET", Prefix+"/whoami", nil).json(t, &w)
		if c := w.Capabilities; !c.ListUsers || c.ManageUsers || !c.Admin {
			t.Errorf("capabilities = %+v, want listUsers and admin without manageUsers", c)
		}
	})
	t.Run("in scope", func(t *testing.T) {
		e.do(&help, "GET", Prefix+"/users/team-a", nil).expectStatus(t, http.StatusOK)
		e.do(&help, "PATCH", Prefix+"/users/team-a", map[string]any{"description": "managed"}).expectStatus(t, http.StatusOK)
		e.do(&help, "PATCH", Prefix+"/users/team-a", map[string]any{"disabled": true}).expectStatus(t, http.StatusOK)
		e.do(&help, "PATCH", Prefix+"/users/team-a", map[string]any{"disabled": false}).expectStatus(t, http.StatusOK)
		e.do(&help, "GET", Prefix+"/users", nil).expectStatus(t, http.StatusOK)
		r := e.do(&help, "POST", Prefix+"/users/team-a/keys", nil)
		r.expectStatus(t, http.StatusCreated)
		var k iam.AccessKey
		r.json(t, &k)
		e.do(&help, "PATCH", Prefix+"/keys/"+k.AccessKey, map[string]any{"status": "Inactive"}).expectStatus(t, http.StatusNoContent)
		e.do(&help, "DELETE", Prefix+"/keys/"+k.AccessKey, nil).expectStatus(t, http.StatusNoContent)
		e.do(&help, "POST", Prefix+"/users", map[string]any{"name": "team-new", "createKey": true}).expectStatus(t, http.StatusCreated)
		e.do(&help, "DELETE", Prefix+"/users/team-new", nil).expectStatus(t, http.StatusNoContent)
	})
	t.Run("out of scope", func(t *testing.T) {
		for _, tc := range []struct{ method, path string }{
			{"GET", "/users/bob"},
			{"PATCH", "/users/bob"},
			{"DELETE", "/users/bob"},
			{"POST", "/users/bob/keys"},
			{"PATCH", "/keys/" + bob.AccessKey},
			{"DELETE", "/keys/" + bob.AccessKey},
			{"PATCH", "/keys/" + boss.AccessKey},
			{"POST", "/users/boss/keys"},
			// itself
			{"GET", "/users/helpdesk"},
			{"PATCH", "/users/helpdesk"},
			{"POST", "/users/helpdesk/keys"},
			// unknown keys look like out-of-scope ones
			{"DELETE", "/keys/NOSUCHKEY"},
		} {
			var body any
			switch tc.path {
			case "/users/bob", "/users/helpdesk":
				if tc.method == "PATCH" {
					body = map[string]any{"policies": []string{"admin"}}
				}
			}
			if tc.method == "PATCH" && body == nil {
				body = map[string]any{"status": "Inactive"}
			}
			e.do(&help, tc.method, Prefix+tc.path, body).expectError(t, http.StatusForbidden, codeAccessDenied)
		}
		e.do(&help, "POST", Prefix+"/users", map[string]any{"name": "outsider"}).expectError(t, http.StatusForbidden, codeAccessDenied)
		for _, n := range []string{"bob", "helpdesk", "boss"} {
			u, err := e.iam.GetUser(ctx, n)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(u.Policies, iam.PolicyAdmin) && n != "boss" || len(u.Keys) != 1 || u.Keys[0].Status != iam.KeyActive {
				t.Errorf("%s changed: %+v", n, u)
			}
		}
		if _, err := e.iam.GetUser(ctx, "outsider"); err == nil {
			t.Error("outsider created")
		}
		// Full admins are unaffected, and see 404 for unknown keys.
		e.do(&boss, "DELETE", Prefix+"/keys/NOSUCHKEY", nil).expectError(t, http.StatusNotFound, codeNotFound)
	})
	t.Run("policy attachment needs its own scope", func(t *testing.T) {
		// UpdateUser in scope, but attaching policies is limited by
		// admin:PolicyName below; here no such limit exists, so attaching
		// "admin" to an in-scope user works: the docs call this out.
		e.do(&help, "PATCH", Prefix+"/users/team-a", map[string]any{"policies": []string{"readonly"}}).expectStatus(t, http.StatusOK)
	})
}

// TestPolicyNameCondition checks the admin:PolicyName condition key on
// CreateUser and UpdateUser.
func TestPolicyNameCondition(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.putPolicy("attach-readonly", map[string]any{
		"Effect": "Allow", "Action": []string{"admin:UpdateUser", "admin:CreateUser"}, "Resource": "arn:gos3:admin:::user/team-*",
		"Condition": map[string]any{"ForAllValues:StringEquals": map[string]any{"admin:PolicyName": []string{"readonly"}}},
	})
	e.putPolicy("never-admin", map[string]any{
		"Effect": "Allow", "Action": "admin:UpdateUser", "Resource": "*",
	}, map[string]any{
		"Effect": "Deny", "Action": []string{"admin:UpdateUser", "admin:CreateUser"}, "Resource": "*",
		"Condition": map[string]any{"ForAnyValue:StringEquals": map[string]any{"admin:PolicyName": "admin"}},
	})
	d := e.addUser("delegate", "attach-readonly")
	e.addUser("team-a")

	e.do(&d, "PATCH", Prefix+"/users/team-a", map[string]any{"policies": []string{"readonly"}}).expectStatus(t, http.StatusOK)
	e.do(&d, "PATCH", Prefix+"/users/team-a", map[string]any{"policies": []string{"admin"}}).expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&d, "PATCH", Prefix+"/users/team-a", map[string]any{"policies": []string{"readonly", "admin"}}).expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&d, "PATCH", Prefix+"/users/team-a", map[string]any{"description": "no policies in the body"}).expectStatus(t, http.StatusOK)
	e.do(&d, "PATCH", Prefix+"/users/delegate", map[string]any{"policies": []string{"readonly"}}).expectError(t, http.StatusForbidden, codeAccessDenied)
	if u, _ := e.iam.GetUser(ctx, "team-a"); !slices.Equal(u.Policies, []string{"readonly"}) {
		t.Errorf("team-a policies = %v", u.Policies)
	}

	e.do(&d, "POST", Prefix+"/users", map[string]any{"name": "team-b", "policies": []string{"readonly"}}).expectStatus(t, http.StatusCreated)
	e.do(&d, "POST", Prefix+"/users", map[string]any{"name": "team-c", "policies": []string{"admin"}}).expectError(t, http.StatusForbidden, codeAccessDenied)
	if _, err := e.iam.GetUser(ctx, "team-c"); err == nil {
		t.Error("team-c created with admin")
	}

	n := e.addUser("nonadmin", "never-admin")
	e.do(&n, "PATCH", Prefix+"/users/team-a", map[string]any{"policies": []string{"readwrite"}}).expectStatus(t, http.StatusOK)
	e.do(&n, "PATCH", Prefix+"/users/nonadmin", map[string]any{"policies": []string{"never-admin", "admin"}}).expectError(t, http.StatusForbidden, codeAccessDenied)
	if u, _ := e.iam.GetUser(ctx, "nonadmin"); slices.Contains(u.Policies, "admin") {
		t.Error("nonadmin escalated")
	}
}

// TestScopedPolicyAndBucketAdmin checks per-policy and per-bucket scoping.
func TestScopedPolicyAndBucketAdmin(t *testing.T) {
	e := newEnv(t)
	e.putPolicy("delegated-pols", map[string]any{
		"Effect": "Allow", "Action": []string{"admin:GetPolicy", "admin:PutPolicy", "admin:DeletePolicy"}, "Resource": "arn:gos3:admin:::policy/team-*",
	}, map[string]any{
		"Effect": "Allow", "Action": []string{"admin:GetBucket", "admin:ForceDeleteBucket"}, "Resource": "arn:gos3:admin:::bucket/scratch-*",
	})
	p := e.addUser("pol", "delegated-pols")
	doc := map[string]any{"document": map[string]any{"Statement": []any{map[string]any{"Effect": "Allow", "Action": "*", "Resource": "*"}}}}

	e.do(&p, "PUT", Prefix+"/policies/team-x", doc).expectStatus(t, http.StatusOK)
	e.do(&p, "GET", Prefix+"/policies/team-x", nil).expectStatus(t, http.StatusOK)
	e.do(&p, "DELETE", Prefix+"/policies/team-x", nil).expectStatus(t, http.StatusNoContent)
	// Its own policy is out of scope, so it cannot rewrite it to Action "*".
	e.do(&p, "PUT", Prefix+"/policies/delegated-pols", doc).expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&p, "GET", Prefix+"/policies/delegated-pols", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&p, "DELETE", Prefix+"/policies/delegated-pols", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&p, "GET", Prefix+"/policies/admin", nil).expectError(t, http.StatusForbidden, codeAccessDenied)

	e.createBucket("scratch-1", storage.CreateBucketOptions{})
	e.createBucket("prod", storage.CreateBucketOptions{})
	e.do(&p, "GET", Prefix+"/buckets/scratch-1", nil).expectStatus(t, http.StatusOK)
	e.do(&p, "GET", Prefix+"/buckets/prod", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&p, "DELETE", Prefix+"/buckets/prod?force=true", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&p, "DELETE", Prefix+"/buckets/scratch-1?force=true", nil).expectStatus(t, http.StatusNoContent)
	// Listing stays service level.
	e.do(&p, "GET", Prefix+"/buckets", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
}

// TestUnscopedGrantsStillWork checks that existing "*" and
// arn:gos3:admin:::* grants cover every per-target resource, and that s3
// ARNs never grant admin actions.
func TestUnscopedGrantsStillWork(t *testing.T) {
	e := newEnv(t)
	e.putPolicy("star", map[string]any{"Effect": "Allow", "Action": "admin:*", "Resource": "*"})
	e.putPolicy("adminarn", map[string]any{"Effect": "Allow", "Action": "admin:*", "Resource": "arn:gos3:admin:::*"})
	e.putPolicy("s3arn", map[string]any{"Effect": "Allow", "Action": "admin:*", "Resource": []string{"arn:aws:s3:::*", "arn:aws:s3:::*/*"}})
	e.createBucket("b1", storage.CreateBucketOptions{})
	e.addUser("target")
	for _, pol := range []string{"star", "adminarn", "s3arn"} {
		c := e.addUser("u-"+pol, pol)
		want := http.StatusOK
		if pol == "s3arn" {
			want = http.StatusForbidden
		}
		for _, p := range []string{"/users", "/users/target", "/policies/readonly", "/buckets/b1", "/config"} {
			if r := e.do(&c, "GET", Prefix+p, nil); r.status != want {
				t.Errorf("%s GET %s = %d, want %d (%s)", pol, p, r.status, want, r.body)
			}
		}
		if pol != "s3arn" {
			e.do(&c, "PATCH", Prefix+"/users/target", map[string]any{"policies": []string{"admin"}}).expectStatus(t, http.StatusOK)
			e.do(&c, "PATCH", Prefix+"/users/target", map[string]any{"policies": []string{}}).expectStatus(t, http.StatusOK)
		}
		r := e.do(&c, "GET", Prefix+"/whoami", nil)
		r.expectStatus(t, http.StatusOK)
		var w whoAmI
		r.json(t, &w)
		if w.Capabilities.ManageUsers != (pol != "s3arn") {
			t.Errorf("%s: whoami capabilities %+v", pol, w.Capabilities)
		}
	}
}

// TestSimulateAdminResources checks that the policy simulator accepts the
// per-target admin resources and the admin:PolicyName key.
func TestSimulateAdminResources(t *testing.T) {
	e := newEnv(t)
	e.putPolicy("team", map[string]any{
		"Effect": "Allow", "Action": "admin:UpdateUser", "Resource": "arn:gos3:admin:::user/team-*",
		"Condition": map[string]any{"ForAllValues:StringEquals": map[string]any{"admin:PolicyName": "readonly"}},
	})
	e.addUser("helper", "team")
	sim := func(body map[string]any) decisionJSON {
		t.Helper()
		r := e.root("POST", "/authz/simulate", body)
		r.expectStatus(t, http.StatusOK)
		var d decisionJSON
		r.json(t, &d)
		return d
	}
	if d := sim(map[string]any{"user": "helper", "action": "admin:UpdateUser", "resource": "arn:gos3:admin:::user/team-a"}); !d.Allowed {
		t.Errorf("team-a: %+v", d)
	}
	if d := sim(map[string]any{"user": "helper", "action": "admin:UpdateUser", "resource": "arn:gos3:admin:::user/bob"}); d.Allowed {
		t.Errorf("bob: %+v", d)
	}
	if d := sim(map[string]any{"user": "helper", "action": "admin:UpdateUser", "resource": "arn:gos3:admin:::user/team-a",
		"conditions": map[string]any{"admin:PolicyName": []string{"admin"}}}); d.Allowed {
		t.Errorf("attach admin: %+v", d)
	}
	if d := sim(map[string]any{"user": "helper", "action": "admin:UpdateUser"}); d.Allowed {
		t.Errorf("service level: %+v", d)
	}
	for _, bad := range []map[string]any{
		{"user": "helper", "action": "admin:UpdateUser", "resource": "arn:aws:s3:::b"},
		{"user": "helper", "action": "admin:UpdateUser", "resource": "arn:gos3:admin:::thing/x"},
		{"user": "helper", "action": "admin:UpdateUser", "resource": "arn:gos3:admin:::user/"},
		{"user": "helper", "action": "admin:UpdateUser", "bucket": "b"},
		{"user": "helper", "action": "s3:GetObject", "resource": "arn:gos3:admin:::user/x"},
	} {
		e.root("POST", "/authz/simulate", bad).expectError(t, http.StatusBadRequest, codeInvalidRequest)
	}
}

// TestEveryActionRouteAuthorizes checks that every route with an action,
// including those authorizing in their handler (inHandler), refuses a user
// without policies, and that root passes every route's authorization.
func TestEveryActionRouteAuthorizes(t *testing.T) {
	e := newEnv(t)
	nobody := e.addUser("nobody")
	for _, rt := range e.api.routes() {
		if rt.action == "" {
			continue
		}
		path := strings.NewReplacer("{name}", "x", "{accessKey}", "NOSUCHKEY").Replace(rt.path)
		var body any
		if rt.method != "GET" && rt.method != "DELETE" {
			body = map[string]any{}
		}
		msg := e.do(&nobody, rt.method, Prefix+path, body).expectError(t, http.StatusForbidden, codeAccessDenied)
		if !strings.Contains(msg, "admin:"+rt.action) {
			t.Errorf("%s %s: message %q", rt.method, rt.path, msg)
		}
		if r := e.do(&rootCreds, rt.method, Prefix+path, body); r.status == http.StatusForbidden {
			t.Errorf("root %s %s: %d %s", rt.method, rt.path, r.status, r.body)
		}
	}
}
