package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/restic"
)

// Secrets of the fake restic repositories: never in responses other than
// the credentials endpoint's, never in logs.
const (
	testResticPassword = "repo-password-0123456789"
	testResticSecret   = "aws-secret-9876543210"
)

// resticHostBackend is a memory backend that keeps (fake) restic
// repositories: one per bucket, existing unless missing is set.
type resticHostBackend struct {
	*memory.Backend
	missing bool  // repositories are not created yet
	err     error // ResticRepository fails with err
}

func (b *resticHostBackend) ResticRepository(ctx context.Context, bucket string, create bool) (restic.RepositoryInfo, restic.Location, error) {
	if _, err := b.GetBucket(ctx, bucket); err != nil {
		return restic.RepositoryInfo{}, restic.Location{}, err
	}
	switch {
	case b.err != nil:
		return restic.RepositoryInfo{}, restic.Location{}, b.err
	case b.missing && !create:
		return restic.RepositoryInfo{}, restic.Location{}, fmt.Errorf("fake: bucket %q has no restic repository yet: %w: %w", bucket, errors.ErrUnsupported, iofs.ErrNotExist)
	}
	return restic.RepositoryInfo{
		BucketID:       "bid-" + bucket,
		Repository:     "s3:https://s3.example.com/home/restic/bid-" + bucket,
		RecordLocation: "s3://home/keys/bid-" + bucket + ".restic.json",
		PasswordSource: "hex of the master key s3://home/keys/bid-" + bucket + ".key",
		Initialized:    true,
		EnvNames:       []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"},
		Options:        []string{"s3.bucket-lookup=path"},
	}, restic.Location{
		Repository: "s3:https://s3.example.com/home/restic/bid-" + bucket,
		Password:   testResticPassword,
		Env:        []string{"AWS_ACCESS_KEY_ID=AKIDEXAMPLE", "AWS_SECRET_ACCESS_KEY=" + testResticSecret},
		Options:    []string{"s3.bucket-lookup=path"},
	}, nil
}

func (b *resticHostBackend) ResticAuxRepository(context.Context, string, bool) (restic.RepositoryInfo, restic.Location, error) {
	return restic.RepositoryInfo{}, restic.Location{}, errors.ErrUnsupported
}

