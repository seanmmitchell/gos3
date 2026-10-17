package s3account

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// Connection changes
//
// PrepareConnection verifies a new connection without changing anything:
// first read-only checks of every store's marker (reachable with the new
// settings, the same backing, a KEK that matches its check value) and, for
// a backing keeping its records locally, of the credentials; then each
// store's own preparation. Only the commit changes the backing, under
// adminMu: it switches the client the backing opens stores with, commits
// the prepared stores and brings every other container to the new
// connection (one that loaded or was added since the preparation is
// prepared and committed then, one that has not loaded is reopened), and
// switches the native store. Each container records the connection it
// uses (container.conn), so a container added while a commit runs is
// switched when AddContainer registers it.

// maxNativeChecks bounds the native remote buckets PrepareConnection
// checks for a backing without a home.
const maxNativeChecks = 20

// switchTimeout bounds the preparation of a container at commit time.
const switchTimeout = 30 * time.Second

// containerConn returns conn for a store in region.
func containerConn(conn *s3store.Connection, region string) s3store.Connection {
	cc := *conn
	cc.Client = conn.Client.ForRegion(region)
	if region != "" {
		cc.Region = region
	}
	return cc
}

// PrepareConnection verifies that conn (new endpoint, credentials,
// addressing, part size or KEK) reaches the backing's data: the home's
// marker and KEK, each loaded container's through its region's client,
// and, for a backing keeping its records locally, that the credentials are
// accepted (the service answers, and up to 20 native buckets' remote
// buckets can be reached). A failed preparation leaves the backing as it
// was. commit switches the backing to conn: the home, every container and
// the native store.
func (b *Backend) PrepareConnection(ctx context.Context, conn s3store.Connection) (commit func(), err error) {
	if conn.Client == nil {
		conn.Client, err = s3client.New(s3client.Config{
			Endpoint: conn.Endpoint, Region: conn.Region, AccessKey: conn.AccessKey,
			SecretKey: conn.SecretKey, PathStyle: conn.PathStyle,
		})
		if err != nil {
			return nil, err
		}
	}
	if conn.Region == "" {
		conn.Region = conn.Client.Region()
	}
	if len(conn.KEK) != 0 && len(conn.KEK) != crypt.KeySize {
		return nil, fmt.Errorf("s3account: the encryption key has %d bytes, want %d", len(conn.KEK), crypt.KeySize)
	}
	if conn.Endpoint == "" && conn.AccessKey == "" {
		// A bare client: keep the service description restic uses.
		cur := b.conn.Load()
		conn.Endpoint, conn.AccessKey, conn.SecretKey, conn.PathStyle = cur.Endpoint, cur.AccessKey, cur.SecretKey, cur.PathStyle
	}
	next := &conn
	b.adminMu.Lock()
	defer b.adminMu.Unlock()
	if b.closed.Load() {
		return nil, b.unavailable()
	}
	// The stores to verify: the home and the loaded containers.
	b.mu.RLock()
	var stores []*container
	for _, c := range b.containers {
		if c.home || c.loaded() {
			stores = append(stores, c)
		}
	}
	var natives []bucketRecord
	if b.reg != nil {
		for _, e := range b.reg.buckets {
			if e.Kind == storage.BucketKindNative && e.claimErr == nil {
				natives = append(natives, e.bucketRecord)
			}
		}
	}
	b.mu.RUnlock()
	slices.SortFunc(stores, func(x, y *container) int {
		switch {
		case x.home:
			return -1
		case y.home:
			return 1
		}
		return strings.Compare(x.id, y.id)
	})
	// Read-only checks first: a store's own preparation may record the
	// KEK's check value in its marker, which must not happen unless every
	// store accepts the connection.
	for _, c := range stores {
		if err := b.checkMarker(ctx, c, containerConn(next, c.rec.Region)); err != nil {
			return nil, err
		}
	}
	if b.home == nil {
		if err := b.checkCredentials(ctx, conn.Client, natives); err != nil {
			return nil, err
		}
	}
	prepared := map[*container]func(){}
	for _, c := range stores {
		commit, err := c.store.PrepareConnection(ctx, containerConn(next, c.rec.Region))
		if err != nil {
			return nil, fmt.Errorf("s3account %q: %s: %w", b.name, storeName(c), err)
		}
		prepared[c] = commit
	}
	return func() {
		b.adminMu.Lock()
		defer b.adminMu.Unlock()
		if b.closed.Load() {
			return
		}
		b.conn.Store(next)
		b.mu.RLock()
		live := make([]*container, 0, len(b.containers))
		for _, c := range b.containers {
			live = append(live, c)
		}
		b.mu.RUnlock()
		ctx, cancel := context.WithTimeout(b.ctx, switchTimeout)
		defer cancel()
		for _, c := range live {
			if commit, ok := prepared[c]; ok {
				commit()
				c.conn = next
				continue
			}
			b.publishSwitched(c, b.switchContainer(ctx, c, next))
		}
		b.switchNative(next)
	}, nil
}

// storeName names a container in errors.
func storeName(c *container) string {
	if c.home {
		return "home " + c.locationString()
	}
	return fmt.Sprintf("container %s (%s)", c.id, c.locationString())
}

