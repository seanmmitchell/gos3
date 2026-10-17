package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
	"github.com/seanmmitchell/gos3/internal/storage/s3account"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// fakeAccount is a fake s3 backing: a memory backend holding native and
// wrapped buckets as far as the router and the admin API can tell
// (storage.BucketCapabilities, BucketReleaser, KindReporter and
// s3account.RemoteManager). It records what it was asked to do.
type fakeAccount struct {
	*memory.Backend
	def storage.BucketKind

	mu         sync.Mutex
	kinds      map[string]storage.BucketKind // gos3 bucket -> kind
	locations  map[string]string             // gos3 bucket -> location
	created    map[string]storage.CreateBucketOptions
	released   []string
	containers []s3account.ContainerInfo
	remote     []s3account.RemoteBucket
	listOpts   []s3account.ListRemoteOptions
	added      []s3account.ContainerSpec
	removed    []string
	nextID     int
	// Errors to return.
	adoptErr  map[string]error // CheckAdoptable, by remote bucket
	addErr    error            // AddContainer
	createErr error            // CreateBucket
	removeErr error            // RemoveContainer
}

var (
	_ storage.BucketCapabilities = (*fakeAccount)(nil)
	_ storage.BucketReleaser     = (*fakeAccount)(nil)
	_ storage.KindReporter       = (*fakeAccount)(nil)
	_ s3account.RemoteManager    = (*fakeAccount)(nil)
)

// newFakeAccount returns a fake s3 backing with a home ("acme-gos3/gos3/",
// the default container) holding wrapped buckets by default, and one
// unmanaged remote bucket, "acme-photos".
func newFakeAccount() *fakeAccount {
	return &fakeAccount{
		Backend:   memory.New(),
		def:       storage.BucketKindWrapped,
		kinds:     map[string]storage.BucketKind{},
		locations: map[string]string{},
		created:   map[string]storage.CreateBucketOptions{},
		adoptErr:  map[string]error{},
		containers: []s3account.ContainerInfo{{
			ID: "home", Label: "shared", Location: s3account.Location{Bucket: "acme-gos3", Prefix: "gos3/"},
			Role: "home", Default: true, State: "ready",
		}},
		remote: []s3account.RemoteBucket{
			{Name: "acme-gos3", Region: "eu-central-1", Status: "home"},
			{Name: "acme-photos", Region: "eu-central-1", Status: "unmanaged"},
		},
	}
}

func (f *fakeAccount) caps(kind storage.BucketKind) storage.BucketCaps {
	if kind == storage.BucketKindNative {
		return storage.BucketCaps{Kind: kind, Clone: true, Versioning: true, ObjectLock: true}
	}
	return storage.BucketCaps{Kind: kind, Import: true, History: true, Clone: true, Versioning: true, ObjectLock: true, Compression: true}
}

// placeLocked returns the kind and location of a bucket created with
// opts. f.mu must be held.
func (f *fakeAccount) placeLocked(name string, opts storage.CreateBucketOptions) (storage.BucketKind, string, error) {
	kind, t := f.def, opts.Target
	if t != nil && t.Kind != "" {
		kind = t.Kind
	}
	if kind == storage.BucketKindNative {
		remote := name
		if t != nil && t.RemoteBucket != "" {
			remote = t.RemoteBucket
		}
		return kind, "s3://" + remote + "/", nil
	}
	id := ""
	if t != nil {
		id = t.Container
	}
	for _, c := range f.containers {
		if (id == "" && c.Default) || c.ID == id {
			return kind, c.Location.String(), nil
		}
	}
	return "", "", fmt.Errorf("%w: no container %q", storage.ErrInvalidTarget, id)
}

func (f *fakeAccount) BucketCaps(_ context.Context, bucket string) (storage.BucketCaps, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind, ok := f.kinds[bucket]
	if !ok {
		return storage.BucketCaps{}, storage.ErrBucketNotFound
	}
	return f.caps(kind), nil
}

func (f *fakeAccount) NewBucketCaps(_ context.Context, opts storage.CreateBucketOptions) (storage.BucketCaps, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind, _, err := f.placeLocked("", opts)
	if err != nil {
		return storage.BucketCaps{}, err
	}
	return f.caps(kind), nil
}

func (f *fakeAccount) BucketKinds() ([]storage.BucketKind, storage.BucketKind) {
	return []storage.BucketKind{storage.BucketKindNative, storage.BucketKindWrapped}, f.def
}

func (f *fakeAccount) CreateBucket(ctx context.Context, name string, opts storage.CreateBucketOptions) error {
	f.mu.Lock()
	kind, loc, err := f.placeLocked(name, opts)
	if err == nil && f.createErr != nil {
		err = f.createErr
	}
	if err == nil && kind == storage.BucketKindNative && (opts.Target == nil || !opts.Target.Adopt) {
		remote := strings.TrimSuffix(strings.TrimPrefix(loc, "s3://"), "/")
		if slices.ContainsFunc(f.remote, func(b s3account.RemoteBucket) bool { return b.Name == remote }) {
			err = fmt.Errorf("%w: %q", storage.ErrRemoteBucketExists, remote)
		}
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	inner := opts
	inner.Target, inner.Import = nil, false
	if err := f.Backend.CreateBucket(ctx, name, inner); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kinds[name], f.locations[name], f.created[name] = kind, loc, opts
	for i, c := range f.containers {
		if kind == storage.BucketKindWrapped && c.Location.String() == loc {
			f.containers[i].Buckets++
		}
	}
	return nil
}

// annotate sets the kind and location of a bucket.
func (f *fakeAccount) annotate(in storage.BucketInfo) storage.BucketInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	in.Kind, in.Location = f.kinds[in.Name], f.locations[in.Name]
	return in
}

