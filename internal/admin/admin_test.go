package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/auth"
	"github.com/seanmmitchell/gos3/internal/iam"
)

func TestAuthentication(t *testing.T) {
	e := newEnv(t)
	t.Run("anonymous", func(t *testing.T) {
		msg := e.do(nil, "GET", Prefix+"/whoami", nil).expectError(t, 401, codeUnauthenticated)
		if !strings.Contains(msg, "anonymous") {
			t.Errorf("message %q", msg)
		}
	})
	t.Run("bad signature", func(t *testing.T) {
		bad := auth.Credentials{AccessKey: rootCreds.AccessKey, SecretKey: "wrong-secret"}
		msg := e.do(&bad, "GET", Prefix+"/whoami", nil).expectError(t, 401, codeUnauthenticated)
		if !strings.Contains(msg, "SignatureDoesNotMatch") {
			t.Errorf("message %q does not carry the S3 code", msg)
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		bad := auth.Credentials{AccessKey: "NOSUCHKEY", SecretKey: "whatever-secret"}
		msg := e.do(&bad, "GET", Prefix+"/whoami", nil).expectError(t, 401, codeUnauthenticated)
		if !strings.Contains(msg, "InvalidAccessKeyId") {
			t.Errorf("message %q", msg)
		}
	})
	t.Run("payload hash mismatch", func(t *testing.T) {
		r := e.root("POST", "/users", `{"name":"x"}`)
		r.expectStatus(t, 201)
		// Sign one body, send another.
		req, _ := http.NewRequest("POST", e.srv.URL+Prefix+"/users", strings.NewReader(`{"name":"y"}`))
		if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3",
			"0000000000000000000000000000000000000000000000000000000000000000", time.Now()); err != nil {
			t.Fatal(err)
		}
		msg := e.send(req).expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, "XAmzContentSHA256Mismatch") {
			t.Errorf("message %q", msg)
		}
		if _, err := e.iam.GetUser(context.Background(), "y"); err == nil {
			t.Error("user created despite payload hash mismatch")
		}
	})
	t.Run("presigned GET", func(t *testing.T) {
		u, err := auth.PresignURL("GET", e.srv.URL+Prefix+"/whoami", rootCreds, "us-east-1", time.Minute, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("GET", u, nil)
		r := e.send(req)
		r.expectStatus(t, 200)
		var w whoAmI
		r.json(t, &w)
		if !w.IsRoot || w.User != "root" {
			t.Errorf("whoami = %+v", w)
		}
	})
	t.Run("unsigned payload", func(t *testing.T) {
		req, _ := http.NewRequest("POST", e.srv.URL+Prefix+"/users", strings.NewReader(`{"name":"unsigned-payload"}`))
		if err := auth.SignRequest(req, rootCreds, "us-east-1", "s3", auth.PayloadUnsigned, time.Now()); err != nil {
			t.Fatal(err)
		}
		e.send(req).expectStatus(t, 201)
	})
}

func TestAuthorization(t *testing.T) {
	e := newEnv(t)
	rw := e.addUser("alice", iam.PolicyReadWrite)
	diag := e.addUser("diag", iam.PolicyDiagnostics)
	adm := e.addUser("boss", iam.PolicyAdmin)

	t.Run("non-admin denied", func(t *testing.T) {
		for _, p := range []string{"/info", "/stats", "/config", "/users", "/policies", "/buckets", "/discovery"} {
			msg := e.do(&rw, "GET", Prefix+p, nil).expectError(t, 403, codeAccessDenied)
			if !strings.Contains(msg, "alice") || !strings.Contains(msg, "admin:") {
				t.Errorf("%s: message %q", p, msg)
			}
		}
	})
	t.Run("non-admin whoami", func(t *testing.T) {
		r := e.do(&rw, "GET", Prefix+"/whoami", nil)
		r.expectStatus(t, 200)
		var w whoAmI
		r.json(t, &w)
		if w.User != "alice" || w.IsRoot || w.AccessKey != rw.AccessKey {
			t.Errorf("whoami = %+v", w)
		}
		if len(w.Policies) != 1 || w.Policies[0] != iam.PolicyReadWrite {
			t.Errorf("policies = %v", w.Policies)
		}
		if w.Capabilities != (capabilities{}) {
			t.Errorf("capabilities = %+v", w.Capabilities)
		}
		if w.Server.ID != e.info.ID {
			t.Errorf("server = %+v", w.Server)
		}
	})
	t.Run("diagnostics", func(t *testing.T) {
		for _, p := range []string{"/info", "/stats", "/discovery"} {
			e.do(&diag, "GET", Prefix+p, nil).expectStatus(t, 200)
		}
		for _, p := range []string{"/config", "/users", "/policies", "/buckets"} {
			e.do(&diag, "GET", Prefix+p, nil).expectError(t, 403, codeAccessDenied)
		}
		r := e.do(&diag, "GET", Prefix+"/whoami", nil)
		var w whoAmI
		r.json(t, &w)
		want := capabilities{Admin: true, ViewInfo: true, ViewStats: true, Discovery: true}
		if w.Capabilities != want {
			t.Errorf("capabilities = %+v, want %+v", w.Capabilities, want)
		}
	})
	t.Run("admin policy and root", func(t *testing.T) {
		all := capabilities{true, true, true, true, true, true, true, true, true}
		for _, c := range []auth.Credentials{adm, rootCreds} {
			r := e.do(&c, "GET", Prefix+"/whoami", nil)
			var w whoAmI
			r.json(t, &w)
			if w.Capabilities != all {
				t.Errorf("%s capabilities = %+v", c.AccessKey, w.Capabilities)
			}
			e.do(&c, "GET", Prefix+"/users", nil).expectStatus(t, 200)
		}
		var w whoAmI
		e.root("GET", "/whoami", nil).json(t, &w)
		if !w.IsRoot || w.User != "root" || len(w.Policies) != 1 || w.Policies[0] != iam.PolicyAdmin {
			t.Errorf("root whoami = %+v", w)
		}
	})
	t.Run("disabled user", func(t *testing.T) {
		disabled := true
		if _, err := e.iam.UpdateUser(context.Background(), "boss", iam.UserUpdate{Disabled: &disabled}); err != nil {
			t.Fatal(err)
		}
		e.do(&adm, "GET", Prefix+"/users", nil).expectError(t, 401, codeUnauthenticated)
	})
}

