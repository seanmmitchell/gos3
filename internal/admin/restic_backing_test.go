package admin

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	storagefs "github.com/seanmmitchell/gos3/internal/storage/fs"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

func TestResticBackingAPI(t *testing.T) {
	cfg := &storage.ResticS3{Endpoint: "https://s3.example.test", Bucket: "archives", Prefix: "backing", AccessKey: "backing-access", SecretKey: "backing-secret", SessionToken: "backing-token"}
	b, err := storagefs.Open(t.TempDir(), storagefs.Options{ResticS3: cfg})
	if err != nil {
		t.Fatal(err)
	}
	r, err := multi.Open(context.Background(), multi.Options{Backings: []multi.Backing{{Name: "default", Kind: "fs", Backend: b}}, HealthInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	e := newEnv(t, func(d *Deps) { d.Backend = r })
	path := "/backings/default/restic"
	response := e.root("GET", path, nil)
	response.expectStatus(t, 200)
	for _, secret := range []string{"backing-access", "backing-secret", "backing-token"} {
		if bytes.Contains(response.body, []byte(secret)) {
			t.Fatal("backing API leaked a credential")
		}
	}
	var doc resticBackingResponse
	response.json(t, &doc)
	if doc.Mode != "s3" || !doc.CredentialsConfigured || !doc.SessionTokenConfigured {
		t.Fatal("wrong backing response")
	}
	e.root("PUT", path, map[string]any{"mode": "local", "secretKey": "unexpected"}).expectStatus(t, 400)
	// Strict decoding names unknown fields; endpoint checks match
	// config.ValidateBaseURL's and are reported for the endpoint.
	if msg := e.root("PUT", path, map[string]any{"mode": "s3", "repository": "x"}).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "repository") {
		t.Fatalf("unknown field: %q", msg)
	}
	for _, endpoint := range []string{"https://:443", "https://s3.example.test:99999", "https://s3.example.test:0", "https://s3.example.test:", "https://s3.example.test/?", "https://s3.example.test/path"} {
		if msg := e.root("PUT", path, map[string]any{"mode": "s3", "endpoint": endpoint, "bucket": "archives"}).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "endpoint") {
			t.Fatalf("%s: %q", endpoint, msg)
		}
	}
	if msg := e.root("PUT", path, map[string]any{"mode": "s3", "endpoint": "https://s3.example.test", "bucket": "archives", "region": "US East"}).expectError(t, 400, codeInvalidRequest); !strings.Contains(msg, "region") {
		t.Fatalf("region: %q", msg)
	}
	e.root("PUT", path, map[string]any{"mode": "local"}).expectStatus(t, 200)
	// Listing permission allows inspecting the redacted backing, while
	// changing endpoint/credentials requires server configuration permission.
	e.putPolicy("backing-view", map[string]any{"Effect": "Allow", "Action": "admin:ListBackings", "Resource": "*"})
	viewer := e.addUser("viewer", "backing-view")
	e.do(&viewer, "GET", Prefix+path, nil).expectStatus(t, 200)
	e.do(&viewer, "PUT", Prefix+path, map[string]any{"mode": "local"}).expectError(t, 403, codeAccessDenied)
	e.root("GET", "/backings/unknown/restic", nil).expectStatus(t, 404)
}
