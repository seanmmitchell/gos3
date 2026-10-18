package s3account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// The remote manager (RemoteManager): the remote bucket browser of the
// admin API and the management of containers. Its methods do remote I/O
// without the multi router's locks; creating buckets goes through the
// router.

// markerKey is the marker object of a gos3 store (s3store) at the root of
// its prefix.
const markerKey = "gos3.json"

// Bounds of the remote checks.
const (
	// adoptProbePrefixes is how many top-level prefixes of a remote bucket
	// CheckAdoptable looks into for a gos3 store (documented limit).
	adoptProbePrefixes = 100
	// probeParallelism bounds the concurrent HEAD requests of probes.
	probeParallelism = 8
	// addContainerWait bounds how long AddContainer waits for a new
	// container to load.
	addContainerWait = 30 * time.Second
)

// Remote bucket statuses (RemoteBucket.Status) and use roles
// (RemoteUse.Role).
const (
	remoteNative       = "native"
	remoteHome         = "home"
	remoteContainer    = "container"
	remoteClaimed      = "claimed"
	remoteGos3         = "gos3"
	remoteInaccessible = "inaccessible"
	remoteUnmanaged    = "unmanaged"
)

// remoteError maps a failure of the remote service to the storage errors
// of the admin API.
func (b *Backend) remoteError(what string, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case s3client.IsNotImplemented(err):
		return fmt.Errorf("s3account %q: %s: %w: %w", b.name, what, storage.ErrNotSupported, err)
	case s3client.IsUnavailable(err):
		return fmt.Errorf("s3account %q: %s: %w: %w", b.name, what, storage.ErrBackingUnavailable, err)
	case s3client.IsAccessDenied(err):
		return fmt.Errorf("s3account %q: %s: %w: %w", b.name, what, storage.ErrRemoteDenied, err)
	}
	var ce *s3client.Error
	if errors.As(err, &ce) && ce.StatusCode >= 400 && ce.StatusCode < 500 {
		return fmt.Errorf("s3account %q: %s: %w: %w", b.name, what, storage.ErrRemoteRejected, err)
	}
	return fmt.Errorf("s3account %q: %s: %w", b.name, what, err)
}

// client returns the client of the current connection.
func (b *Backend) client() *s3client.Client { return b.conn.Load().Client }

// --- the browser ---

// RemoteBuckets lists a page of the remote service's buckets with their
// uses. Credentials that may not list buckets yield Complete false and the
// remote buckets the backing knows.
func (b *Backend) RemoteBuckets(ctx context.Context, opts ListRemoteOptions) (RemoteBucketList, error) {
	if err := b.serving(ctx); err != nil {
		return RemoteBucketList{}, err
	}
	page, err := b.client().ListBuckets(ctx, s3client.ListBucketsInput{
		Prefix: opts.Prefix, ContinuationToken: opts.ContinuationToken, MaxBuckets: opts.MaxBuckets,
	})
	if err != nil {
		if s3client.IsAccessDenied(err) {
			return RemoteBucketList{Buckets: b.knownRemoteBuckets(opts.Prefix), Complete: false, ListError: err.Error()}, nil
		}
		return RemoteBucketList{}, b.remoteError("list the remote buckets", err)
	}
	out := RemoteBucketList{Buckets: []RemoteBucket{}, Complete: true, ContinuationToken: page.ContinuationToken}
	for _, e := range page.Buckets {
		if !strings.HasPrefix(e.Name, opts.Prefix) {
			continue // a service ignoring the prefix
		}
		out.Buckets = append(out.Buckets, b.describeRemote(e.Name, e.Region, e.CreationDate))
	}
	if opts.Probe {
		b.probeRemotes(ctx, out.Buckets)
	}
	return out, nil
}

