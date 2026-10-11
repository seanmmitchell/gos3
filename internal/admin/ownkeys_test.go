package admin

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/seanmmitchell/gos3/internal/iam"
)

// syncBuffer is a log sink safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func withLogger(l *slog.Logger) envOption { return func(d *Deps) { d.Logger = l } }

// TestCreateOwnKeyNeedsExplicitAllow checks auth-2: minting a fresh access
// key for oneself needs an explicit Allow of admin:CreateOwnAccessKey.
// Previously every user could do it unless explicitly denied, so a stolen
// key could mint keys that survived revoking it.
func TestCreateOwnKeyNeedsExplicitAllow(t *testing.T) {
	var logs syncBuffer
	e := newEnv(t, withLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	e.putPolicy("own-keys", map[string]any{"Effect": "Allow", "Action": "admin:CreateOwnAccessKey", "Resource": "*"})
	e.putPolicy("deny-own-keys", map[string]any{"Effect": "Deny", "Action": "admin:CreateOwnAccessKey", "Resource": "*"})

	t.Run("default user refused", func(t *testing.T) {
		for _, pols := range [][]string{nil, {iam.PolicyReadWrite}, {iam.PolicyDiagnostics}} {
			name := "plain" + strings.Join(pols, "-")
			c := e.addUser(name, pols...)
			msg := e.do(&c, "POST", Prefix+"/whoami/keys", map[string]any{"description": "x"}).expectError(t, http.StatusForbidden, codeAccessDenied)
			if !strings.Contains(msg, "admin:CreateOwnAccessKey") {
				t.Errorf("message %q", msg)
			}
			u, _ := e.iam.GetUser(context.Background(), name)
			if len(u.Keys) != 1 {
				t.Errorf("%s has %d keys", name, len(u.Keys))
			}
		}
	})
	t.Run("explicit allow", func(t *testing.T) {
		c := e.addUser("allowed", "own-keys")
		r := e.do(&c, "POST", Prefix+"/whoami/keys", map[string]any{"description": "laptop"})
		r.expectStatus(t, http.StatusCreated)
		var k iam.AccessKey
		r.json(t, &k)
		if k.SecretKey == "" || k.Description != "laptop" {
			t.Errorf("key = %+v", k)
		}
		out := logs.String()
		if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "user=allowed") ||
			!strings.Contains(out, "accessKey="+k.AccessKey) || !strings.Contains(out, "signedBy="+c.AccessKey) {
			t.Errorf("no creation log entry in %q", out)
		}
	})
	t.Run("admin policy allows", func(t *testing.T) {
		c := e.addUser("boss", iam.PolicyAdmin)
		e.do(&c, "POST", Prefix+"/whoami/keys", nil).expectStatus(t, http.StatusCreated)
	})
	t.Run("explicit deny wins", func(t *testing.T) {
		c := e.addUser("both", "own-keys", "deny-own-keys")
		e.do(&c, "POST", Prefix+"/whoami/keys", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
		c = e.addUser("boss-denied", iam.PolicyAdmin, "deny-own-keys")
		e.do(&c, "POST", Prefix+"/whoami/keys", nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	})
	t.Run("root still refused", func(t *testing.T) {
		e.root("POST", "/whoami/keys", nil).expectError(t, http.StatusBadRequest, codeInvalidRequest)
	})
	t.Run("delete own key unchanged", func(t *testing.T) {
		c := e.addUser("deleter")
		k, err := e.iam.CreateAccessKey(context.Background(), "deleter", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		// Allowed without any grant ...
		e.do(&c, "DELETE", Prefix+"/whoami/keys/"+k.AccessKey, nil).expectStatus(t, http.StatusNoContent)
		// ... and refused by an explicit Deny.
		e.putPolicy("deny-del", map[string]any{"Effect": "Deny", "Action": "admin:DeleteOwnAccessKey", "Resource": "*"})
		c2 := e.addUser("nodelete", "deny-del")
		k2, err := e.iam.CreateAccessKey(context.Background(), "nodelete", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		e.do(&c2, "DELETE", Prefix+"/whoami/keys/"+k2.AccessKey, nil).expectError(t, http.StatusForbidden, codeAccessDenied)
	})
	t.Run("whoami capability", func(t *testing.T) {
		cases := []struct {
			name string
			pols []string
			want bool
		}{
			{"cap-none", nil, false},
			{"cap-own", []string{"own-keys"}, true},
			{"cap-denied", []string{"own-keys", "deny-own-keys"}, false},
			{"cap-admin", []string{iam.PolicyAdmin}, true},
		}
		for _, tc := range cases {
			c := e.addUser(tc.name, tc.pols...)
			r := e.do(&c, "GET", Prefix+"/whoami", nil)
			r.expectStatus(t, http.StatusOK)
			var w whoAmI
			r.json(t, &w)
			if w.Capabilities.CreateOwnKeys != tc.want {
				t.Errorf("%s: createOwnKeys = %v, want %v", tc.name, w.Capabilities.CreateOwnKeys, tc.want)
			}
			if tc.name == "cap-own" && w.Capabilities.Admin {
				t.Errorf("%s: self-service alone must not make the caller an admin", tc.name)
			}
		}
		r := e.root("GET", "/whoami", nil)
		var w whoAmI
		r.json(t, &w)
		if w.Capabilities.CreateOwnKeys {
			t.Error("root: createOwnKeys must be false")
		}
		if !strings.Contains(string(r.body), `"createOwnKeys":false`) {
			t.Errorf("body %s", r.body)
		}
	})
}