// newResticEnv returns an environment whose router has the backings
// "default" (keeping restic repositories) and "fast" (memory, keeping
// none), a bucket "photos" with a replica on fast, and a log buffer.
func newResticEnv(t *testing.T, opts ...envOption) (*testEnv, *resticHostBackend, *syncBuffer) {
	t.Helper()
	host := &resticHostBackend{Backend: memory.New()}
	r, err := multi.Open(context.Background(), multi.Options{
		Backings: []multi.Backing{
			{Name: "default", Kind: "s3", Backend: host},
			{Name: "fast", Kind: "memory", Backend: memory.New()},
		},
		HealthInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	logs := &syncBuffer{}
	opts = append([]envOption{func(d *Deps) {
		d.Backend = r
		d.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}}, opts...)
	e := newEnv(t, opts...)
	if err := r.CreateBucket(context.Background(), "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	e.root("PUT", "/buckets/photos/placement", map[string]any{"mode": "active-passive", "replicas": []string{"fast"}}).expectStatus(t, 200)
	return e, host, logs
}

func assertNoSecrets(t *testing.T, what string, data []byte) {
	t.Helper()
	for _, s := range []string{testResticPassword, testResticSecret} {
		if bytes.Contains(data, []byte(s)) {
			t.Fatalf("%s discloses a secret: %s", what, data)
		}
	}
}

func TestResticInfo(t *testing.T) {
	e, host, logs := newResticEnv(t)
	if err := e.api.d.Backend.PutBucketConfig(context.Background(), "photos", storage.ConfigEncryptionProvider,
		[]byte(`{"provider":"restic","scope":"all"}`)); err != nil {
		t.Fatal(err)
	}
	r := e.root("GET", "/buckets/photos/restic", nil)
	r.expectStatus(t, 200)
	assertNoSecrets(t, "GET /restic", r.body)
	var info resticInfo
	r.json(t, &info)
	if info.Settings != (storage.EncryptionSettings{Provider: "restic", Scope: "all"}) {
		t.Fatalf("settings = %+v", info.Settings)
	}
	if len(info.Archive) != 2 {
		t.Fatalf("archive = %+v", info.Archive)
	}
	p, rep := info.Archive[0], info.Archive[1]
	if p.Backing != "default" || p.Role != multi.RolePrimary || p.Error != "" || p.Info == nil ||
		p.Info.BucketID != "bid-photos" || !p.Info.Initialized || len(p.Info.EnvNames) != 2 || len(p.Info.Options) != 1 {
		t.Fatalf("primary = %+v (%+v)", p, p.Info)
	}
	if rep.Backing != "fast" || rep.Role != multi.RoleReplica || rep.Info != nil || !strings.HasPrefix(rep.Error, "no repository") {
		t.Fatalf("replica = %+v", rep)
	}
	want := resticTags{Object: "gos3-object", Bucket: restic.BucketTag("photos"), BucketID: "gos3-bucket-id:bid-photos", Snapshot: "gos3-snapshot"}
	if info.Tags != want {
		t.Fatalf("tags = %+v", info.Tags)
	}
	if !bytes.Contains(r.body, []byte(`"snapshots":null`)) || !bytes.Contains(r.body, []byte(`"snapshotRepository":null`)) {
		t.Fatalf("snapshot fields: %s", r.body)
	}

	// A repository that does not exist yet.
	host.missing = true
	info = e.resticInfoDoc("photos")
	if p := info.Archive[0]; p.Info != nil || !strings.HasPrefix(p.Error, "not created yet") || info.Tags.BucketID != "" {
		t.Fatalf("missing repository = %+v, tags %+v", p, info.Tags)
	}
	// Default settings when none are stored.
	if err := e.api.d.Backend.DeleteBucketConfig(context.Background(), "photos", storage.ConfigEncryptionProvider); err != nil {
		t.Fatal(err)
	}
	info = e.resticInfoDoc("photos")
	if info.Settings != (storage.EncryptionSettings{Provider: "AES256", Scope: "sse"}) {
		t.Fatalf("default settings = %+v", info.Settings)
	}
	e.root("GET", "/buckets/nope/restic", nil).expectError(t, 404, codeNotFound)
	assertNoSecrets(t, "the log", []byte(logs.String()))
}

// The info endpoint is GetBucketEncryption; the credentials need
// GetResticCredentials.
func TestResticCredentialsAuthorization(t *testing.T) {
	e, _, logs := newResticEnv(t)
	e.putPolicy("enc", map[string]any{"Effect": "Allow", "Action": "admin:GetBucketEncryption", "Resource": "arn:gos3:admin:::bucket/photos"})
	e.putPolicy("creds", map[string]any{"Effect": "Allow", "Action": "admin:GetResticCredentials", "Resource": "arn:gos3:admin:::bucket/photos"})
	viewer := e.addUser("viewer", "enc")
	revealer := e.addUser("revealer", "creds")

	e.do(&viewer, "GET", Prefix+"/buckets/photos/restic", nil).expectStatus(t, 200)
	r := e.do(&viewer, "POST", Prefix+"/buckets/photos/restic/credentials", map[string]any{})
	r.expectError(t, 403, codeAccessDenied)
	assertNoSecrets(t, "a refused request", r.body)
	if strings.Contains(logs.String(), "disclosed") {
		t.Fatalf("refused request logged as a disclosure: %s", logs.String())
	}

	var c resticCredentials
	r = e.do(&revealer, "POST", Prefix+"/buckets/photos/restic/credentials", map[string]any{})
	r.json(t, &c)
	if c.Password != testResticPassword || c.Env["AWS_SECRET_ACCESS_KEY"] != testResticSecret || c.Env["AWS_ACCESS_KEY_ID"] != "AKIDEXAMPLE" ||
		c.Backing != "default" || c.Purpose != "archive" || !strings.HasPrefix(c.Repository, "s3:https://") ||
		len(c.Options) != 1 {
		t.Fatalf("credentials = %+v", c)
	}
	log := logs.String()
	if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "credentials disclosed") ||
		!strings.Contains(log, `user \"revealer\"`) || !strings.Contains(log, "bucket=photos") ||
		!strings.Contains(log, "backing=default") || !strings.Contains(log, "purpose=archive") {
		t.Fatalf("disclosure not logged: %s", log)
	}
	assertNoSecrets(t, "the log", []byte(log))
	e.do(&revealer, "POST", Prefix+"/buckets/other/restic/credentials", map[string]any{}).expectError(t, 403, codeAccessDenied)
}

func TestResticCredentials(t *testing.T) {
	e, host, _ := newResticEnv(t)
	path := "/buckets/photos/restic/credentials"
	var c resticCredentials
	// An empty body asks for the primary's archive.
	e.root("POST", path, "").json(t, &c)
	if c.Backing != "default" || c.Password != testResticPassword {
		t.Fatalf("credentials = %+v", c)
	}
	e.root("POST", path, map[string]any{"backing": "default", "purpose": "archive"}).json(t, &c)
	if c.Backing != "default" {
		t.Fatalf("credentials = %+v", c)
	}
	for _, tc := range []struct {
		body           any
		status         int
		code, contains string
	}{
		{map[string]any{"backing": "fast"}, 404, codeNotFound, "no restic archive repository"},
		{map[string]any{"backing": "nope"}, 404, codeNotFound, "holds no copy"},
		{map[string]any{"purpose": "everything"}, 400, codeInvalidRequest, "purpose"},
		{map[string]any{"purpose": "snapshots"}, 404, codeNotFound, "not available"},
		{map[string]any{"purpose": "snapshots", "backing": "default"}, 400, codeInvalidRequest, "backing"},
		{map[string]any{"password": "x"}, 400, codeInvalidRequest, "password"},
	} {
		r := e.root("POST", path, tc.body)
		if msg := r.expectError(t, tc.status, tc.code); !strings.Contains(msg, tc.contains) {
			t.Errorf("%v: message %q", tc.body, msg)
		}
		assertNoSecrets(t, "an error", r.body)
	}
	e.root("POST", "/buckets/nope/restic/credentials", "").expectError(t, 404, codeNotFound)

	host.missing = true
	if msg := e.root("POST", path, "").expectError(t, 404, codeNotFound); !strings.Contains(msg, "first restic write") {
		t.Fatalf("missing repository: %q", msg)
	}
	// Unexpected failures do not carry their cause (it is logged).
	host.missing, host.err = false, errors.New("reading key with password "+testResticPassword)
	r := e.root("POST", path, "")
	r.expectError(t, 500, codeInternalError)
	assertNoSecrets(t, "a failure", r.body)
	host.err = fmt.Errorf("restic is not installed: %w", storage.ErrEncryptionUnavailable)
	e.root("POST", path, "").expectError(t, 503, codeInternalError)
}

// Whole-bucket snapshot repositories, through the snapshot service.
func TestResticInfoAndCredentialsWithSnapshots(t *testing.T) {
	snaps := newFakeSnapshots()
	e, _, logs := newResticEnv(t, func(d *Deps) { d.Snapshots = snaps })
	var info resticInfo
	info = e.resticInfoDoc("photos")
	if !isNull(info.Snapshots) || info.SnapshotRepository != nil || info.SnapshotsError != "" {
		t.Fatalf("without a schedule: %+v", info)
	}
	e.root("POST", "/buckets/photos/restic/credentials", map[string]any{"purpose": "snapshots"}).expectError(t, 404, codeNotFound)

	snaps.setConfig("photos", `{"enabled":true,"target":{"kind":"backing","backing":"fast","repo":"snap-1"}}`)
	r := e.root("GET", "/buckets/photos/restic", nil)
	assertNoSecrets(t, "GET /restic", r.body)
	r.json(t, &info)
	if !strings.Contains(string(info.Snapshots), `"snap-1"`) || info.SnapshotRepository == nil ||
		info.SnapshotRepository.Repository != snaps.repo.Repository {
		t.Fatalf("with a schedule: %+v", info)
	}
	var c resticCredentials
	e.root("POST", "/buckets/photos/restic/credentials", map[string]any{"purpose": "snapshots"}).json(t, &c)
	if c.Purpose != "snapshots" || c.Backing != "fast" || c.Password != testResticPassword || c.Env["RESTIC_CACHE_DIR"] != "/tmp/c" {
		t.Fatalf("snapshot credentials = %+v", c)
	}
	if !strings.Contains(logs.String(), "purpose=snapshots") {
		t.Fatalf("not logged: %s", logs.String())
	}
	assertNoSecrets(t, "the log", []byte(logs.String()))

	// A repository that cannot be described is reported, not fatal.
	snaps.mu.Lock()
	snaps.repoErr = fmt.Errorf("restic: %w", storage.ErrEncryptionUnavailable)
	snaps.mu.Unlock()
	info = e.resticInfoDoc("photos")
	if info.SnapshotsError == "" || info.SnapshotRepository != nil || isNull(info.Snapshots) {
		t.Fatalf("failing repository: %+v", info)
	}
}

func TestNoResticRepository(t *testing.T) {
	for err, want := range map[error]bool{
		errors.ErrUnsupported:                              true,
		fmt.Errorf("x: %w", iofs.ErrNotExist):              true,
		storage.ErrBucketNotFound:                          false,
		fmt.Errorf("x: %w", storage.ErrBackingUnavailable): false,
	} {
		if got := noResticRepository(err); got != want {
			t.Errorf("noResticRepository(%v) = %v", err, got)
		}
	}
}