// checkMarker verifies, without writing anything, that c's marker can be
// read with cc, names the backing c loaded, and accepts cc's KEK.
func (b *Backend) checkMarker(ctx context.Context, c *container, cc s3store.Connection) error {
	m, _, err := s3store.ReadMarker(ctx, cc.Client, c.rec.Bucket, c.rec.Prefix)
	if err != nil {
		if s3client.IsNotFound(err) {
			err = fmt.Errorf("%w (with the new connection)", s3store.ErrMarkerMissing)
		}
		return fmt.Errorf("s3account %q: %s: %w", b.name, storeName(c), err)
	}
	if want := c.store.Marker().BackingID; want != "" && m.BackingID != want {
		return fmt.Errorf("s3account %q: %s: %w: the location holds backing %s, not %s (with the new connection)",
			b.name, storeName(c), s3store.ErrMarkerMismatch, m.BackingID, want)
	}
	if len(cc.KEK) > 0 {
		if _, err := m.CheckKEK(cc.KEK); err != nil {
			return fmt.Errorf("s3account %q: %s: %w", b.name, storeName(c), err)
		}
	}
	return nil
}

// switchContainer brings container c to conn (adminMu held) and returns
// the container to use: c itself when it already uses conn, failed to open
// (it has no store), or is loaded and accepts conn (prepared and
// committed); otherwise a new container reopened with conn under the same
// claim (it loads in the background).
func (b *Backend) switchContainer(ctx context.Context, c *container, conn *s3store.Connection) *container {
	if c.home || c.conn == conn {
		return c
	}
	if c.store == nil {
		c.conn = conn
		return c
	}
	if c.loaded() {
		commit, err := c.store.PrepareConnection(ctx, containerConn(conn, c.rec.Region))
		if err == nil {
			commit()
			c.conn = conn
			return c
		}
		b.log.Error("s3account: a container does not accept the new connection; reopening it with it", "backing", b.name,
			"container", c.id, "location", c.locationString(), "err", err)
	}
	return b.reopenContainer(c)
}

// publishSwitched replaces container old by its switched version next in
// the backing (adminMu held), unless old was removed meanwhile.
func (b *Backend) publishSwitched(old, next *container) {
	if next == old {
		return
	}
	b.mu.Lock()
	cur := b.containers[old.id]
	if cur == old {
		b.containers[old.id] = next
	}
	b.mu.Unlock()
	if cur != old {
		_ = closeContainer(next)
		return
	}
	b.watchContainer(next)
}

// checkCredentials checks, for a backing without a home, that client's
// credentials are accepted: the service answers ListBuckets with anything
// but a credentials error, and the remote buckets of up to
// maxNativeChecks native buckets answer HEAD (a missing one is not the
// connection's fault).
func (b *Backend) checkCredentials(ctx context.Context, client *s3client.Client, natives []bucketRecord) error {
	_, err := client.ListBuckets(ctx, s3client.ListBucketsInput{MaxBuckets: 1})
	if err := connectionError(err); err != nil {
		return fmt.Errorf("s3account %q: with the new connection: %w", b.name, err)
	}
	slices.SortFunc(natives, func(a, c bucketRecord) int { return strings.Compare(a.Remote, c.Remote) })
	for i, rec := range natives {
		if i == maxNativeChecks {
			break
		}
		err := client.ForRegion(rec.RemoteRegion).HeadBucket(ctx, rec.Remote)
		if s3client.IsAccessDenied(err) {
			return fmt.Errorf("s3account %q: with the new connection, the remote bucket %q of native bucket %q: %w",
				b.name, rec.Remote, rec.Info.Name, err)
		}
		if err := connectionError(err); err != nil {
			return fmt.Errorf("s3account %q: with the new connection, the remote bucket %q of native bucket %q: %w",
				b.name, rec.Remote, rec.Info.Name, err)
		}
	}
	return nil
}

// connectionError returns err when it says the connection settings are
// wrong (unknown key, bad signature) or the service cannot be reached.
func connectionError(err error) error {
	switch {
	case err == nil, s3client.IsNotImplemented(err):
		return nil
	case s3client.IsCode(err, "InvalidAccessKeyId"), s3client.IsCode(err, "SignatureDoesNotMatch"),
		s3client.IsCode(err, "AuthorizationHeaderMalformed"):
		return err
	case s3client.IsUnavailable(err):
		return fmt.Errorf("%w: %w", storage.ErrBackingUnavailable, err)
	}
	return nil
}

// reopenContainer closes c and opens it again with the backing's current
// connection under the same claim (adminMu held, or c not published yet);
// the caller publishes the result. The new container is opened with
// MustExist and c's backing ID: it must find the same store.
func (b *Backend) reopenContainer(c *container) *container {
	release := c.release
	c.release = nil
	_ = closeContainer(c)
	next := b.openContainer(c.rec, s3store.MustExist, c.rec.BackingID, release)
	if next.failErr != nil {
		b.log.Error("s3account: reopening a container with the new connection failed", "backing", b.name,
			"container", c.id, "err", next.failErr)
	}
	return next
}