func (f *fakeAccount) GetBucket(ctx context.Context, name string) (storage.BucketInfo, error) {
	in, err := f.Backend.GetBucket(ctx, name)
	if err != nil {
		return in, err
	}
	return f.annotate(in), nil
}

func (f *fakeAccount) ListBuckets(ctx context.Context) ([]storage.BucketInfo, error) {
	list, err := f.Backend.ListBuckets(ctx)
	for i := range list {
		list[i] = f.annotate(list[i])
	}
	return list, err
}

func (f *fakeAccount) ReleaseBucket(ctx context.Context, name string) error {
	f.mu.Lock()
	kind := f.kinds[name]
	f.mu.Unlock()
	if kind != storage.BucketKindNative {
		return fmt.Errorf("releasing a wrapped bucket: %w", storage.ErrNotSupported)
	}
	if err := f.Backend.DeleteBucket(ctx, name); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, name)
	return nil
}

func (f *fakeAccount) RemoteBuckets(_ context.Context, opts s3account.ListRemoteOptions) (s3account.RemoteBucketList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listOpts = append(f.listOpts, opts)
	out := s3account.RemoteBucketList{Complete: true}
	for _, b := range f.remote {
		if strings.HasPrefix(b.Name, opts.Prefix) {
			out.Buckets = append(out.Buckets, b)
		}
	}
	return out, nil
}

func (f *fakeAccount) CheckAdoptable(_ context.Context, remote string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.adoptErr[remote]; err != nil {
		return "", err
	}
	return "eu-central-1", nil
}

func (f *fakeAccount) CreateRemoteBucket(_ context.Context, name, region string, objectLock bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.ContainsFunc(f.remote, func(b s3account.RemoteBucket) bool { return b.Name == name }) {
		return fmt.Errorf("%w: %q", storage.ErrRemoteBucketExists, name)
	}
	note := ""
	if objectLock {
		note = "object lock"
	}
	f.remote = append(f.remote, s3account.RemoteBucket{Name: name, Region: region, Status: "unmanaged", Note: note})
	return nil
}

func (f *fakeAccount) Containers(context.Context) []s3account.ContainerInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.containers)
}

func (f *fakeAccount) AddContainer(_ context.Context, spec s3account.ContainerSpec) (s3account.ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, spec)
	if f.addErr != nil {
		return s3account.ContainerInfo{}, f.addErr
	}
	f.nextID++
	role := "existing"
	if spec.CreateBucket {
		role = "dedicated"
	}
	ci := s3account.ContainerInfo{
		ID: fmt.Sprintf("c-%d", f.nextID), Label: spec.Label, Location: s3account.Location{Bucket: spec.Bucket, Prefix: spec.Prefix},
		Region: spec.Region, Role: role, State: "ready",
	}
	f.containers = append(f.containers, ci)
	return ci, nil
}

func (f *fakeAccount) RemoveContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, id)
	f.containers = slices.DeleteFunc(f.containers, func(c s3account.ContainerInfo) bool { return c.ID == id })
	return nil
}

func (f *fakeAccount) SetDefaultContainer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.containers {
		f.containers[i].Default = f.containers[i].ID == id
	}
	return nil
}

func (f *fakeAccount) Info() s3account.AccountInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := s3account.AccountInfo{
		Home: &s3account.Location{Bucket: "acme-gos3", Prefix: "gos3/"}, Records: "home", DefaultKind: f.def,
		Kinds: []storage.BucketKind{storage.BucketKindNative, storage.BucketKindWrapped},
	}
	for _, c := range f.containers {
		if c.Default {
			info.DefaultContainer = c.ID
		}
	}
	for _, k := range f.kinds {
		if k == storage.BucketKindNative {
			info.NativeBuckets++
		} else {
			info.WrappedBuckets++
		}
	}
	return info
}

// createdWith returns the options the bucket was created with.
func (f *fakeAccount) createdWith(t *testing.T, bucket string) storage.CreateBucketOptions {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	opts, ok := f.created[bucket]
	if !ok {
		t.Fatalf("bucket %q was not created on the fake account", bucket)
	}
	return opts
}

