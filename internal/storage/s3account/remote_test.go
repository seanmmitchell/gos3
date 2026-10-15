package s3account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestRemoteBuckets checks the statuses of the remote bucket browser, and
// the fallback listing when the credentials may not list buckets.
func TestRemoteBuckets(t *testing.T) {
	var denyList atomic.Bool
	r := s3clienttest.StartWith(t, s3clienttest.Options{Wrap: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			switch {
			case denyList.Load() && req.Method == http.MethodGet && req.URL.Path == "/":
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>no ListBuckets</Message></Error>`)
				return
			case strings.HasPrefix(req.URL.Path, "/zz-private/"):
				w.WriteHeader(http.StatusForbidden)
				return
			}
			h.ServeHTTP(w, req)
		})
	}})
	for _, n := range []string{"home", "other-home", "plain", "stray-store", "zz-private"} {
		r.CreateBucket(t, n)
	}
	ctx := context.Background()
	claims := NewClaims()
	b := openReady(t, homeOptions(t, r, claims, "home", "a/"))
	oo := homeOptions(t, r, claims, "other-home", "")
	oo.Name = "other" // a second backing of the same server
	openReady(t, oo)
	ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "cont", CreateBucket: true})
	noErr(t, "AddContainer", err)
	noErr(t, "create native", b.CreateBucket(ctx, "mine", storage.CreateBucketOptions{Target: nativeTarget("remote-mine", false)}))
	remotePut(t, r.Client(t), "stray-store", "gos3.json", `{"format":1}`)

	list, err := b.RemoteBuckets(ctx, ListRemoteOptions{Probe: true})
	noErr(t, "RemoteBuckets", err)
	if !list.Complete {
		t.Fatalf("incomplete listing: %+v", list)
	}
	got := map[string]RemoteBucket{}
	for _, rb := range list.Buckets {
		got[rb.Name] = rb
	}
	want := map[string]string{
		"home": remoteHome, "cont": remoteContainer, "remote-mine": remoteNative, "other-home": remoteClaimed,
		"plain": remoteUnmanaged, "stray-store": remoteGos3, "zz-private": remoteInaccessible,
	}
	for name, status := range want {
		if got[name].Status != status {
			t.Errorf("%s: status %q, want %q (%+v)", name, got[name].Status, status, got[name])
		}
	}
	if u := got["remote-mine"].Uses; len(u) != 1 || u[0].Bucket != "mine" || !u[0].Created {
		t.Errorf("uses of remote-mine: %+v", u)
	}
	if u := got["cont"].Uses; len(u) != 1 || u[0].ContainerID != ci.ID {
		t.Errorf("uses of cont: %+v", u)
	}
	// Without probing, nothing is looked into.
	list, err = b.RemoteBuckets(ctx, ListRemoteOptions{Prefix: "st"})
	noErr(t, "RemoteBuckets(prefix)", err)
	if len(list.Buckets) != 1 || list.Buckets[0].Status != remoteUnmanaged {
		t.Fatalf("prefix listing %+v", list.Buckets)
	}

	denyList.Store(true)
	list, err = b.RemoteBuckets(ctx, ListRemoteOptions{})
	noErr(t, "RemoteBuckets(denied)", err)
	if list.Complete || list.ListError == "" {
		t.Fatalf("denied listing %+v", list)
	}
	names := []string{}
	for _, rb := range list.Buckets {
		names = append(names, rb.Name+"="+rb.Status)
	}
	if strings.Join(names, ",") != "cont=container,home=home,remote-mine=native" {
		t.Fatalf("known buckets %v", names)
	}
}

// TestClaimConflicts: remote locations in use by this or another backing
// cannot be adopted, created or used as containers, and a conflict found
// while loading makes only the bucket unavailable.
func TestClaimConflicts(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	r.CreateBucket(t, "free")
	ctx := context.Background()
	claims := NewClaims()
	a := openReady(t, homeOptions(t, r, claims, "home", "a/"))
	bo := localOptions(t, r, claims, t.TempDir())
	bo.Name = "b"
	b := openReady(t, bo)

	// Adopting the home's bucket.
	err := a.CreateBucket(ctx, "x1", storage.CreateBucketOptions{Target: nativeTarget("home", true)})
	wantErr(t, "adopt the home bucket", err, ErrClaimed)
	_, err = b.CheckAdoptable(ctx, "home")
	wantErr(t, "CheckAdoptable(home)", err, ErrClaimed)
	// A container nested in a home, either way.
	_, err = a.AddContainer(ctx, ContainerSpec{Bucket: "home", Prefix: "a/inner"})
	wantErr(t, "container inside the home", err, ErrClaimed)
	_, err = b.AddContainer(ctx, ContainerSpec{Bucket: "home"})
	wantErr(t, "container around the home", err, ErrClaimed)
	// A container's bucket.
	ci, err := a.AddContainer(ctx, ContainerSpec{Bucket: "home", Prefix: "c/"})
	noErr(t, "AddContainer beside the home", err)
	err = b.CreateBucket(ctx, "x2", storage.CreateBucketOptions{Target: nativeTarget("home", true)})
	wantErr(t, "adopt a container's bucket", err, ErrClaimed)
	// Another backing's native bucket.
	noErr(t, "adopt free", a.CreateBucket(ctx, "freed", storage.CreateBucketOptions{Target: nativeTarget("free", true)}))
	err = b.CreateBucket(ctx, "x3", storage.CreateBucketOptions{Target: nativeTarget("free", true)})
	wantErr(t, "adopt another backing's native bucket", err, ErrClaimed)
	_, err = b.AddContainer(ctx, ContainerSpec{Bucket: "free", Prefix: "p/"})
	wantErr(t, "container in a native bucket's remote bucket", err, ErrClaimed)
	// A store not claimed by this server (another server's container): a
	// container nested in it is refused.
	c2 := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	_, err = c2.AddContainer(ctx, ContainerSpec{Bucket: "home", Prefix: "c/deeper/"})
	wantErr(t, "container nested in a store", err, ErrClaimed)
	_ = ci

	// A conflict found while loading: a releases "freed", b adopts the
	// remote bucket, then a's records say "freed" again (as after a
	// restore of old records).
	a.mu.RLock()
	rec := a.reg.buckets["freed"].bucketRecord
	a.mu.RUnlock()
	noErr(t, "ReleaseBucket", a.ReleaseBucket(ctx, "freed"))
	noErr(t, "b adopts free", b.CreateBucket(ctx, "bfree", storage.CreateBucketOptions{Target: nativeTarget("free", true)}))
	a.adminMu.Lock()
	noErr(t, "putBucket", a.records.PutRecord(ctx, bucketKey("freed"), recordJSON(rec)))
	a.adminMu.Unlock()
	noErr(t, "Close", a.Close())
	a = openReady(t, homeOptions(t, r, claims, "home", "a/"))
	_, err = a.GetObjectInfo(ctx, "freed", "k", "")
	wantErr(t, "conflicting native bucket", err, storage.ErrBucketUnavailable)
	noErr(t, "ProbeHealth", a.ProbeHealth(ctx))
	put(t, b, "bfree", "k", "b's")
	// It can be released, which ends the conflict.
	noErr(t, "release the conflicting bucket", a.ReleaseBucket(ctx, "freed"))
	if hs := claims.Holders(r.URL, "free"); len(hs) != 1 || hs[0].Backing != "b" {
		t.Fatalf("holders of free: %+v", hs)
	}
}

// TestContainers covers adding (empty, non-empty, attach, legacy, nested),
// listing, defaulting and removing containers.
func TestContainers(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	r.CreateBucket(t, "exists")
	r.CreateBucket(t, "crowded")
	remotePut(t, r.Client(t), "crowded", "data/file", "x")
	ctx := context.Background()
	b := openReady(t, homeOptions(t, r, NewClaims(), "home", ""))

	_, err := b.AddContainer(ctx, ContainerSpec{Bucket: "missing"})
	wantErr(t, "missing bucket", err, storage.ErrInvalidTarget)
	_, err = b.AddContainer(ctx, ContainerSpec{Bucket: "crowded", Prefix: "data"})
	wantErr(t, "non-empty prefix", err, storage.ErrInvalidTarget)
	_, err = b.AddContainer(ctx, ContainerSpec{Bucket: "exists", Prefix: "../x/"})
	wantErr(t, "invalid prefix", err, storage.ErrInvalidTarget)
	_, err = b.AddContainer(ctx, ContainerSpec{Bucket: "exists", Attach: true})
	wantErr(t, "attach nothing", err, storage.ErrInvalidTarget)
	_, err = b.AddContainer(ctx, ContainerSpec{Bucket: "exists", CreateBucket: true})
	wantErr(t, "create an existing bucket", err, storage.ErrRemoteBucketExists)
	if hs := b.holders("exists"); len(hs) != 0 {
		t.Fatalf("claims after failed adds: %v", hs)
	}

	// A container in an existing bucket, with a folder placeholder.
	remotePut(t, r.Client(t), "exists", "team/", "")
	ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "exists", Prefix: "team", Label: "Team"})
	noErr(t, "AddContainer", err)
	if ci.Location.Prefix != "team/" || ci.State != "ready" || ci.Role != containerRoleExisting || ci.Label != "Team" {
		t.Fatalf("container %+v", ci)
	}
	noErr(t, "create", b.CreateBucket(ctx, "tb", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: ci.ID}}))
	put(t, b, "tb", "k", "team data")
	noErr(t, "SetDefaultContainer", b.SetDefaultContainer(ctx, ci.ID))
	wantErr(t, "remove a container with buckets", b.RemoveContainer(ctx, ci.ID), storage.ErrBucketNotEmpty)
	wantErr(t, "remove the home", b.RemoveContainer(ctx, homeID), storage.ErrInvalidTarget)
	wantErr(t, "SetDefaultContainer(unknown)", b.SetDefaultContainer(ctx, "nope"), storage.ErrInvalidTarget)
	noErr(t, "Close", b.Close())

	// Another server attaches the container (here: a backing with its own
	// claims, after the first one stopped); its buckets come along.
	o2 := localOptions(t, r, NewClaims(), t.TempDir())
	b2 := openReady(t, o2)
	_, err = b2.AddContainer(ctx, ContainerSpec{Bucket: "exists", Prefix: "team/"})
	wantErr(t, "add over a store without attach", err, ErrClaimed)
	ci2, err := b2.AddContainer(ctx, ContainerSpec{Bucket: "exists", Prefix: "team/", Attach: true})
	noErr(t, "attach", err)
	if k := bucketKinds(t, b2)["tb"]; k != storage.BucketKindWrapped {
		t.Fatalf("attached bucket kind %q", k)
	}
	if got := read(t, b2, "tb", "k"); got != "team data" {
		t.Fatalf("tb/k = %q", got)
	}
	_, err = b2.DeleteObject(ctx, "tb", "k", "")
	noErr(t, "DeleteObject", err)
	noErr(t, "DeleteBucket", b2.DeleteBucket(ctx, "tb"))
	noErr(t, "RemoveContainer", b2.RemoveContainer(ctx, ci2.ID))
	if cs := b2.Containers(ctx); len(cs) != 0 {
		t.Fatalf("containers after removal: %+v", cs)
	}
	if _, err := r.Backend.GetObjectInfo(ctx, "exists", "team/gos3.json", ""); err != nil {
		t.Fatalf("removing a container deleted its store: %v", err)
	}
	// Removed: attachable again (it is empty now).
	_, err = b2.AddContainer(ctx, ContainerSpec{Bucket: "exists", Prefix: "team/", Attach: true})
	noErr(t, "attach again", err)
}

// TestCheckAdoptable refuses remote buckets holding a gos3 store at the
// root or in a top-level prefix.
func TestCheckAdoptable(t *testing.T) {
	r := s3clienttest.Start(t)
	for _, n := range []string{"clean", "rooted", "nested"} {
		r.CreateBucket(t, n)
	}
	c := r.Client(t)
	remotePut(t, c, "clean", "a/b", "x")
	remotePut(t, c, "rooted", "gos3.json", "{}")
	remotePut(t, c, "nested", "pfx/gos3.json", "{}")
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), t.TempDir()))
	region, err := b.CheckAdoptable(ctx, "clean")
	noErr(t, "CheckAdoptable(clean)", err)
	if region != s3clienttest.Region {
		t.Fatalf("region %q", region)
	}
	for _, n := range []string{"rooted", "nested"} {
		if _, err := b.CheckAdoptable(ctx, n); !errors.Is(err, ErrClaimed) {
			t.Fatalf("CheckAdoptable(%s): %v", n, err)
		}
	}
	if _, err := b.CheckAdoptable(ctx, "missing"); !errors.Is(err, storage.ErrInvalidTarget) || errors.Is(err, ErrClaimed) {
		t.Fatalf("CheckAdoptable(missing): %v", err)
	}
	noErr(t, "CreateRemoteBucket", b.CreateRemoteBucket(ctx, "fresh", "", false))
	wantErr(t, "CreateRemoteBucket again", b.CreateRemoteBucket(ctx, "fresh", "", false), storage.ErrRemoteBucketExists)
	if !remoteHasBucket(t, r, "fresh") {
		t.Fatal("CreateRemoteBucket did not create the bucket")
	}
}
