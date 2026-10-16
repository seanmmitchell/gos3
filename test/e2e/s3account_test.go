package e2e

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
)

// s3 backings against an in-process S3 service (s3clienttest, gos3's S3
// handler over memory storage) driven with aws-sdk-go-v2: native buckets
// pass versions, delete markers, versioning, object lock and governance
// bypasses through to their remote buckets; wrapped buckets live in the
// backing's containers.

// s3Backing returns the configuration of an s3 backing on remote with its
// home at homeBucket/homePrefix ("" = no home: records in the system
// directory).
func s3Backing(remote *s3clienttest.Remote, name, homeBucket, homePrefix string) config.BackingConfig {
	return config.BackingConfig{Name: name, Backend: config.BackendS3, S3: &config.S3Config{
		Endpoint: remote.URL, HomeBucket: homeBucket, HomePrefix: homePrefix, Addressing: "path",
		AccessKey: s3clienttest.AccessKey, SecretKey: s3clienttest.SecretKey,
	}}
}

// remoteVersions returns "<key>:<version ID>[:dm]" for the versions of key
// on the remote bucket, newest first.
func remoteVersions(t *testing.T, rc *s3client.Client, bucket, key string) []string {
	t.Helper()
	out, err := rc.ListObjectVersions(context.Background(), bucket, s3client.VersionListInput{Prefix: key})
	if err != nil {
		t.Fatalf("remote ListObjectVersions %s: %v", bucket, err)
	}
	var list []string
	for _, e := range out.Entries {
		if e.Key != key {
			continue
		}
		s := e.Key + ":" + e.VersionID
		if e.DeleteMarker {
			s += ":dm"
		}
		list = append(list, s)
	}
	return list
}

// gos3Versions is remoteVersions through gos3 (ListObjectVersions of the SDK).
func gos3Versions(t *testing.T, c *s3.Client, bucket, key string) []string {
	t.Helper()
	out, err := c.ListObjectVersions(ctx(t), &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(key)})
	must(t, err, "ListObjectVersions")
	type entry struct {
		s    string
		time time.Time
		at   int
	}
	// The SDK splits versions and delete markers; merge them back by
	// LastModified, newest first (ties keep versions before markers as
	// listed).
	var list []entry
	for i, v := range out.Versions {
		if aws.ToString(v.Key) == key {
			list = append(list, entry{key + ":" + aws.ToString(v.VersionId), aws.ToTime(v.LastModified), i})
		}
	}
	for i, m := range out.DeleteMarkers {
		if aws.ToString(m.Key) == key {
			list = append(list, entry{key + ":" + aws.ToString(m.VersionId) + ":dm", aws.ToTime(m.LastModified), i})
		}
	}
	slices.SortStableFunc(list, func(a, b entry) int { return b.time.Compare(a.time) })
	out2 := make([]string, len(list))
	for i, e := range list {
		out2[i] = e.s
	}
	return out2
}