// newAccountEnv returns a test environment whose backend is a router over
// a memory backing "default" (the default), a second memory backing "fast"
// and two fake s3 backings, "cloud" and "cloud2".
func newAccountEnv(t *testing.T) (*testEnv, *multi.Router, *fakeAccount, *fakeAccount) {
	t.Helper()
	cloud, cloud2 := newFakeAccount(), newFakeAccount()
	r, err := multi.Open(context.Background(), multi.Options{
		Backings: []multi.Backing{
			{Name: "default", Kind: "memory", Backend: memory.New()},
			{Name: "fast", Kind: "memory", Backend: memory.New()},
			{Name: "cloud", Kind: "s3", Location: "s3://acme-gos3/gos3/", Backend: cloud},
			{Name: "cloud2", Kind: "s3", Location: "s3://acme-gos3/other/", Backend: cloud2},
		},
		HealthInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	e := newEnv(t, func(d *Deps) { d.Backend = r })
	return e, r, cloud, cloud2
}

func TestRemoteBucketBrowser(t *testing.T) {
	e, _, cloud, _ := newAccountEnv(t)

	var list s3account.RemoteBucketList
	e.root("GET", "/backings/cloud/remote-buckets?prefix=acme-p&maxBuckets=10&probe=true&continuationToken=tok", nil).json(t, &list)
	if !list.Complete || len(list.Buckets) != 1 || list.Buckets[0].Name != "acme-photos" {
		t.Fatalf("remote buckets = %+v", list)
	}
	if got := cloud.listOpts[0]; got != (s3account.ListRemoteOptions{Prefix: "acme-p", ContinuationToken: "tok", MaxBuckets: 10, Probe: true}) {
		t.Errorf("list options = %+v", got)
	}
	// An empty page is an empty array.
	r := e.root("GET", "/backings/cloud/remote-buckets?prefix=zzz", nil)
	r.expectStatus(t, 200)
	if !strings.Contains(string(r.body), `"buckets":[]`) {
		t.Errorf("empty page = %s", r.body)
	}
	for _, q := range []string{"maxBuckets=0", "maxBuckets=1001", "maxBuckets=x", "probe=maybe", "prefix=" + strings.Repeat("a", 256)} {
		e.root("GET", "/backings/cloud/remote-buckets?"+q, nil).expectError(t, 400, codeInvalidRequest)
	}
	e.root("GET", "/backings/nope/remote-buckets", nil).expectError(t, 404, codeNotFound)
	msg := e.root("GET", "/backings/default/remote-buckets", nil).expectError(t, 400, codeInvalidRequest)
	if !strings.Contains(msg, "not an s3 backing") {
		t.Errorf("message = %q", msg)
	}

	// Creating an unmanaged remote bucket.
	var rb s3account.RemoteBucket
	r = e.root("POST", "/backings/cloud/remote-buckets", map[string]any{"name": "acme-new", "region": "eu-west-1", "objectLock": true})
	r.expectStatus(t, 201)
	r.json(t, &rb)
	if rb.Name != "acme-new" || rb.Region != "eu-west-1" || rb.Status != "unmanaged" {
		t.Errorf("created = %+v", rb)
	}
	if b := cloud.remote[len(cloud.remote)-1]; b.Name != "acme-new" || b.Region != "eu-west-1" || b.Note != "object lock" {
		t.Errorf("remote = %+v", b)
	}
	e.root("POST", "/backings/cloud/remote-buckets", map[string]any{"name": "acme-new"}).expectError(t, 409, codeConflict)
	for _, body := range []map[string]any{
		{"name": "Bad_Name"}, {"name": "ab"}, {}, {"name": "acme-x", "region": "EU WEST"}, {"name": "acme-x", "bogus": 1},
	} {
		e.root("POST", "/backings/cloud/remote-buckets", body).expectError(t, 400, codeInvalidRequest)
	}
}

func TestContainersEndpoints(t *testing.T) {
	e, r, cloud, _ := newAccountEnv(t)

	var cl struct {
		Containers []s3account.ContainerInfo `json:"containers"`
	}
	e.root("GET", "/backings/cloud/containers", nil).json(t, &cl)
	if len(cl.Containers) != 1 || cl.Containers[0].ID != "home" || !cl.Containers[0].Default {
		t.Fatalf("containers = %+v", cl)
	}
	e.root("GET", "/backings/fast/containers", nil).expectError(t, 400, codeInvalidRequest)

	// Adding: the prefix is made to end in "/".
	var ci s3account.ContainerInfo
	res := e.root("POST", "/backings/cloud/containers", map[string]any{"bucket": "acme-archive", "prefix": "gos3/x", "label": " archive "})
	res.expectStatus(t, 201)
	res.json(t, &ci)
	want := s3account.ContainerSpec{Bucket: "acme-archive", Prefix: "gos3/x/", Label: "archive"}
	if ci.ID != "c-1" || cloud.added[0] != want {
		t.Fatalf("added %+v from %+v", ci, cloud.added)
	}
	for _, body := range []map[string]any{
		{"bucket": "acme-new", "createBucket": true, "attach": true},
		{"bucket": ""},
		{"bucket": "Acme"},
		{"bucket": "acme-x", "createBucket": true, "prefix": "a//b"},
		{"bucket": "acme-x", "prefix": "/abs"},
		{"bucket": "acme-x", "prefix": "a/../b"},
		{"bucket": "acme-x", "label": "bad\nlabel"},
		{"bucket": "acme-x", "label": strings.Repeat("l", 129)},
		{"bucket": "acme-x", "region": "Nope!"},
		{"bucket": "acme-x", "unknown": true},
	} {
		e.root("POST", "/backings/cloud/containers", body).expectError(t, 400, codeInvalidRequest)
	}
	if len(cloud.added) != 1 {
		t.Errorf("invalid specs reached the backing: %+v", cloud.added)
	}
	// Errors of the backing.
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("x: %w", s3store.ErrPrefixNotEmpty), 400, codeInvalidRequest},
		{fmt.Errorf("x: %w", s3store.ErrMarkerMissing), 400, codeInvalidRequest},
		{fmt.Errorf("x: %w", s3store.ErrInUse), 409, codeConflict},
		{fmt.Errorf("x: %w", s3account.ErrClaimed), 409, codeConflict},
		{fmt.Errorf("x: %w", storage.ErrInvalidTarget), 400, codeInvalidRequest},
		{fmt.Errorf("x: %w", storage.ErrBucketUnavailable), 503, codeInternalError},
		{fmt.Errorf("x: %w", storage.ErrRemoteDenied), 400, codeInvalidRequest},
	} {
		cloud.mu.Lock()
		cloud.addErr = c.err
		cloud.mu.Unlock()
		e.root("POST", "/backings/cloud/containers", map[string]any{"bucket": "acme-y"}).expectError(t, c.status, c.code)
	}
	cloud.mu.Lock()
	cloud.addErr = nil
	cloud.mu.Unlock()

	// The default container.
	cl.Containers = nil
	e.root("PUT", "/backings/cloud/containers/default", map[string]any{"id": "c-1"}).json(t, &cl)
	if len(cl.Containers) != 2 || cl.Containers[0].Default || !cl.Containers[1].Default {
		t.Fatalf("after PUT default: %+v", cl)
	}
	e.root("PUT", "/backings/cloud/containers/default", map[string]any{"id": "c-9"}).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", "/backings/cloud/containers/default", map[string]any{}).expectError(t, 400, codeInvalidRequest)
	e.root("PUT", "/backings/cloud/containers/default", map[string]any{"id": "home"}).expectStatus(t, 200)

	// Removing: not the home, not one holding buckets.
	e.root("DELETE", "/backings/cloud/containers/home", nil).expectError(t, 400, codeInvalidRequest)
	e.root("DELETE", "/backings/cloud/containers/c-9", nil).expectError(t, 404, codeNotFound)
	if err := r.CreateBucket(context.Background(), "in-c1", storage.CreateBucketOptions{
		Backing: "cloud", Target: &storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "c-1"},
	}); err != nil {
		t.Fatal(err)
	}
	e.root("DELETE", "/backings/cloud/containers/c-1", nil).expectError(t, 409, codeConflict)
	cloud.mu.Lock()
	cloud.containers[1].Buckets = 0
	cloud.mu.Unlock()
	e.root("DELETE", "/backings/cloud/containers/c-1", nil).expectStatus(t, 204)
	if !slices.Equal(cloud.removed, []string{"c-1"}) {
		t.Errorf("removed = %v", cloud.removed)
	}

	// 405s: the literal path .../containers/default and the wildcard one
	// .../containers/{id} each allow their own methods.
	res = e.root("GET", "/backings/cloud/containers/default", nil)
	res.expectError(t, 405, codeMethodNotAllowed)
	if got := res.header.Get("Allow"); got != "PUT, OPTIONS" {
		t.Errorf("Allow = %q", got)
	}
	e.root("DELETE", "/backings/cloud/containers/default", nil).expectError(t, 405, codeMethodNotAllowed)
	res = e.root("PATCH", "/backings/cloud/containers/c-2", map[string]any{})
	res.expectError(t, 405, codeMethodNotAllowed)
	if got := res.header.Get("Allow"); got != "DELETE, OPTIONS" {
		t.Errorf("Allow = %q", got)
	}
}