// knownRemoteBuckets describes the remote buckets the backing uses (home,
// containers, native buckets) whose names have the prefix.
func (b *Backend) knownRemoteBuckets(prefix string) []RemoteBucket {
	b.mu.RLock()
	names := map[string]string{} // lower-case -> name
	add := func(name string) {
		if strings.HasPrefix(name, prefix) {
			names[strings.ToLower(name)] = name
		}
	}
	if b.home != nil {
		add(b.opts.Home.Bucket)
	}
	if b.reg != nil {
		for _, c := range b.reg.containers {
			add(c.Bucket)
		}
		for _, e := range b.reg.buckets {
			if e.Kind == storage.BucketKindNative {
				add(e.Remote)
			}
		}
	}
	b.mu.RUnlock()
	out := make([]RemoteBucket, 0, len(names))
	for _, name := range names {
		out = append(out, b.describeRemote(name, "", time.Time{}))
	}
	slices.SortFunc(out, func(x, y RemoteBucket) int { return strings.Compare(x.Name, y.Name) })
	return out
}

// describeRemote returns a remote bucket with its uses and status.
func (b *Backend) describeRemote(name, region string, created time.Time) RemoteBucket {
	rb := RemoteBucket{Name: name, Region: region, CreatedAt: created}
	b.mu.RLock()
	if b.home != nil && strings.EqualFold(b.opts.Home.Bucket, name) {
		rb.Uses = append(rb.Uses, RemoteUse{Role: remoteHome, Backing: b.name, Prefix: b.opts.Home.Prefix, ContainerID: homeID})
	}
	if b.reg != nil {
		for _, c := range sortedContainers(b.reg.containers) {
			if strings.EqualFold(c.Bucket, name) {
				rb.Uses = append(rb.Uses, RemoteUse{Role: remoteContainer, Backing: b.name, Prefix: c.Prefix, ContainerID: c.ID})
			}
		}
		for gos, e := range b.reg.buckets {
			if e.Kind == storage.BucketKindNative && strings.EqualFold(e.Remote, name) {
				rb.Uses = append(rb.Uses, RemoteUse{Role: remoteNative, Backing: b.name, Bucket: gos, Created: e.Created})
			}
		}
	}
	b.mu.RUnlock()
	for _, c := range b.holders(name) {
		if c.Backing == b.name {
			continue
		}
		rb.Uses = append(rb.Uses, RemoteUse{Role: remoteClaimed, Backing: c.Backing, Prefix: c.Prefix, Bucket: c.GosBucket})
	}
	rb.Status = remoteUnmanaged
	for _, role := range []string{remoteNative, remoteHome, remoteContainer, remoteClaimed} {
		if slices.ContainsFunc(rb.Uses, func(u RemoteUse) bool { return u.Role == role }) {
			rb.Status = role
			break
		}
	}
	switch rb.Status {
	case remoteNative:
		for _, u := range rb.Uses {
			if u.Role == remoteNative {
				rb.Note = fmt.Sprintf("native bucket %q", u.Bucket)
				break
			}
		}
	case remoteClaimed:
		rb.Note = fmt.Sprintf("used by backing %q", rb.Uses[0].Backing)
	}
	return rb
}

// sortedContainers returns container records sorted by ID.
func sortedContainers(m map[string]containerRecord) []containerRecord {
	out := make([]containerRecord, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, c containerRecord) int { return strings.Compare(a.ID, c.ID) })
	return out
}

// probeRemotes looks for a gos3 store at the root of each unmanaged
// bucket (one HEAD each, concurrently).
func (b *Backend) probeRemotes(ctx context.Context, buckets []RemoteBucket) {
	sem := make(chan struct{}, probeParallelism)
	var wg sync.WaitGroup
	for i := range buckets {
		rb := &buckets[i]
		if rb.Status != remoteUnmanaged {
			continue
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			found, err := b.hasStore(ctx, b.client().ForRegion(rb.Region), rb.Name, "")
			switch {
			case found:
				rb.Status, rb.Note = remoteGos3, "holds a gos3 store ("+markerKey+" at the root)"
				rb.Uses = append(rb.Uses, RemoteUse{Role: remoteGos3})
			case s3client.IsAccessDenied(err):
				rb.Status, rb.Note = remoteInaccessible, "the backing's credentials cannot read it"
			case err != nil && !s3client.IsUnavailable(err):
				rb.Status, rb.Note = remoteInaccessible, err.Error()
			case err != nil:
				rb.Note = "probe failed: " + err.Error()
			}
		})
	}
	wg.Wait()
}

