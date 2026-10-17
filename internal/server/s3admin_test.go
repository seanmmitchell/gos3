package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage/s3account"
)

// remotePut writes key directly on the remote, returning its version ID.
func remotePut(t *testing.T, c *s3client.Client, bucket, key, body string) string {
	t.Helper()
	w, err := c.Put(context.Background(), bucket, key, strings.NewReader(body), int64(len(body)), s3client.PutOptions{})
	if err != nil {
		t.Fatalf("remote put %s/%s: %v", bucket, key, err)
	}
	return w.VersionID
}

// versionList returns "<key>:<version ID>[:dm]" for every entry of the
// ListObjectVersions answer of c (one page) for bucket, in order.
func versionList(t *testing.T, c *s3client.Client, bucket, prefix string) []string {
	t.Helper()
	out, err := c.ListObjectVersions(context.Background(), bucket, s3client.VersionListInput{Prefix: prefix})
	if err != nil {
		t.Fatalf("ListObjectVersions %s: %v", bucket, err)
	}
	var list []string
	for _, e := range out.Entries {
		s := e.Key + ":" + e.VersionID
		if e.DeleteMarker {
			s += ":dm"
		}
		list = append(list, s)
	}
	return list
}

// remoteBucketsOf returns GET /backings/<backing>/remote-buckets by name.
func remoteBucketsOf(t *testing.T, url, backing string) (s3account.RemoteBucketList, map[string]s3account.RemoteBucket) {
	t.Helper()
	var list s3account.RemoteBucketList
	res := adminDo(t, url, "GET", "/backings/"+backing+"/remote-buckets", nil)
	mustStatus(t, "list remote buckets of "+backing, res, 200)
	res.json(t, &list)
	m := map[string]s3account.RemoteBucket{}
	for _, b := range list.Buckets {
		m[b.Name] = b
	}
	return list, m
}