func TestCreateBucketOnBacking(t *testing.T) {
	e, r, cloud, _ := newAccountEnv(t)
	ctx := context.Background()

	create := func(body map[string]any) bucketSummary {
		t.Helper()
		res := e.root("POST", "/backings/cloud/buckets", body)
		res.expectStatus(t, 201)
		var s bucketSummary
		res.json(t, &s)
		return s
	}
	target := func(bucket string) storage.BucketTarget {
		t.Helper()
		opts := cloud.createdWith(t, bucket)
		if opts.Target == nil {
			t.Fatalf("%s: no target", bucket)
		}
		return *opts.Target
	}

	// A native bucket on a new remote bucket (named after the gos3 bucket).
	s := create(map[string]any{"bucket": "photos", "kind": "native", "objectLock": true})
	if s.Name != "photos" || s.Kind != "native" || s.Location != "s3://photos/" || s.Owner != "root" ||
		s.Region != "us-east-1" || !s.ObjectLock || s.Usage != nil || s.Placement == nil || s.Placement.Primary != "cloud" {
		t.Fatalf("summary = %+v", s)
	}
	opts := cloud.createdWith(t, "photos")
	if opts.Owner != "" || opts.Region != "us-east-1" || !opts.ObjectLockEnabled || opts.Backing != "" ||
		*opts.Target != (storage.BucketTarget{Kind: storage.BucketKindNative, RemoteBucket: "photos"}) {
		t.Fatalf("options = %+v target %+v", opts, opts.Target)
	}

	// An existing remote bucket is not created over: adopt it instead.
	msg := e.root("POST", "/backings/cloud/buckets", map[string]any{"bucket": "pics", "remoteBucket": "acme-photos"}).
		expectError(t, 409, codeConflict)
	if !strings.Contains(msg, "adopt") {
		t.Errorf("message = %q", msg)
	}
	// Adopting (the kind is implied); the region comes from the check.
	s = create(map[string]any{"bucket": "pics", "remoteBucket": "acme-photos", "adopt": true})
	if s.Kind != "native" || s.Location != "s3://acme-photos/" {
		t.Errorf("adopted summary = %+v", s)
	}
	if got := target("pics"); got != (storage.BucketTarget{Kind: storage.BucketKindNative, RemoteBucket: "acme-photos",
		RemoteRegion: "eu-central-1", Adopt: true}) {
		t.Errorf("adopt target = %+v", got)
	}
	// An explicit region wins; a refused adoption creates nothing.
	create(map[string]any{"bucket": "pics2", "kind": "native", "remoteBucket": "legacy_bucket.x", "adopt": true, "region": "us-west-2"})
	if got := target("pics2"); got.RemoteRegion != "us-west-2" || got.RemoteBucket != "legacy_bucket.x" {
		t.Errorf("adopt target = %+v", got)
	}
	cloud.adoptErr["acme-gos3"] = fmt.Errorf("%w: s3://acme-gos3/ holds a gos3 store", s3account.ErrClaimed)
	e.root("POST", "/backings/cloud/buckets", map[string]any{"bucket": "store", "remoteBucket": "acme-gos3", "adopt": true}).
		expectError(t, 409, codeConflict)
	cloud.adoptErr["acme-gone"] = fmt.Errorf("%w: the remote bucket does not exist", storage.ErrInvalidTarget)
	e.root("POST", "/backings/cloud/buckets", map[string]any{"bucket": "store", "remoteBucket": "acme-gone", "adopt": true}).
		expectError(t, 400, codeInvalidRequest)
	if _, err := r.GetBucket(ctx, "store"); !errors.Is(err, storage.ErrBucketNotFound) {
		t.Errorf("refused adoption created the bucket: %v", err)
	}

	// Wrapped buckets: the default kind and container, a chosen container,
	// a new container.
	s = create(map[string]any{"bucket": "docs"})
	if s.Kind != "wrapped" || s.Location != "s3://acme-gos3/gos3/" {
		t.Errorf("wrapped summary = %+v", s)
	}
	if got := target("docs"); got != (storage.BucketTarget{Kind: storage.BucketKindWrapped}) {
		t.Errorf("wrapped target = %+v", got)
	}
	s = create(map[string]any{"bucket": "archive", "newContainer": map[string]any{"bucket": "acme-archive", "createBucket": true, "label": "archive"}})
	if got := target("archive"); got != (storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "c-1"}) {
		t.Errorf("new container target = %+v", got)
	}
	if s.Location != "s3://acme-archive/" || cloud.added[0] != (s3account.ContainerSpec{Bucket: "acme-archive", CreateBucket: true, Label: "archive"}) {
		t.Errorf("summary %+v, added %+v", s, cloud.added)
	}
	create(map[string]any{"bucket": "archive2", "kind": "wrapped", "container": "c-1"})
	if got := target("archive2"); got.Container != "c-1" {
		t.Errorf("container target = %+v", got)
	}
	e.root("POST", "/backings/cloud/buckets", map[string]any{"bucket": "nowhere", "container": "c-9"}).
		expectError(t, 400, codeInvalidRequest)
	// A failed creation after adding a container names the container.
	cloud.mu.Lock()
	cloud.createErr = fmt.Errorf("remote: %w", storage.ErrBucketUnavailable)
	cloud.mu.Unlock()
	msg = e.root("POST", "/backings/cloud/buckets", map[string]any{"bucket": "more", "newContainer": map[string]any{"bucket": "acme-more"}}).
		expectError(t, 503, codeInternalError)
	if !strings.Contains(msg, `"c-2"`) {
		t.Errorf("message = %q", msg)
	}
	cloud.mu.Lock()
	cloud.createErr = nil
	cloud.mu.Unlock()

	// The gos3 name is taken: refused before anything is done remotely.
	e.root("POST", "/backings/cloud/buckets", map[string]any{"bucket": "photos", "kind": "wrapped"}).expectError(t, 409, codeConflict)
	e.root("POST", "/backings/cloud/buckets", map[string]any{"bucket": "photos", "newContainer": map[string]any{"bucket": "acme-x"}}).
		expectError(t, 409, codeConflict)
	if len(cloud.added) != 2 {
		t.Errorf("containers added = %+v", cloud.added)
	}

	// Contradictions and invalid fields.
	for _, body := range []map[string]any{
		{"bucket": "xx1", "kind": "native", "container": "home"},
		{"bucket": "xx1", "kind": "wrapped", "remoteBucket": "acme-x"},
		{"bucket": "xx1", "kind": "wrapped", "adopt": true},
		{"bucket": "xx1", "remoteBucket": "acme-x", "container": "home"},
		{"bucket": "xx1", "adopt": true, "objectLock": true},
		{"bucket": "xx1", "container": "home", "newContainer": map[string]any{"bucket": "acme-x"}},
		{"bucket": "xx1", "kind": "other"},
		{"bucket": "xx1", "container": "Not Valid"},
		{"bucket": "xx1", "remoteBucket": "Not_valid_to_create"},
		{"bucket": "xx1", "region": "Bad Region"},
		{"bucket": "xx1", "newContainer": map[string]any{"bucket": ""}},
		{"bucket": "X1"},
		{"bucket": "xx1", "bogus": true},
		{},
	} {
		e.root("POST", "/backings/cloud/buckets", body).expectError(t, 400, codeInvalidRequest)
	}
	e.root("POST", "/backings/nope/buckets", map[string]any{"bucket": "xx1"}).expectError(t, 404, codeNotFound)
	e.root("POST", "/backings/default/buckets", map[string]any{"bucket": "xx1"}).expectError(t, 400, codeInvalidRequest)

	// The summaries report kind and location; memory buckets have none.
	var got bucketSummary
	e.root("GET", "/buckets/pics", nil).json(t, &got)
	if got.Kind != "native" || got.Location != "s3://acme-photos/" {
		t.Errorf("GET /buckets/pics = %+v", got)
	}
	if err := r.CreateBucket(ctx, "plain", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	res := e.root("GET", "/buckets/plain", nil)
	res.expectStatus(t, 200)
	if strings.Contains(string(res.body), `"kind"`) || strings.Contains(string(res.body), `"location"`) {
		t.Errorf("memory bucket summary = %s", res.body)
	}
}

