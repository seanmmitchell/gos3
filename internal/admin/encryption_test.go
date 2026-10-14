package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

type encryptionTestBackend struct {
	storage.Backend
	validationError error
	prepareError    error
}

func (b *encryptionTestBackend) ValidateEncryptionSettings(context.Context, string, storage.EncryptionSettings) error {
	return b.validationError
}
func (b *encryptionTestBackend) PrepareEncryption(context.Context, string, string) error {
	return b.prepareError
}

func TestBucketEncryptionProvider(t *testing.T) {
	e := newEnv(t)
	e.createBucket("photos", storage.CreateBucketOptions{})
	b := &encryptionTestBackend{Backend: e.backend}
	e.api.d.Backend = b
	path := "/buckets/photos/encryption-provider"
	body := map[string]any{"provider": "restic", "repository": "s3:https://s3.example.test/backups/photos", "accessKey": "test-access-key", "secretKey": "test-secret-key", "password": "test-password", "sessionToken": "test-token"}
	r := e.root("PUT", path, body)
	r.expectStatus(t, 200)
	for _, method := range []string{"PUT", "GET"} {
		if method == "GET" {
			r = e.root(method, path, nil)
		}
		for _, secret := range []string{"test-access-key", "test-secret-key", "test-password", "test-token"} {
			if bytes.Contains(r.body, []byte(secret)) {
				t.Fatalf("%s leaked a credential", method)
			}
		}
	}
	stored, err := e.backend.GetBucketConfig(context.Background(), "photos", storage.ConfigEncryptionProvider)
	if err != nil {
		t.Fatal(err)
	}
	var cfg storage.EncryptionSettings
	if err = json.Unmarshal(stored, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SecretKey != "test-secret-key" || cfg.Password != "test-password" {
		t.Fatal("credentials not persisted")
	}
	// Failed validation leaves working credentials untouched.
	b.validationError = errors.New("wrong password")
	body["password"] = "incorrect"
	e.root("PUT", path, body).expectStatus(t, 400)
	again, _ := e.backend.GetBucketConfig(context.Background(), "photos", storage.ConfigEncryptionProvider)
	if !bytes.Equal(stored, again) {
		t.Fatal("failed validation replaced working credentials")
	}
	b.validationError = nil
	// Switching to native preserves the archive's credentials and location.
	e.root("PUT", path, map[string]any{"provider": "AES256"}).expectStatus(t, 200)
	stored, _ = e.backend.GetBucketConfig(context.Background(), "photos", storage.ConfigEncryptionProvider)
	_ = json.Unmarshal(stored, &cfg)
	if cfg.Password != "test-password" || cfg.Repository != "s3:https://s3.example.test/backups/photos" {
		t.Fatal("switch lost archive access")
	}
	e.root("PUT", path, map[string]any{"provider": "restic", "repository": "s3:https://s3.example.test/new/path"}).expectStatus(t, 400)
	// An uncertain initialization result preserves the generated password.
	e.createBucket("new-bucket", storage.CreateBucketOptions{})
	b.prepareError = errors.New("initialization timed out")
	r = e.root("PUT", "/buckets/new-bucket/encryption-provider", map[string]any{"provider": "restic"})
	r.expectStatus(t, 200)
	var doc encryptionDocument
	r.json(t, &doc)
	if doc.Ready == nil || *doc.Ready {
		t.Fatal("failed init reported ready")
	}
	stored, _ = e.backend.GetBucketConfig(context.Background(), "new-bucket", storage.ConfigEncryptionProvider)
	_ = json.Unmarshal(stored, &cfg)
	if cfg.Password == "" {
		t.Fatal("uncertain init discarded password")
	}
}

func TestBucketEncryptionProviderScoped(t *testing.T) {
	e := newEnv(t)
	e.createBucket("team-a", storage.CreateBucketOptions{})
	e.createBucket("other", storage.CreateBucketOptions{})
	e.putPolicy("encryption", map[string]any{"Effect": "Allow", "Action": []string{"admin:GetBucketEncryption", "admin:PutBucketEncryption"}, "Resource": "arn:gos3:admin:::bucket/team-*"})
	alice := e.addUser("alice", "encryption")
	e.do(&alice, "PUT", Prefix+"/buckets/team-a/encryption-provider", map[string]any{"provider": "AES256"}).expectStatus(t, 200)
	e.do(&alice, "GET", Prefix+"/buckets/team-a/encryption-provider", nil).expectStatus(t, 200)
	e.do(&alice, "GET", Prefix+"/buckets/other/encryption-provider", nil).expectError(t, 403, codeAccessDenied)
	e.do(&alice, "PUT", Prefix+"/buckets/other/encryption-provider", map[string]any{"provider": "AES256"}).expectError(t, 403, codeAccessDenied)
}