// TestS3NativePassthrough adopts a versioned remote bucket under another
// name and checks that gos3 serves and changes the remote's own history,
// versioning, object lock and retention.
func TestS3NativePassthrough(t *testing.T) {
	remote := s3clienttest.Start(t)
	rc := remote.Client(t)
	bg := context.Background()
	// A versioned bucket written by another tool.
	must(t, rc.CreateBucket(bg, "remote-docs"), "remote CreateBucket")
	must(t, rc.PutBucketVersioning(bg, "remote-docs", "Enabled"), "remote PutBucketVersioning")
	var ids []string
	for _, body := range []string{"one", "two"} {
		w, err := rc.Put(bg, "remote-docs", "doc", strings.NewReader(body), int64(len(body)), s3client.PutOptions{})
		must(t, err, "remote Put")
		ids = append(ids, w.VersionID)
	}

	cfg := baseConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = filepath.Join(t.TempDir(), "data")
	cfg.Storage.FsyncData = false
	cfg.Storage.Backings = []config.BackingConfig{s3Backing(remote, "edge", "", "")}
	e := startPrivate(t, cfg)
	c := e.root()

	// Adopt it as "docs".
	if r := e.admin(t, rootCreds, http.MethodPost, "/backings/edge/buckets",
		map[string]any{"bucket": "docs", "remoteBucket": "remote-docs", "adopt": true}, nil); r.status != http.StatusCreated {
		t.Fatalf("adopt: %d %s", r.status, r.body)
	}
	if r := e.signed(t, rootCreds, http.MethodHead, "/docs", nil, nil); r.status != 200 ||
		r.header.Get("x-gos3-backing") != "edge" || r.header.Get("x-gos3-bucket-kind") != "native" {
		t.Fatalf("HEAD docs: %d %v", r.status, r.header)
	}
	v, err := c.GetBucketVersioning(ctx(t), &s3.GetBucketVersioningInput{Bucket: aws.String("docs")})
	must(t, err, "GetBucketVersioning")
	if v.Status != s3types.BucketVersioningStatusEnabled {
		t.Fatalf("versioning of the adopted bucket: %q", v.Status)
	}

	// ListObjectVersions shows the remote's version IDs; GET by version ID.
	want := []string{"doc:" + ids[1], "doc:" + ids[0]}
	if got := gos3Versions(t, c, "docs", "doc"); !slices.Equal(got, want) {
		t.Fatalf("versions through gos3 %v, want %v", got, want)
	}
	for i, body := range []string{"one", "two"} {
		o, err := c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String("docs"), Key: aws.String("doc"), VersionId: aws.String(ids[i])})
		must(t, err, "GetObject by version")
		if got := string(readAll(t, o.Body)); got != body || aws.ToString(o.VersionId) != ids[i] {
			t.Fatalf("version %s: %q (version %q)", ids[i], got, aws.ToString(o.VersionId))
		}
	}

	// A delete through gos3 leaves a delete marker on the remote.
	d, err := c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String("docs"), Key: aws.String("doc")})
	must(t, err, "DeleteObject")
	if !aws.ToBool(d.DeleteMarker) || aws.ToString(d.VersionId) == "" {
		t.Fatalf("delete: marker %v version %q", aws.ToBool(d.DeleteMarker), aws.ToString(d.VersionId))
	}
	want = append([]string{"doc:" + aws.ToString(d.VersionId) + ":dm"}, want...)
	if got := remoteVersions(t, rc, "remote-docs", "doc"); !slices.Equal(got, want) {
		t.Fatalf("remote versions after the delete %v, want %v", got, want)
	}
	_, err = c.GetObject(ctx(t), &s3.GetObjectInput{Bucket: aws.String("docs"), Key: aws.String("doc")})
	wantErr(t, err, "NoSuchKey", http.StatusNotFound)
	// Removing the marker brings the object back, on the remote too.
	_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String("docs"), Key: aws.String("doc"), VersionId: d.VersionId})
	must(t, err, "delete the delete marker")
	if got := string(e.get(t, "docs", "doc")); got != "two" {
		t.Fatalf("doc after removing the marker: %q", got)
	}
	if got := remoteVersions(t, rc, "remote-docs", "doc"); !slices.Equal(got, want[1:]) {
		t.Fatalf("remote versions after removing the marker %v, want %v", got, want[1:])
	}

	// A new native bucket: versioning and object lock enabled through gos3
	// are the remote bucket's.
	if r := e.admin(t, rootCreds, http.MethodPost, "/backings/edge/buckets",
		map[string]any{"bucket": "vault", "kind": "native", "remoteBucket": "vault-remote"}, nil); r.status != http.StatusCreated {
		t.Fatalf("create vault: %d %s", r.status, r.body)
	}
	if st, err := rc.GetBucketVersioning(bg, "vault-remote"); err != nil || st != "" {
		t.Fatalf("remote versioning before: %q %v", st, err)
	}
	e.setVersioning(t, "vault", "Enabled")
	if st, err := rc.GetBucketVersioning(bg, "vault-remote"); err != nil || st != "Enabled" {
		t.Fatalf("remote versioning after: %q %v", st, err)
	}
	_, err = c.PutObjectLockConfiguration(ctx(t), &s3.PutObjectLockConfigurationInput{
		Bucket: aws.String("vault"),
		ObjectLockConfiguration: &s3types.ObjectLockConfiguration{
			ObjectLockEnabled: s3types.ObjectLockEnabledEnabled,
			Rule: &s3types.ObjectLockRule{DefaultRetention: &s3types.DefaultRetention{
				Mode: s3types.ObjectLockRetentionModeGovernance, Days: aws.Int32(1),
			}},
		},
	})
	must(t, err, "PutObjectLockConfiguration")
	lock, found, err := rc.GetObjectLockConfiguration(bg, "vault-remote")
	if err != nil || !found || !lock.Enabled || lock.Mode != "GOVERNANCE" || lock.Days != 1 {
		t.Fatalf("remote object lock: %+v %v %v", lock, found, err)
	}

	// GOVERNANCE retention: a delete without the bypass is refused; with
	// it (and s3:BypassGovernanceRetention) the version is deleted on the
	// remote, which also enforces the retention: it saw the bypass.
	put := e.put(t, "vault", "held", []byte("keep me"))
	vid := aws.ToString(put.VersionId)
	// Beyond the default retention (1 day): extending needs no bypass.
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	_, err = c.PutObjectRetention(ctx(t), &s3.PutObjectRetentionInput{
		Bucket: aws.String("vault"), Key: aws.String("held"), VersionId: aws.String(vid),
		Retention: &s3types.ObjectLockRetention{Mode: s3types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(until)},
	})
	must(t, err, "PutObjectRetention")
	ret, err := rc.GetObjectRetention(bg, "vault-remote", "held", vid)
	if err != nil || ret.Mode != "GOVERNANCE" || ret.RetainUntil.Before(until) {
		t.Fatalf("remote retention: %+v %v", ret, err)
	}
	_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String("vault"), Key: aws.String("held"), VersionId: aws.String(vid)})
	wantErr(t, err, "AccessDenied", http.StatusForbidden)
	pname := userName("vault-nobypass")
	e.putPolicy(t, pname, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject*","s3:PutObject*","s3:DeleteObject*","s3:ListBucket*"],"Resource":["arn:aws:s3:::vault","arn:aws:s3:::vault/*"]}]}`)
	user := e.s3Client(e.createUser(t, userName("vault-user"), pname))
	_, err = user.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String("vault"), Key: aws.String("held"), VersionId: aws.String(vid),
		BypassGovernanceRetention: aws.Bool(true)})
	wantErr(t, err, "AccessDenied", http.StatusForbidden)
	if got := remoteVersions(t, rc, "vault-remote", "held"); !slices.Equal(got, []string{"held:" + vid}) {
		t.Fatalf("remote versions after refused deletes: %v", got)
	}
	_, err = c.DeleteObject(ctx(t), &s3.DeleteObjectInput{Bucket: aws.String("vault"), Key: aws.String("held"), VersionId: aws.String(vid),
		BypassGovernanceRetention: aws.Bool(true)})
	must(t, err, "DeleteObject with the bypass")
	if got := remoteVersions(t, rc, "vault-remote", "held"); len(got) != 0 {
		t.Fatalf("remote versions after the bypass: %v", got)
	}
}

// TestS3WrappedContainers creates wrapped buckets in the three kinds of
// container of an s3 backing (its home, a dedicated remote bucket, an
// existing remote bucket under a prefix) and checks they survive a
// restart with their versions.
func TestS3WrappedContainers(t *testing.T) {
	remote := s3clienttest.Start(t)
	rc := remote.Client(t)
	bg := context.Background()
	remote.CreateBucket(t, "cloudhome")
	must(t, rc.CreateBucket(bg, "spare"), "remote CreateBucket")
	_, err := rc.Put(bg, "spare", "unrelated", strings.NewReader("other data"), int64(len("other data")), s3client.PutOptions{})
	must(t, err, "remote Put")

	cfg := baseConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = filepath.Join(t.TempDir(), "data")
	cfg.Storage.FsyncData = false
	cfg.Storage.Backings = []config.BackingConfig{s3Backing(remote, "cloud", "cloudhome", "gos3/")}
	e := startPrivate(t, cfg)

	for _, req := range []map[string]any{
		{"bucket": "w-home", "kind": "wrapped"},
		{"bucket": "w-dedicated", "newContainer": map[string]any{"bucket": "cloud-dedicated", "createBucket": true}},
		{"bucket": "w-existing", "newContainer": map[string]any{"bucket": "spare", "prefix": "wrapped/"}},
	} {
		if r := e.admin(t, rootCreds, http.MethodPost, "/backings/cloud/buckets", req, nil); r.status != http.StatusCreated {
			t.Fatalf("create %v: %d %s", req["bucket"], r.status, r.body)
		}
	}
	buckets := []string{"w-home", "w-dedicated", "w-existing"}
	versions := map[string][]string{}
	for _, b := range buckets {
		e.setVersioning(t, b, "Enabled")
		for i := range 3 {
			out := e.put(t, b, "doc", []byte(b+" v"+string(rune('0'+i))))
			versions[b] = append([]string{"doc:" + aws.ToString(out.VersionId)}, versions[b]...)
		}
	}
	// The containers' stores are where they were asked to be.
	for bucket, key := range map[string]string{"cloudhome": "gos3/gos3.json", "cloud-dedicated": "gos3.json", "spare": "wrapped/gos3.json"} {
		if _, err := rc.HeadObject(bg, bucket, key); err != nil {
			t.Fatalf("marker %s/%s: %v", bucket, key, err)
		}
	}
	if _, err := rc.HeadObject(bg, "spare", "unrelated"); err != nil {
		t.Fatalf("spare's own data: %v", err)
	}

	check := func(e *env) {
		t.Helper()
		c := e.root()
		for _, b := range buckets {
			if r := e.signed(t, rootCreds, http.MethodHead, "/"+b, nil, nil); r.status != 200 ||
				r.header.Get("x-gos3-backing") != "cloud" || r.header.Get("x-gos3-bucket-kind") != "wrapped" {
				t.Fatalf("HEAD %s: %d %v", b, r.status, r.header)
			}
			if got := gos3Versions(t, c, b, "doc"); !slices.Equal(got, versions[b]) {
				t.Fatalf("versions of %s: %v, want %v", b, got, versions[b])
			}
			if got := string(e.get(t, b, "doc")); got != b+" v2" {
				t.Fatalf("%s/doc: %q", b, got)
			}
		}
	}
	check(e)
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
	e = startPrivate(t, cfg)
	check(e)
	var containers struct {
		Containers []struct {
			ID      string `json:"id"`
			Role    string `json:"role"`
			Buckets int    `json:"buckets"`
		} `json:"containers"`
	}
	if r := e.admin(t, rootCreds, http.MethodGet, "/backings/cloud/containers", nil, &containers); r.status != 200 || len(containers.Containers) != 3 {
		t.Fatalf("containers after restart: %d %s", r.status, r.body)
	}
	for _, ci := range containers.Containers {
		if ci.Buckets != 1 {
			t.Fatalf("containers after restart: %+v", containers.Containers)
		}
	}
}