// hasStore reports whether bucket holds a gos3 store at prefix (its
// marker). A wrong-region answer naming the region is retried there.
func (b *Backend) hasStore(ctx context.Context, c *s3client.Client, bucket, prefix string) (bool, error) {
	_, err := c.HeadObjectWith(ctx, bucket, prefix+markerKey, s3client.HeadOptions{})
	if region, wrong := s3client.IsWrongRegion(err); wrong && region != "" && region != c.Region() {
		_, err = c.ForRegion(region).HeadObjectWith(ctx, bucket, prefix+markerKey, s3client.HeadOptions{})
	}
	if err == nil {
		return true, nil
	}
	if _, marker := s3client.IsDeleteMarker(err); marker || s3client.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// checkNoStore refuses (ErrClaimed) a location holding a gos3 store.
func (b *Backend) checkNoStore(ctx context.Context, c *s3client.Client, bucket, prefix string) error {
	found, err := b.hasStore(ctx, c, bucket, prefix)
	switch {
	case err != nil:
		return b.remoteError(fmt.Sprintf("look for a gos3 store at s3://%s/%s", bucket, prefix), err)
	case found:
		return fmt.Errorf("%w: s3://%s/%s holds a gos3 store (%s)", ErrClaimed, bucket, prefix, markerKey)
	}
	return nil
}

// bucketRegion returns the region of an existing remote bucket; a missing
// one is storage.ErrInvalidTarget.
func (b *Backend) bucketRegion(ctx context.Context, bucket string) (string, error) {
	region, err := b.client().BucketRegion(ctx, bucket)
	switch {
	case err == nil:
		return region, nil
	case s3client.IsNotFound(err):
		return "", fmt.Errorf("%w: the remote bucket %q does not exist", storage.ErrInvalidTarget, bucket)
	}
	return "", b.remoteError(fmt.Sprintf("find the region of remote bucket %q", bucket), err)
}

// CheckAdoptable checks that remote can become a native bucket: no use of
// it is claimed (by any backing), it exists, and it holds no gos3 store at
// its root or in one of its first 100 top-level prefixes. It returns the
// bucket's region.
func (b *Backend) CheckAdoptable(ctx context.Context, remote string) (string, error) {
	if err := b.serving(ctx); err != nil {
		return "", err
	}
	if remote == "" {
		return "", fmt.Errorf("%w: a remote bucket name is required", storage.ErrInvalidTarget)
	}
	if hs := b.holders(remote); len(hs) > 0 {
		return "", fmt.Errorf("%w: the remote bucket %q is used by backing %q %s", ErrClaimed, remote, hs[0].Backing, useOf(hs[0]))
	}
	region, err := b.bucketRegion(ctx, remote)
	if err != nil {
		return "", err
	}
	c := b.client().ForRegion(region)
	if err := b.checkNoStore(ctx, c, remote, ""); err != nil {
		return "", err
	}
	var prefixes []string
	in := s3client.ListInput{Delimiter: "/", MaxKeys: 1000}
	for range 10 {
		out, err := c.ListObjectsV2(ctx, remote, in)
		if err != nil {
			return "", b.remoteError(fmt.Sprintf("list remote bucket %q", remote), err)
		}
		prefixes = append(prefixes, out.CommonPrefixes...)
		if len(prefixes) >= adoptProbePrefixes || !out.IsTruncated || out.NextContinuationToken == "" {
			break
		}
		in.ContinuationToken = out.NextContinuationToken
	}
	if len(prefixes) > adoptProbePrefixes {
		prefixes = prefixes[:adoptProbePrefixes]
	}
	errs := make([]error, len(prefixes))
	sem := make(chan struct{}, probeParallelism)
	var wg sync.WaitGroup
	for i, p := range prefixes {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			errs[i] = b.checkNoStore(ctx, c, remote, p)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return "", err
	}
	return region, nil
}

// CreateRemoteBucket creates an unmanaged remote bucket in region ("" =
// the connection's). An existing bucket is storage.ErrRemoteBucketExists.
func (b *Backend) CreateRemoteBucket(ctx context.Context, name, region string, objectLock bool) error {
	if err := b.serving(ctx); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("%w: a remote bucket name is required", storage.ErrInvalidTarget)
	}
	c := b.client()
	err := c.ForRegion(region).HeadBucket(ctx, name)
	_, wrongRegion := s3client.IsWrongRegion(err)
	switch {
	case err == nil, s3client.IsAccessDenied(err), wrongRegion:
		return fmt.Errorf("s3account %q: the remote bucket %q exists: %w", b.name, name, storage.ErrRemoteBucketExists)
	case !s3client.IsNotFound(err):
		return b.remoteError(fmt.Sprintf("look for remote bucket %q", name), err)
	}
	err = c.CreateBucketWith(ctx, name, s3client.CreateBucketOptions{Region: region, ObjectLock: objectLock})
	switch {
	case err == nil:
		b.log.Info("s3account: created a remote bucket", "backing", b.name, "remote", name, "region", region, "objectLock", objectLock)
		return nil
	case s3client.IsCode(err, "BucketAlreadyOwnedByYou"), s3client.IsCode(err, "BucketAlreadyExists"):
		return fmt.Errorf("s3account %q: the remote bucket %q exists: %w", b.name, name, storage.ErrRemoteBucketExists)
	case s3client.IsCode(err, "InvalidBucketName"):
		return fmt.Errorf("%w: the remote service refuses the bucket name %q: %w", storage.ErrInvalidTarget, name, err)
	}
	return b.remoteError(fmt.Sprintf("create remote bucket %q", name), err)
}