func TestRouting(t *testing.T) {
	e := newEnv(t)
	t.Run("unknown path", func(t *testing.T) {
		e.root("GET", "/nope", nil).expectError(t, 404, codeNotFound)
		e.root("GET", "/users/a/b/c", nil).expectError(t, 404, codeNotFound)
		e.root("GET", "", nil).expectError(t, 404, codeNotFound)
		e.do(&rootCreds, "GET", "/_gos3/admin/v2/info", nil).expectError(t, 404, codeNotFound)
	})
	t.Run("non-canonical path", func(t *testing.T) {
		req, _ := http.NewRequest("GET", e.srv.URL+Prefix+"/x/../info", nil)
		req.URL.Opaque = Prefix + "/x/../info"
		e.send(req).expectError(t, 404, codeNotFound)
	})
	t.Run("method not allowed", func(t *testing.T) {
		r := e.root("POST", "/info", nil)
		r.expectError(t, 405, codeMethodNotAllowed)
		if allow := r.header.Get("Allow"); !strings.Contains(allow, "GET") || strings.Contains(allow, "POST") {
			t.Errorf("Allow = %q", allow)
		}
		r = e.root("PUT", "/users/bob", nil)
		r.expectError(t, 405, codeMethodNotAllowed)
		for _, m := range []string{"GET", "PATCH", "DELETE"} {
			if !strings.Contains(r.header.Get("Allow"), m) {
				t.Errorf("Allow %q lacks %s", r.header.Get("Allow"), m)
			}
		}
	})
	t.Run("405 before auth", func(t *testing.T) {
		e.do(nil, "DELETE", Prefix+"/info", nil).expectError(t, 405, codeMethodNotAllowed)
	})
	t.Run("HEAD", func(t *testing.T) {
		e.root("HEAD", "/info", nil).expectStatus(t, 200)
	})
	t.Run("response headers", func(t *testing.T) {
		r := e.root("GET", "/info", nil)
		if r.header.Get("Cache-Control") != "no-store" || r.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("headers = %v", r.header)
		}
	})
}

func TestBodyHandling(t *testing.T) {
	e := newEnv(t)
	t.Run("too large", func(t *testing.T) {
		big := `{"name":"` + strings.Repeat("a", maxBodySize) + `"}`
		msg := e.root("POST", "/users", big).expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, "exceeds") {
			t.Errorf("message %q", msg)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		msg := e.root("POST", "/users", `{"name":"a","bogus":1}`).expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, "bogus") {
			t.Errorf("message %q", msg)
		}
	})
	t.Run("trailing data", func(t *testing.T) {
		e.root("POST", "/users", `{"name":"a"} {}`).expectError(t, 400, codeInvalidRequest)
	})
	t.Run("syntax error", func(t *testing.T) {
		e.root("POST", "/users", `{"name":`).expectError(t, 400, codeInvalidRequest)
	})
	t.Run("wrong type", func(t *testing.T) {
		msg := e.root("POST", "/users", `{"name":5}`).expectError(t, 400, codeInvalidRequest)
		if !strings.Contains(msg, "name") {
			t.Errorf("message %q", msg)
		}
	})
	t.Run("missing body", func(t *testing.T) {
		e.root("POST", "/users", nil).expectError(t, 400, codeInvalidRequest)
	})
}