func TestRemoteBackingAuthorization(t *testing.T) {
	e, _, cloud, cloud2 := newAccountEnv(t)
	ctx := context.Background()
	put := func(name, doc string) {
		t.Helper()
		pol, err := iam.ParsePolicy([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.iam.PutPolicy(ctx, name, "", *pol); err != nil {
			t.Fatal(err)
		}
	}
	put("browse-cloud", `{"Statement":[{"Effect":"Allow","Action":"admin:ListRemoteBuckets","Resource":"arn:gos3:admin:::backing/cloud"}]}`)
	put("team-adopt", `{"Statement":[
		{"Effect":"Allow","Action":"admin:CreateBucket","Resource":"arn:gos3:admin:::bucket/team-*"},
		{"Effect":"Allow","Action":"admin:AdoptRemoteBucket","Resource":"arn:gos3:admin:::backing/cloud"}]}`)
	put("containers", `{"Statement":[
		{"Effect":"Allow","Action":["admin:ManageContainers","admin:CreateRemoteBucket"],"Resource":"arn:gos3:admin:::backing/cloud"}]}`)
	put("browse-all", `{"Statement":[{"Effect":"Allow","Action":"admin:ListRemoteBuckets","Resource":"*"}]}`)

	// Browsing is granted per backing.
	browser := e.addUser("browser", "browse-cloud")
	e.do(&browser, "GET", Prefix+"/backings/cloud/remote-buckets", nil).expectStatus(t, 200)
	e.do(&browser, "GET", Prefix+"/backings/cloud/containers", nil).expectStatus(t, 200)
	msg := e.do(&browser, "GET", Prefix+"/backings/cloud2/remote-buckets", nil).expectError(t, 403, codeAccessDenied)
	if !strings.Contains(msg, "arn:gos3:admin:::backing/cloud2") {
		t.Errorf("message = %q", msg)
	}
	e.do(&browser, "POST", Prefix+"/backings/cloud/remote-buckets", map[string]any{"name": "acme-z"}).expectError(t, 403, codeAccessDenied)
	e.do(&browser, "POST", Prefix+"/backings/cloud/containers", map[string]any{"bucket": "acme-z"}).expectError(t, 403, codeAccessDenied)
	e.do(&browser, "GET", Prefix+"/backings", nil).expectError(t, 403, codeAccessDenied)
	if len(cloud2.listOpts) != 0 {
		t.Errorf("cloud2 was listed: %+v", cloud2.listOpts)
	}
	// whoami capabilities are evaluated at service level.
	var w whoAmI
	e.do(&browser, "GET", Prefix+"/whoami", nil).json(t, &w)
	if w.Capabilities != (capabilities{}) {
		t.Errorf("scoped capabilities = %+v", w.Capabilities)
	}
	all := e.addUser("browse-all", "browse-all")
	e.do(&all, "GET", Prefix+"/whoami", nil).json(t, &w)
	if want := (capabilities{Admin: true, BrowseRemote: true}); w.Capabilities != want {
		t.Errorf("capabilities = %+v", w.Capabilities)
	}

	// Creating on a backing: CreateBucket on the bucket plus the backing
	// action the body needs.
	team := e.addUser("team", "team-adopt")
	adopt := map[string]any{"bucket": "team-a", "remoteBucket": "acme-photos", "adopt": true}
	res := e.do(&team, "POST", Prefix+"/backings/cloud/buckets", adopt)
	res.expectStatus(t, 201)
	var s bucketSummary
	res.json(t, &s)
	if s.Owner != "team" || cloud.createdWith(t, "team-a").Owner != "team" {
		t.Errorf("owner = %q", s.Owner)
	}
	adopt["bucket"] = "other-a"
	msg = e.do(&team, "POST", Prefix+"/backings/cloud/buckets", adopt).expectError(t, 403, codeAccessDenied)
	if !strings.Contains(msg, "admin:CreateBucket on arn:gos3:admin:::bucket/other-a") {
		t.Errorf("message = %q", msg)
	}
	adopt["bucket"] = "team-b"
	msg = e.do(&team, "POST", Prefix+"/backings/cloud2/buckets", adopt).expectError(t, 403, codeAccessDenied)
	if !strings.Contains(msg, "admin:AdoptRemoteBucket on arn:gos3:admin:::backing/cloud2") {
		t.Errorf("message = %q", msg)
	}
	msg = e.do(&team, "POST", Prefix+"/backings/cloud/buckets", map[string]any{"bucket": "team-c", "kind": "native"}).
		expectError(t, 403, codeAccessDenied)
	if !strings.Contains(msg, "admin:CreateRemoteBucket") {
		t.Errorf("message = %q", msg)
	}
	msg = e.do(&team, "POST", Prefix+"/backings/cloud/buckets", map[string]any{"bucket": "team-c",
		"newContainer": map[string]any{"bucket": "acme-c"}}).expectError(t, 403, codeAccessDenied)
	if !strings.Contains(msg, "admin:ManageContainers") {
		t.Errorf("message = %q", msg)
	}
	// A wrapped bucket in an existing container needs nothing more.
	e.do(&team, "POST", Prefix+"/backings/cloud/buckets", map[string]any{"bucket": "team-d"}).expectStatus(t, 201)
	if len(cloud.added) != 0 {
		t.Errorf("containers added: %+v", cloud.added)
	}

	// Containers: ManageContainers plus the right to the location.
	cm := e.addUser("cm", "containers")
	e.do(&cm, "POST", Prefix+"/backings/cloud/containers", map[string]any{"bucket": "acme-existing"}).
		expectError(t, 403, codeAccessDenied)
	e.do(&cm, "POST", Prefix+"/backings/cloud/containers", map[string]any{"bucket": "acme-new", "createBucket": true}).
		expectStatus(t, 201)
	e.do(&cm, "PUT", Prefix+"/backings/cloud/containers/default", map[string]any{"id": "c-1"}).expectStatus(t, 200)
	e.do(&cm, "PUT", Prefix+"/backings/cloud2/containers/default", map[string]any{"id": "home"}).expectError(t, 403, codeAccessDenied)
	e.do(&cm, "DELETE", Prefix+"/backings/cloud2/containers/home", nil).expectError(t, 403, codeAccessDenied)
	e.do(&cm, "POST", Prefix+"/backings/cloud/buckets", map[string]any{"bucket": "cm-1"}).expectError(t, 403, codeAccessDenied)

	// The built-in diagnostics policy cannot browse remote buckets.
	diag := e.addUser("diag", iam.PolicyDiagnostics)
	e.do(&diag, "GET", Prefix+"/backings/cloud/remote-buckets", nil).expectError(t, 403, codeAccessDenied)
	e.do(&diag, "GET", Prefix+"/backings", nil).expectStatus(t, 200)
}

func TestReleaseBucket(t *testing.T) {
	e, r, cloud, _ := newAccountEnv(t)
	ctx := context.Background()
	for name, target := range map[string]*storage.BucketTarget{
		"native":  {Kind: storage.BucketKindNative, RemoteBucket: "acme-n"},
		"wrapped": {Kind: storage.BucketKindWrapped},
	} {
		if err := r.CreateBucket(ctx, name, storage.CreateBucketOptions{Backing: "cloud", Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateBucket(ctx, "plain", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}

	e.root("POST", "/buckets/native/release", nil).expectStatus(t, 204)
	if !slices.Equal(cloud.released, []string{"native"}) {
		t.Errorf("released = %v", cloud.released)
	}
	e.root("GET", "/buckets/native", nil).expectError(t, 404, codeNotFound)
	e.root("POST", "/buckets/native/release", nil).expectError(t, 404, codeNotFound)
	msg := e.root("POST", "/buckets/wrapped/release", nil).expectError(t, 400, codeInvalidRequest)
	if !strings.Contains(msg, "delete the bucket instead") {
		t.Errorf("message = %q", msg)
	}
	e.root("POST", "/buckets/plain/release", nil).expectError(t, 400, codeInvalidRequest)
	// Replicas first.
	e.root("PUT", "/buckets/plain/placement", map[string]any{"mode": "active-passive", "replicas": []string{"fast"}}).expectStatus(t, 200)
	msg = e.root("POST", "/buckets/plain/release", nil).expectError(t, 409, codeConflict)
	if !strings.Contains(msg, "remove replicas first") {
		t.Errorf("message = %q", msg)
	}

	// ReleaseBucket is evaluated on the bucket.
	put := func(name, doc string) {
		t.Helper()
		pol, err := iam.ParsePolicy([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.iam.PutPolicy(ctx, name, "", *pol); err != nil {
			t.Fatal(err)
		}
	}
	put("release-other", `{"Statement":[{"Effect":"Allow","Action":"admin:ReleaseBucket","Resource":"arn:gos3:admin:::bucket/other"}]}`)
	u := e.addUser("releaser", "release-other")
	e.do(&u, "POST", Prefix+"/buckets/wrapped/release", nil).expectError(t, 403, codeAccessDenied)
}

func TestBackingsAccountAndWhoAmI(t *testing.T) {
	e, _, _, _ := newAccountEnv(t)

	var bl struct {
		Backings []backingJSON `json:"backings"`
	}
	e.root("GET", "/backings", nil).json(t, &bl)
	if len(bl.Backings) != 4 {
		t.Fatalf("backings = %+v", bl.Backings)
	}
	def, cloud := bl.Backings[0], bl.Backings[2]
	if def.Account != nil || len(def.Kinds) != 0 || def.DefaultKind != "" {
		t.Errorf("memory backing = %+v", def)
	}
	if cloud.Name != "cloud" || !slices.Equal(cloud.Kinds, []string{"native", "wrapped"}) || cloud.DefaultKind != "wrapped" ||
		cloud.Location != "s3://acme-gos3/gos3/" {
		t.Errorf("s3 backing = %+v", cloud)
	}
	if a := cloud.Account; a == nil || a.Records != "home" || a.Home == nil || a.Home.Bucket != "acme-gos3" ||
		a.DefaultContainer != "home" || len(a.Containers) != 1 || a.Containers[0].Label != "shared" {
		t.Errorf("account = %+v", cloud.Account)
	}

	// whoami offers every caller the kinds and containers.
	nobody := e.addUser("nobody")
	res := e.do(&nobody, "GET", Prefix+"/whoami", nil)
	res.expectStatus(t, 200)
	var raw struct {
		Backings struct {
			Default   string            `json:"default"`
			Available []json.RawMessage `json:"available"`
		} `json:"backings"`
	}
	res.json(t, &raw)
	if raw.Backings.Default != "default" || len(raw.Backings.Available) != 4 {
		t.Fatalf("backings = %s", res.body)
	}
	if got := string(raw.Backings.Available[0]); got != `{"name":"default","kind":"memory"}` {
		t.Errorf("memory choice = %s", got)
	}
	if got := string(raw.Backings.Available[2]); got != `{"name":"cloud","kind":"s3","kinds":["native","wrapped"],"defaultKind":"wrapped",`+
		`"containers":[{"id":"home","label":"shared","default":true}]}` {
		t.Errorf("s3 choice = %s", got)
	}
}

func TestMoveIntoContainer(t *testing.T) {
	e, r, cloud, _ := newAccountEnv(t)
	ctx := context.Background()
	if err := r.CreateBucket(ctx, "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	e.root("POST", "/backings/cloud/containers", map[string]any{"bucket": "acme-archive"}).expectStatus(t, 201)

	e.root("POST", "/buckets/photos/placement/move", map[string]any{"to": "cloud", "container": "Bad!"}).
		expectError(t, 400, codeInvalidRequest)
	e.root("POST", "/buckets/photos/placement/move", map[string]any{"to": "fast", "container": "c-1"}).
		expectError(t, 400, codeInvalidRequest)
	e.root("POST", "/buckets/photos/placement/move", map[string]any{"to": "cloud", "container": "c-9"}).
		expectError(t, 400, codeInvalidRequest)

	var st multi.PlacementStatus
	res := e.root("POST", "/buckets/photos/placement/move", map[string]any{"to": "cloud", "container": "c-1"})
	res.expectStatus(t, http.StatusAccepted)
	res.json(t, &st)
	if st.Move == nil || st.Move.Target == nil || *st.Move.Target != (storage.BucketTarget{Kind: storage.BucketKindWrapped, Container: "c-1"}) {
		t.Fatalf("move = %+v", st.Move)
	}
	waitFor(t, func() bool {
		st, _ := r.Placement("photos")
		return st.Move == nil && st.Primary == "cloud"
	})
	opts := cloud.createdWith(t, "photos")
	if !opts.Import || opts.Target == nil || opts.Target.Container != "c-1" {
		t.Errorf("move created %+v (target %+v)", opts, opts.Target)
	}
}

func TestStorageErrorMapping(t *testing.T) {
	a := newEnv(t).api
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{storage.ErrBucketUnavailable, 503, codeInternalError},
		{fmt.Errorf("w: %w", storage.ErrRemoteBucketExists), 409, codeConflict},
		{storage.ErrBucketExists, 409, codeConflict},
		{storage.ErrInvalidTarget, 400, codeInvalidRequest},
		{storage.ErrRemoteDenied, 400, codeInvalidRequest},
		{storage.ErrRemoteRejected, 400, codeInvalidRequest},
		{storage.ErrNotSupported, 400, codeInvalidRequest},
		{storage.ErrInvalidBacking, 404, codeNotFound},
		{s3store.ErrPrefixNotEmpty, 400, codeInvalidRequest},
		{s3store.ErrMarkerMismatch, 400, codeInvalidRequest},
		{s3store.ErrMarkerMissing, 400, codeInvalidRequest},
		{s3store.ErrUnmanagedPrefix, 400, codeInvalidRequest},
		{s3store.ErrWrongKEK, 400, codeInvalidRequest},
		{s3store.ErrInUse, 409, codeConflict},
		{s3store.ErrFenced, 409, codeConflict},
		{fmt.Errorf("%w: backing %q uses it", s3account.ErrClaimed, "other"), 409, codeConflict},
		{fmt.Errorf("%w: busy", multi.ErrConflict), 409, codeConflict},
	} {
		got := a.toAPIError(c.err)
		if got.Status != c.status || got.Code != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.err, got.Status, got.Code, c.status, c.code)
		}
	}
	if got := a.toAPIError(storage.ErrRemoteBucketExists); !strings.Contains(got.Message, "adopt") || strings.HasPrefix(got.Message, "storage:") {
		t.Errorf("message = %q", got.Message)
	}
}

func TestRefinesPath(t *testing.T) {
	for _, c := range []struct {
		p, q string
		want bool
	}{
		{"/backings/{name}/containers/default", "/backings/{name}/containers/{id}", true},
		{"/backings/{name}/containers/{id}", "/backings/{name}/containers/default", false},
		{"/backings/{name}/containers/{id}", "/backings/{x}/containers/{id}", false},
		{"/backings/config", "/backings/{name}/containers", false},
		{"/buckets/{name}", "/users/{name}", false},
	} {
		if got := refinesPath(c.p, c.q); got != c.want {
			t.Errorf("refinesPath(%q, %q) = %v", c.p, c.q, got)
		}
	}
}
