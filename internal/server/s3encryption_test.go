package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/restic/restictest"
)

// Encryption on s3 backings, end to end: AES256 needs the backing's
// key-encryption key (KEK); restic repositories live under the backing's
// prefix.

const sseDefaultAES256 = `<ServerSideEncryptionConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule>` +
	`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault>` +
	`</Rule></ServerSideEncryptionConfiguration>`

// encryptionDoc is the admin encryption-provider document.
type encryptionDoc struct {
	Provider  string   `json:"provider"`
	Available []string `json:"available"`
}

func getProvider(t *testing.T, url, bucket string) encryptionDoc {
	t.Helper()
	var doc encryptionDoc
	res := do(t, "GET", url+"/_gos3/admin/v1/buckets/"+bucket+"/encryption-provider", nil, &rootCreds, nil)
	mustStatus(t, "get encryption provider", res, 200)
	res.json(t, &doc)
	return doc
}

func putProvider(t *testing.T, url, bucket, provider string) response {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"provider": provider})
	return do(t, "PUT", url+"/_gos3/admin/v1/buckets/"+bucket+"/encryption-provider", raw, &rootCreds, nil)
}

// remoteObjects returns the keys and contents of the remote's objects
// under prefix in bucket.
func remoteObjects(t *testing.T, remote *s3clienttest.Remote, bucket, prefix string) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	out := map[string][]byte{}
	err := remote.Backend.WalkObjects(ctx, bucket, prefix, "", func(o storage.ObjectInfo) error {
		_, rc, err := remote.Backend.OpenObject(ctx, bucket, o.Key, "")
		if err != nil {
			return err
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		out[o.Key] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// incompressible returns n random bytes, so that neither compression nor
// a missing encryption can hide them in a remote blob.
func incompressible(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// AES256 on the s3 backing "cold": refused without a KEK; with one
// (s3.encryption_key) data is stored encrypted in the remote and survives
// a restart.
func TestS3BackingAES256(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "coldstore")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cold", Backend: config.BackendS3, S3: s3Settings(remote, "coldstore", "cold/")},
	}
	srv := start(t, cfg)
	mustStatus(t, "create", do(t, "PUT", srv.url+"/secret", nil, &rootCreds, map[string]string{"x-gos3-backing": "cold"}), 200)

	// Without a KEK nothing about AES256 is accepted.
	if doc := getProvider(t, srv.url, "secret"); len(doc.Available) != 0 {
		t.Fatalf("providers without a KEK: %+v", doc)
	}
	mustStatus(t, "AES256 provider without a KEK", putProvider(t, srv.url, "secret", storage.EncryptionAES256), 400)
	sse := map[string]string{"x-amz-server-side-encryption": "AES256"}
	mustStatus(t, "SSE put without a KEK", do(t, "PUT", srv.url+"/secret/sse", []byte("x"), &rootCreds, sse), 501)
	mustStatus(t, "default encryption without a KEK", do(t, "PUT", srv.url+"/secret?encryption", []byte(sseDefaultAES256), &rootCreds, nil), 501)
	mustStatus(t, "plain put", do(t, "PUT", srv.url+"/secret/plain", []byte("plain"), &rootCreds, nil), 200)
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}

	// With the KEK it works.
	cfg.Storage.Backings[0].S3.EncryptionKey = base64.StdEncoding.EncodeToString(incompressible(t, config.KEKSize))
	srv = start(t, cfg)
	if doc := getProvider(t, srv.url, "secret"); !slices.Contains(doc.Available, storage.EncryptionAES256) {
		t.Fatalf("providers with a KEK: %+v", doc)
	}
	mustStatus(t, "AES256 provider", putProvider(t, srv.url, "secret", storage.EncryptionAES256), 200)
	mustStatus(t, "default encryption", do(t, "PUT", srv.url+"/secret?encryption", []byte(sseDefaultAES256), &rootCreds, nil), 200)
	explicit := incompressible(t, 64<<10)
	res := do(t, "PUT", srv.url+"/secret/sse", explicit, &rootCreds, sse)
	mustStatus(t, "SSE put", res, 200)
	if res.header.Get("x-amz-server-side-encryption") != "AES256" {
		t.Fatalf("SSE put headers: %v", res.header)
	}
	byDefault := incompressible(t, 64<<10)
	mustStatus(t, "put with default encryption", do(t, "PUT", srv.url+"/secret/default", byDefault, &rootCreds, nil), 200)
	res = do(t, "HEAD", srv.url+"/secret/default", nil, &rootCreds, nil)
	if res.status != 200 || res.header.Get("x-amz-server-side-encryption") != "AES256" {
		t.Fatalf("HEAD default: %d %v", res.status, res.header)
	}
	getBody(t, srv.url+"/secret/sse", string(explicit))
	getBody(t, srv.url+"/secret/default", string(byDefault))
	getBody(t, srv.url+"/secret/plain", "plain")

	// The remote holds ciphertext only.
	blobs := remoteObjects(t, remote, "coldstore", "cold/")
	if len(blobs) == 0 {
		t.Fatal("no remote objects")
	}
	for key, data := range blobs {
		for _, p := range [][]byte{explicit[:64], explicit[len(explicit)-64:], byDefault[:64], byDefault[len(byDefault)-64:]} {
			if bytes.Contains(data, p) {
				t.Fatalf("remote object %s holds plaintext", key)
			}
		}
	}

	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	srv = start(t, cfg)
	getBody(t, srv.url+"/secret/sse", string(explicit))
	getBody(t, srv.url+"/secret/default", string(byDefault))
	if doc := getProvider(t, srv.url, "secret"); doc.Provider != storage.EncryptionAES256 {
		t.Fatalf("provider after restart: %+v", doc)
	}
}

// restic on an s3 backing: the bucket's repository lives under the
// backing's prefix, restic/<bucket-id>/ (fake restic with s3: repos).
func TestS3BackingRestic(t *testing.T) {
	log := restictest.Install(t, "s3backing")
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "coldstore")
	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cold := s3Settings(remote, "coldstore", "cold/")
	cold.EncryptionKey = base64.StdEncoding.EncodeToString(incompressible(t, config.KEKSize))
	cfg.Storage.Backings = []config.BackingConfig{{Name: "cold", Backend: config.BackendS3, S3: cold}}
	srv := start(t, cfg)
	mustStatus(t, "create", do(t, "PUT", srv.url+"/vault", nil, &rootCreds, map[string]string{"x-gos3-backing": "cold"}), 200)
	if doc := getProvider(t, srv.url, "vault"); !slices.Contains(doc.Available, storage.EncryptionRestic) {
		t.Fatalf("providers: %+v", doc)
	}
	mustStatus(t, "restic provider", putProvider(t, srv.url, "vault", storage.EncryptionRestic), 200)
	data := incompressible(t, 32<<10)
	sse := map[string]string{"x-amz-server-side-encryption": "AES256"}
	mustStatus(t, "SSE put", do(t, "PUT", srv.url+"/vault/archived", data, &rootCreds, sse), 200)
	getBody(t, srv.url+"/vault/archived", string(data))
	if restictest.Calls(t, log, "backup") == 0 {
		t.Fatal("restic backup did not run")
	}

	repo := remoteObjects(t, remote, "coldstore", "cold/restic/")
	ids := map[string]bool{}
	for key := range repo {
		id, _, ok := strings.Cut(strings.TrimPrefix(key, "cold/restic/"), "/")
		if !ok || id == "" {
			t.Fatalf("unexpected repository object %q", key)
		}
		ids[id] = true
	}
	if len(ids) != 1 {
		t.Fatalf("repository objects under cold/restic/: %v", keysOf(repo))
	}
	for id := range ids {
		if _, ok := repo["cold/restic/"+id+"/config"]; !ok {
			t.Fatalf("no repository config under cold/restic/%s/: %v", id, keysOf(repo))
		}
	}

	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	srv = start(t, cfg)
	getBody(t, srv.url+"/vault/archived", string(data))
}