// (c) and (e): the remote bucket browser and bucket creation of the admin
// API on the s3 backings "cloud" (a home) and "edge" (no home): adopting a
// versioned remote bucket under another name, native and wrapped buckets in
// every kind of container, release; everything survives a restart.
func TestS3RemoteBucketsAdmin(t *testing.T) {
	remote := s3clienttest.Start(t)
	remote.CreateBucket(t, "cloudhome")
	ctx := context.Background()
	rc := remote.Client(t)
	// A versioned remote bucket with history, written by another tool.
	if err := rc.CreateBucket(ctx, "existing"); err != nil {
		t.Fatal(err)
	}
	if err := rc.PutBucketVersioning(ctx, "existing", "Enabled"); err != nil {
		t.Fatal(err)
	}
	v1 := remotePut(t, rc, "existing", "doc", "one")
	v2 := remotePut(t, rc, "existing", "doc", "two")
	remotePut(t, rc, "existing", "dir/obj", "deep")
	if v1 == "" || v2 == "" || v1 == v2 {
		t.Fatalf("remote version IDs %q %q", v1, v2)
	}
	// A remote bucket holding other data, where a container goes under a
	// prefix.
	if err := rc.CreateBucket(ctx, "spare"); err != nil {
		t.Fatal(err)
	}
	remotePut(t, rc, "spare", "unrelated.txt", "not gos3's")

	cfg := testConfig()
	cfg.Storage.Backend = config.BackendFS
	cfg.Storage.Path = t.TempDir()
	cfg.Storage.Backings = []config.BackingConfig{
		{Name: "cloud", Backend: config.BackendS3, S3: s3Settings(remote, "cloudhome", "gos3/")},
		{Name: "edge", Backend: config.BackendS3, S3: s3Settings(remote, "", "")},
	}
	srv := start(t, cfg)

	// The browser lists the remote buckets with their uses.
	list, byName := remoteBucketsOf(t, srv.url, "cloud")
	if !list.Complete {
		t.Fatalf("remote bucket list incomplete: %+v", list)
	}
	for name, want := range map[string]string{"cloudhome": "home", "existing": "unmanaged", "spare": "unmanaged"} {
		if got := byName[name].Status; got != want {
			t.Fatalf("remote bucket %s: status %q, want %q (%+v)", name, got, want, list)
		}
	}
	mustStatus(t, "unknown backing", adminDo(t, srv.url, "GET", "/backings/nope/remote-buckets", nil), 404)
	mustStatus(t, "not an s3 backing", adminDo(t, srv.url, "GET", "/backings/default/remote-buckets", nil), 400)

	// Adopt "existing" on edge as "alias".
	res := adminDo(t, srv.url, "POST", "/backings/edge/buckets", map[string]any{"bucket": "alias", "remoteBucket": "existing", "adopt": true})
	mustStatus(t, "adopt existing", res, 201)
	headBucket(t, srv.url, "alias", "edge", "native")
	_, byName = remoteBucketsOf(t, srv.url, "edge")
	if b := byName["existing"]; b.Status != "native" || len(b.Uses) == 0 || b.Uses[0].Bucket != "alias" || b.Uses[0].Created {
		t.Fatalf("existing after adoption: %+v", b)
	}
	_, byName = remoteBucketsOf(t, srv.url, "cloud")
	if b := byName["existing"]; b.Status == "unmanaged" {
		t.Fatalf("existing seen from cloud after adoption: %+v", b)
	}
	// Adopting it twice is a conflict; adopting a missing bucket fails.
	mustStatus(t, "adopt again", adminDo(t, srv.url, "POST", "/backings/edge/buckets",
		map[string]any{"bucket": "alias2", "remoteBucket": "existing", "adopt": true}), 409)
	mustStatusIn(t, "adopt a missing bucket", adminDo(t, srv.url, "POST", "/backings/edge/buckets",
		map[string]any{"bucket": "ghost", "remoteBucket": "no-such-bucket", "adopt": true}), 400, 404)

	// Its objects and versions are served as they are on the remote.
	g := gos3Client(t, srv.url)
	getBody(t, srv.url+"/alias/doc", "two")
	getBody(t, srv.url+"/alias/dir/obj", "deep")
	getBody(t, srv.url+"/alias/doc?versionId="+v1, "one")
	if got, want := versionList(t, g, "alias", "doc"), versionList(t, rc, "existing", "doc"); !slices.Equal(got, want) ||
		!slices.Equal(got, []string{"doc:" + v2, "doc:" + v1}) {
		t.Fatalf("versions through gos3 %v, on the remote %v", got, want)
	}
	// A write through gos3 is a new remote version.
	mustStatus(t, "put through gos3", do(t, "PUT", srv.url+"/alias/doc", []byte("three"), &rootCreds, nil), 200)
	if got := versionList(t, rc, "existing", "doc"); len(got) != 3 {
		t.Fatalf("remote versions after a write: %v", got)
	}

	// A native bucket with a new remote bucket of its name.
	res = adminDo(t, srv.url, "POST", "/backings/edge/buckets", map[string]any{"bucket": "fresh", "kind": "native"})
	mustStatus(t, "create native", res, 201)
	if !remoteHas(remote, "fresh") {
		t.Fatal("no remote bucket for fresh")
	}
	_, byName = remoteBucketsOf(t, srv.url, "edge")
	if b := byName["fresh"]; b.Status != "native" || len(b.Uses) == 0 || !b.Uses[0].Created {
		t.Fatalf("fresh: %+v", b)
	}
	// A wrapped bucket in a new dedicated container (a remote bucket gos3
	// creates), and one in an existing remote bucket under a prefix.
	res = adminDo(t, srv.url, "POST", "/backings/cloud/buckets", map[string]any{
		"bucket": "boxed", "kind": "wrapped", "newContainer": map[string]any{"bucket": "cloudbox", "createBucket": true, "label": "box"},
	})
	mustStatus(t, "create wrapped in a dedicated container", res, 201)
	res = adminDo(t, srv.url, "POST", "/backings/cloud/buckets", map[string]any{
		"bucket": "shelved", "newContainer": map[string]any{"bucket": "spare", "prefix": "gos3"},
	})
	mustStatus(t, "create wrapped under a prefix of an existing bucket", res, 201)
	// A wrapped bucket on edge, which has no home: in a container of its own.
	res = adminDo(t, srv.url, "POST", "/backings/edge/buckets", map[string]any{
		"bucket": "edgebox", "kind": "wrapped", "newContainer": map[string]any{"bucket": "edgebox-remote", "createBucket": true},
	})
	mustStatus(t, "create wrapped on edge", res, 201)
	for bucket, kind := range map[string]string{"boxed": "wrapped", "shelved": "wrapped", "fresh": "native", "edgebox": "wrapped"} {
		backing := "cloud"
		if bucket == "fresh" || bucket == "edgebox" {
			backing = "edge"
		}
		headBucket(t, srv.url, bucket, backing, kind)
		mustStatus(t, "put", do(t, "PUT", srv.url+"/"+bucket+"/k", []byte("in "+bucket), &rootCreds, nil), 200)
	}
	if !slices.Equal(remoteKeys(t, remote, "fresh"), []string{"k"}) {
		t.Fatalf("fresh's remote objects: %v", remoteKeys(t, remote, "fresh"))
	}
	if keys := remoteKeys(t, remote, "spare"); !slices.Contains(keys, "unrelated.txt") || !slices.Contains(keys, "gos3/gos3.json") ||
		slices.ContainsFunc(keys, func(k string) bool { return k != "unrelated.txt" && !strings.HasPrefix(k, "gos3/") }) {
		t.Fatalf("spare's remote keys: %v", keys)
	}
	var containers struct {
		Containers []s3account.ContainerInfo `json:"containers"`
	}
	res = adminDo(t, srv.url, "GET", "/backings/cloud/containers", nil)
	mustStatus(t, "list containers", res, 200)
	res.json(t, &containers)
	if len(containers.Containers) != 3 || containers.Containers[0].ID != "home" {
		t.Fatalf("cloud's containers: %s", res.body)
	}
	if b := backingStatuses(t, srv.url)["edge"]; !slices.Contains(b.Kinds, "wrapped") || b.DefaultKind != "native" {
		t.Fatalf("edge with a container: %+v", b)
	}

	// (e) Restart: the records of edge (in the system directory) and cloud
	// (in its home) survive, and so does every bucket.
	if err := srv.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(S3RecordsDir(filepath.Join(cfg.Storage.Path, config.SystemDirName), "edge"), "state.db")); err != nil {
		t.Fatalf("edge's records: %v", err)
	}
	srv = start(t, cfg)
	headBucket(t, srv.url, "alias", "edge", "native")
	getBody(t, srv.url+"/alias/doc", "three")
	getBody(t, srv.url+"/alias/doc?versionId="+v1, "one")
	for _, bucket := range []string{"boxed", "shelved", "fresh", "edgebox"} {
		getBody(t, srv.url+"/"+bucket+"/k", "in "+bucket)
	}
	res = adminDo(t, srv.url, "GET", "/backings/cloud/containers", nil)
	mustStatus(t, "list containers after restart", res, 200)
	res.json(t, &containers)
	if len(containers.Containers) != 3 {
		t.Fatalf("cloud's containers after restart: %s", res.body)
	}

	// Release: gos3 forgets the bucket; the remote bucket and its objects
	// stay.
	before := versionList(t, rc, "existing", "")
	mustStatus(t, "release alias", adminDo(t, srv.url, "POST", "/buckets/alias/release", nil), 204)
	mustStatus(t, "HEAD alias", do(t, "HEAD", srv.url+"/alias", nil, &rootCreds, nil), 404)
	if after := versionList(t, rc, "existing", ""); !slices.Equal(before, after) {
		t.Fatalf("the remote bucket changed on release:\nbefore %v\nafter  %v", before, after)
	}
	mustStatus(t, "release a wrapped bucket", adminDo(t, srv.url, "POST", "/buckets/boxed/release", nil), 400)
	_, byName = remoteBucketsOf(t, srv.url, "edge")
	if b := byName["existing"]; b.Status != "unmanaged" {
		t.Fatalf("existing after release: %+v", b)
	}
	// Once released it can be adopted again.
	mustStatus(t, "adopt after release", adminDo(t, srv.url, "POST", "/backings/edge/buckets",
		map[string]any{"bucket": "alias", "remoteBucket": "existing", "adopt": true}), 201)
	getBody(t, srv.url+"/alias/doc", "three")

	// Deleting a native bucket gos3 created deletes its remote bucket.
	mustStatus(t, "delete k", do(t, "DELETE", srv.url+"/fresh/k", nil, &rootCreds, nil), 204)
	mustStatus(t, "delete fresh", do(t, "DELETE", srv.url+"/fresh", nil, &rootCreds, nil), 204)
	if remoteHas(remote, "fresh") {
		t.Fatal("the remote bucket of a deleted native bucket gos3 created is still there")
	}
}