// --- containers ---

// Containers lists the containers, the home first.
func (b *Backend) Containers(ctx context.Context) []ContainerInfo { return b.containerInfos() }

// containerInfos describes the containers, the home first, then by the
// time they were added.
func (b *Backend) containerInfos() []ContainerInfo {
	b.mu.RLock()
	cs := make([]*container, 0, len(b.containers))
	for _, c := range b.containers {
		cs = append(cs, c)
	}
	counts := map[string]int{}
	if b.reg != nil {
		for _, e := range b.reg.buckets {
			if e.Kind == storage.BucketKindWrapped {
				counts[e.Container]++
			}
		}
	}
	def := b.defaultContainer()
	labels := map[string]string{}
	if b.reg != nil {
		for id, rec := range b.reg.containers {
			labels[id] = rec.Label
		}
	}
	b.mu.RUnlock()
	out := make([]ContainerInfo, 0, len(cs))
	for _, c := range cs {
		state, why := c.state()
		out = append(out, ContainerInfo{
			ID: c.id, Label: labels[c.id], Location: c.location(), Region: c.rec.Region, Role: c.rec.Role,
			Default: c.id == def, Buckets: counts[c.id], State: state, Error: why, Added: c.rec.Added,
		})
	}
	slices.SortFunc(out, func(x, y ContainerInfo) int {
		switch {
		case x.ID == homeID:
			return -1
		case y.ID == homeID:
			return 1
		}
		if c := x.Added.Compare(y.Added); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

// containerInfo describes one container.
func (b *Backend) containerInfo(id string) ContainerInfo {
	for _, ci := range b.containerInfos() {
		if ci.ID == id {
			return ci
		}
	}
	return ContainerInfo{ID: id}
}

// normalizePrefix trims p and makes a non-empty prefix end in "/".
func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// ancestorPrefixes returns the prefixes a gos3 store at prefix would be
// nested in: "" and each parent ("a/b/c/" -> "", "a/", "a/b/").
func ancestorPrefixes(prefix string) []string {
	out := []string{""}
	for i := 0; i < len(prefix)-1; i++ {
		if prefix[i] == '/' {
			out = append(out, prefix[:i+1])
		}
	}
	if prefix == "" {
		return nil
	}
	return out
}

// newContainerID returns an unused container ID (mu held).
func (b *Backend) newContainerID() string {
	for {
		var r [4]byte
		_, _ = rand.Read(r[:])
		id := "c" + hex.EncodeToString(r[:])
		if _, ok := b.containers[id]; !ok {
			if _, ok := b.reg.containers[id]; !ok {
				return id
			}
		}
	}
}

// AddContainer registers a container at spec's location and waits (at
// most 30s) until it has loaded. The location is claimed (it must not
// overlap the home, another container or a native bucket's remote bucket
// of any backing), must not be nested in a gos3 store, and must be empty
// (a zero-byte folder placeholder aside), unless spec.Attach adopts the
// gos3 store already there (a container detached from another server),
// whose buckets become wrapped buckets of the backing. With
// spec.CreateBucket the remote bucket is created first.
func (b *Backend) AddContainer(ctx context.Context, spec ContainerSpec) (ContainerInfo, error) {
	if err := b.serving(ctx); err != nil {
		return ContainerInfo{}, err
	}
	spec.Bucket, spec.Prefix = strings.TrimSpace(spec.Bucket), normalizePrefix(spec.Prefix)
	switch {
	case spec.Bucket == "":
		return ContainerInfo{}, fmt.Errorf("%w: a container needs a remote bucket", storage.ErrInvalidTarget)
	case spec.CreateBucket && spec.Attach:
		return ContainerInfo{}, fmt.Errorf("%w: a new remote bucket holds no gos3 store to attach", storage.ErrInvalidTarget)
	}
	if err := validPrefix(spec.Prefix); err != nil {
		return ContainerInfo{}, fmt.Errorf("%w: %w", storage.ErrInvalidTarget, err)
	}
	b.mu.RLock()
	n := len(b.containers)
	b.mu.RUnlock()
	if n >= maxContainers {
		return ContainerInfo{}, fmt.Errorf("%w: s3 backing %q has %d containers, the most it can have", storage.ErrInvalidTarget, b.name, n)
	}
	loc := Location{Bucket: spec.Bucket, Prefix: spec.Prefix}
	release, err := b.claim(loc, RoleContainer, "")
	if err != nil {
		return ContainerInfo{}, err
	}
	created := false
	fail := func(err error) (ContainerInfo, error) {
		if release != nil {
			release()
		}
		if created {
			if derr := b.client().ForRegion(spec.Region).DeleteBucket(context.WithoutCancel(ctx), spec.Bucket); derr != nil {
				b.log.Warn("s3account: delete the remote bucket created for a container that could not be added; it stays unmanaged",
					"backing", b.name, "remote", spec.Bucket, "err", derr)
			}
		}
		return ContainerInfo{}, err
	}
	region := spec.Region
	if spec.CreateBucket {
		if err := b.CreateRemoteBucket(ctx, spec.Bucket, region, false); err != nil {
			return fail(err)
		}
		created = true
	} else {
		r, err := b.bucketRegion(ctx, spec.Bucket)
		if err != nil {
			return fail(err)
		}
		if r != "" {
			region = r
		}
	}
	rc := b.client().ForRegion(region)
	for _, p := range ancestorPrefixes(spec.Prefix) {
		if err := b.checkNoStore(ctx, rc, spec.Bucket, p); err != nil {
			return fail(fmt.Errorf("%w (s3://%s/%s would be nested in it)", err, spec.Bucket, spec.Prefix))
		}
	}
	m, _, err := s3store.ReadMarker(ctx, rc, spec.Bucket, spec.Prefix)
	hasMarker := err == nil
	if err != nil && !s3client.IsNotFound(err) {
		return fail(b.remoteError("read "+loc.String()+markerKey, err))
	}
	switch {
	case hasMarker && !spec.Attach:
		return fail(fmt.Errorf("%w: %s already holds a gos3 store; attach it to use it", ErrClaimed, loc))
	case !hasMarker && spec.Attach:
		return fail(fmt.Errorf("%w: %s holds no gos3 store to attach", storage.ErrInvalidTarget, loc))
	case hasMarker && m.Gateway:
		return fail(fmt.Errorf("%w: %s is the metadata of a legacy gateway backing; configure it as a backing's home to migrate it",
			storage.ErrInvalidTarget, loc))
	case hasMarker && b.knownBackingID(m.BackingID):
		return fail(fmt.Errorf("%w: the gos3 store at %s is already used by this backing (a copy of one of its containers?)",
			ErrClaimed, loc))
	}
	mode, expect := s3store.CreateIfEmpty, ""
	if spec.Attach {
		mode, expect = s3store.MustExist, m.BackingID
	}
	role := containerRoleExisting
	if spec.CreateBucket {
		role = containerRoleDedicated
	}
	b.mu.RLock()
	id := b.newContainerID()
	b.mu.RUnlock()
	rec := containerRecord{
		ID: id, Bucket: spec.Bucket, Prefix: spec.Prefix, Region: region, Role: role, CreatedBucket: created,
		Label: strings.TrimSpace(spec.Label), Added: time.Now().UTC(),
	}
	c := b.openContainer(rec, mode, expect, release)
	release = nil // the container's now
	if c.failErr != nil {
		return fail(fmt.Errorf("%w: %w", storage.ErrInvalidTarget, c.failErr))
	}
	wctx, cancel := context.WithTimeout(ctx, addContainerWait)
	err = waitLoaded(wctx, c.store)
	cancel()
	if err != nil {
		_ = closeContainer(c)
		return fail(b.addContainerError(loc, err))
	}
	c.rec.BackingID = c.store.Marker().BackingID
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if b.closed.Load() {
		_ = closeContainer(c)
		return fail(b.unavailable())
	}
	if cur := b.conn.Load(); c.conn != cur {
		// A connection change was committed while the container loaded.
		if c = b.switchContainer(ctx, c, cur); c.failErr != nil {
			return fail(fmt.Errorf("%w: %w", storage.ErrInvalidTarget, c.failErr))
		}
	}
	if err := b.putContainerRecord(ctx, c.rec); err != nil {
		_ = closeContainer(c)
		return fail(err)
	}
	b.mu.Lock()
	b.containers[id] = c
	b.mu.Unlock()
	b.log.Info("s3account: added a container", "backing", b.name, "container", id, "location", loc.String(),
		"attached", spec.Attach, "createdBucket", created)
	if !c.loaded() {
		b.watchContainer(c) // reopened with the new connection: reconciled once loaded
	} else if err := b.reconcileContainer(ctx, c); err != nil {
		b.log.Warn("s3account: reconciling a new container failed (retried at the next start)", "backing", b.name,
			"container", id, "err", err)
	}
	return b.containerInfo(id), nil
}

// knownBackingID reports whether id is the backing ID of the home or of a
// registered container.
func (b *Backend) knownBackingID(id string) bool {
	if id == "" {
		return false
	}
	if b.home != nil && b.home.Marker().BackingID == id {
		return true
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, c := range b.reg.containers {
		if c.BackingID == id {
			return true
		}
	}
	return false
}

// waitLoaded waits until st has loaded; it gives up when the load fails
// (st keeps retrying, but the caller reports the reason) or ctx ends.
func waitLoaded(ctx context.Context, st *s3store.Backend) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-st.Ready():
			return nil
		case <-ctx.Done():
			if err := st.Status().Err; err != nil {
				return err
			}
			return fmt.Errorf("%w: not loaded in time (%s)", storage.ErrBackingUnavailable, st.Status().State)
		case <-tick.C:
			if s := st.Status(); s.State == s3store.StateFailed && s.Err != nil {
				return s.Err
			}
		}
	}
}

// addContainerError classifies why a new container did not load: settings
// that must change are storage.ErrInvalidTarget, refusals of the remote
// service storage.ErrRemoteDenied/ErrRemoteRejected, outages
// storage.ErrBackingUnavailable.
func (b *Backend) addContainerError(loc Location, err error) error {
	for _, e := range []error{s3store.ErrInUse, s3store.ErrWrongKEK, s3store.ErrMarkerMismatch, s3store.ErrUnmanagedPrefix,
		s3store.ErrPrefixNotEmpty, s3store.ErrNoConditionalWrites, s3store.ErrCorruptJournal} {
		if errors.Is(err, e) {
			return fmt.Errorf("%w: container %s: %w", storage.ErrInvalidTarget, loc, err)
		}
	}
	var ce *s3client.Error
	if errors.As(err, &ce) && !errors.Is(err, storage.ErrBackingUnavailable) {
		return b.remoteError("container "+loc.String(), err)
	}
	if errors.Is(err, storage.ErrBackingUnavailable) {
		return err
	}
	return fmt.Errorf("%w: container %s: %w", storage.ErrBackingUnavailable, loc, err)
}

// RemoveContainer forgets a container that holds no bucket; nothing is
// deleted (the remote bucket and the gos3 store in it stay, and can be
// attached again). The home and the default container cannot be removed.
func (b *Backend) RemoveContainer(ctx context.Context, id string) error {
	if err := b.serving(ctx); err != nil {
		return err
	}
	if id == homeID {
		return fmt.Errorf("%w: the home cannot be removed", storage.ErrInvalidTarget)
	}
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	b.mu.RLock()
	_, registered := b.reg.containers[id]
	c := b.containers[id]
	var holds []string
	for name, e := range b.reg.buckets {
		if e.Kind == storage.BucketKindWrapped && e.Container == id {
			holds = append(holds, name)
		}
	}
	isDefault := b.reg.defaultContainer == id
	b.mu.RUnlock()
	slices.Sort(holds)
	switch {
	case !registered:
		return fmt.Errorf("%w: s3 backing %q has no container %q", storage.ErrInvalidTarget, b.name, id)
	case len(holds) > 0:
		return fmt.Errorf("%w: container %s holds the buckets %s; delete or move them first", storage.ErrBucketNotEmpty, id, strings.Join(holds, ", "))
	case isDefault:
		return fmt.Errorf("%w: container %s is the default container; make another one the default first", storage.ErrInvalidTarget, id)
	}
	if c != nil && c.loaded() {
		list, err := c.store.ListBuckets(ctx)
		if err != nil {
			return c.classify(err)
		}
		if len(list) > 0 {
			return fmt.Errorf("%w: container %s holds %d buckets", storage.ErrBucketNotEmpty, id, len(list))
		}
	}
	if err := b.records.DeleteRecords(ctx, containerKey(id)); err != nil {
		return fmt.Errorf("s3account %q: delete the record of container %s: %w", b.name, id, err)
	}
	b.mu.Lock()
	delete(b.reg.containers, id)
	delete(b.containers, id)
	b.mu.Unlock()
	if c != nil {
		if err := closeContainer(c); err != nil {
			b.log.Warn("s3account: close a removed container", "backing", b.name, "container", id, "err", err)
		}
	}
	b.log.Info("s3account: removed a container", "backing", b.name, "container", id)
	return nil
}

// SetDefaultContainer makes a container the default of new wrapped
// buckets.
func (b *Backend) SetDefaultContainer(ctx context.Context, id string) error {
	if err := b.serving(ctx); err != nil {
		return err
	}
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	b.mu.RLock()
	_, ok := b.reg.containers[id]
	if id == homeID {
		ok = b.home != nil
	}
	cur := b.defaultContainer()
	b.mu.RUnlock()
	switch {
	case !ok:
		return fmt.Errorf("%w: s3 backing %q has no container %q", storage.ErrInvalidTarget, b.name, id)
	case cur == id:
		return nil
	}
	return b.putDefaultContainer(ctx, id)
}

// Info summarises the backing.
func (b *Backend) Info() AccountInfo {
	b.mu.RLock()
	defer b.mu.RUnlock()
	info := AccountInfo{
		Records:          b.recordsWhere(),
		DefaultKind:      b.effectiveDefaultKind(),
		DefaultContainer: b.defaultContainer(),
		Kinds:            []storage.BucketKind{storage.BucketKindNative},
	}
	if b.hasContainers() {
		info.Kinds = []storage.BucketKind{storage.BucketKindWrapped, storage.BucketKindNative}
	}
	if b.opts.Home != nil {
		h := *b.opts.Home
		info.Home = &h
	}
	if b.reg != nil {
		for _, e := range b.reg.buckets {
			if e.Kind == storage.BucketKindNative {
				info.NativeBuckets++
			} else {
				info.WrappedBuckets++
			}
		}
	}
	return info
}
