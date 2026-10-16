package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/crypt"
)

// Marker is the content of P/gos3.json, which identifies a prefix as a gos3
// s3 backing. It is created once (If-None-Match) by the first Open and only
// ever rewritten with If-Match on its ETag (rewriteMarker): to add the KEK
// check value, to set BlobTags on a backing created before blobs were
// tagged, and to clear the legacy gateway flag (ClearLegacyGateway).
type Marker struct {
	Format    int    `json:"format"`
	Kind      string `json:"kind"`
	BackingID string `json:"backingId"` // random 128-bit hex
	// Gateway marks the metadata prefix of a legacy gateway backing (builds
	// before s3 account backings). It stays readable for the migration,
	// which clears it (Backend.ClearLegacyGateway); Open ignores it.
	Gateway bool `json:"gateway,omitempty"`
	// Role is the role given when the backing was created (Options.Role:
	// RoleHome, RoleContainer or ""); informational.
	Role string `json:"role,omitempty"`
	// BlobTags says that blobs carry the x-amz-meta-gos3-store user
	// metadata naming the backing. It is set when the marker is created,
	// or at the first Open of a backing created before blobs were tagged
	// (whose older blobs stay untagged: the orphan recovery never deletes
	// them).
	BlobTags bool `json:"blobTags,omitempty"`
	// KEKCheck is crypt.KeyCheck of the key-encryption key the backing's
	// master keys are wrapped with ("" until a KEK is first configured).
	KEKCheck string    `json:"kekCheck,omitempty"`
	Created  time.Time `json:"created"`
}

// KEKState says how the configured KEK relates to the marker's check value.
type KEKState int

const (
	// KEKNone: no KEK is configured and the backing never had one.
	KEKNone KEKState = iota
	// KEKVerified: the configured KEK matches the marker's check value.
	KEKVerified
	// KEKMissing: the backing has a KEK check value but no KEK is
	// configured; wrapped keys exist (or may) that cannot be opened, so
	// encryption is unavailable.
	KEKMissing
)

func (s KEKState) String() string {
	switch s {
	case KEKNone:
		return "none"
	case KEKVerified:
		return "verified"
	case KEKMissing:
		return "missing"
	}
	return fmt.Sprintf("KEKState(%d)", int(s))
}

// CheckKEK compares kek (nil: none configured) with the marker's check
// value. A mismatch is ErrWrongKEK. A KEK while the marker has no check
// value yet is reported as KEKVerified; the caller records the check.
func (m Marker) CheckKEK(kek []byte) (KEKState, error) {
	switch {
	case len(kek) == 0 && m.KEKCheck == "":
		return KEKNone, nil
	case len(kek) == 0:
		return KEKMissing, nil
	case m.KEKCheck == "" || m.KEKCheck == crypt.KeyCheck(kek):
		return KEKVerified, nil
	}
	return KEKVerified, ErrWrongKEK
}

// validate checks that the marker describes a backing this package can
// open and, when want is set, that it is the backing with ID want.
func (m Marker) validate(want string) error {
	if m.Format != markerFormat || m.Kind != markerKind || m.BackingID == "" {
		return fmt.Errorf("%w: format %d, kind %q", ErrMarkerMismatch, m.Format, m.Kind)
	}
	if want != "" && m.BackingID != want {
		return fmt.Errorf("%w: the location holds backing %s, not %s", ErrMarkerMismatch, m.BackingID, want)
	}
	return nil
}

// ReadMarker reads the marker of the backing at bucket/prefix. A missing
// marker is reported with s3client.IsNotFound; remote failures wrap
// storage.ErrBackingUnavailable.
func ReadMarker(ctx context.Context, c *s3client.Client, bucket, prefix string) (Marker, string, error) {
	r := remote{c: c, bucket: bucket}
	var m Marker
	etag, err := r.getJSON(ctx, keys{prefix}.marker(), &m)
	return m, etag, err
}

// rewriteMarker reconciles the marker of the backing at bucket/prefix: it
// calls check on the marker and, when change modifies it, writes the result
// with If-Match on the marker's ETag. A concurrent rewrite (412) or an
// unknown outcome reads the marker again and starts over, so concurrent
// writers of compatible changes all succeed, each change applied
// atomically. m and etag are the marker as last read (etag "": read it
// first). It returns the marker as stored.
func rewriteMarker(ctx context.Context, c *s3client.Client, bucket, prefix string, m Marker, etag string,
	check func(Marker) error, change func(*Marker) bool) (Marker, error) {
	r, k := remote{c: c, bucket: bucket}, keys{prefix}
	for range 5 {
		if etag == "" {
			m = Marker{}
			var err error
			if etag, err = r.getJSON(ctx, k.marker(), &m); err != nil {
				return Marker{}, err
			}
			if etag == "" {
				return Marker{}, fmt.Errorf("s3store: the service returned no ETag for %s", k.marker())
			}
		}
		if err := check(m); err != nil {
			return Marker{}, err
		}
		next := m
		if !change(&next) {
			return m, nil
		}
		data, _ := json.Marshal(next)
		_, err := r.put(ctx, k.marker(), data, s3client.PutOptions{IfMatch: etag, NoRetry: true})
		switch {
		case err == nil:
			return next, nil
		case s3client.IsPreconditionFailed(err) || s3client.IsUnavailable(err):
			etag = "" // changed concurrently or unknown outcome: read it again
		default:
			return Marker{}, remoteErr("write marker", err)
		}
	}
	return Marker{}, fmt.Errorf("s3store: marker keeps changing: %w", storage.ErrBackingUnavailable)
}

// recordKEKCheck stores the check value of kek in the marker of backing
// backingID at bucket/prefix if it has none (a concurrent writer of another
// value wins or loses atomically) and returns the marker. A different
// stored value is ErrWrongKEK.
func recordKEKCheck(ctx context.Context, c *s3client.Client, bucket, prefix, backingID string, kek []byte) (Marker, error) {
	return rewriteMarker(ctx, c, bucket, prefix, Marker{}, "",
		func(m Marker) error {
			if err := m.validate(backingID); err != nil {
				return err
			}
			_, err := m.CheckKEK(kek)
			return err
		},
		func(m *Marker) bool {
			if m.KEKCheck != "" {
				return false
			}
			m.KEKCheck = crypt.KeyCheck(kek)
			return true
		})
}

// remote wraps the client with the bucket and small helpers.
type remote struct {
	c      *s3client.Client
	bucket string
}

// maxSmallObject bounds the size of JSON documents read whole (marker,
// lease, journal batch headers).
const maxSmallObject = 1 << 20

// getBytes reads a whole object of at most limit bytes.
func (r remote) getBytes(ctx context.Context, key string, limit int64) ([]byte, s3client.ObjectAttrs, error) {
	body, attrs, err := r.c.GetObject(ctx, r.bucket, key, s3client.GetOptions{})
	if err != nil {
		return nil, attrs, remoteErr("get "+key, err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, attrs, remoteErr("get "+key, &s3client.Error{Op: "GetObject", Err: err})
	}
	if int64(len(data)) > limit {
		return nil, attrs, fmt.Errorf("%w: %s is larger than %d bytes", ErrCorruptJournal, key, limit)
	}
	return data, attrs, nil
}

// getJSON reads a small JSON object into v and returns its ETag.
func (r remote) getJSON(ctx context.Context, key string, v any) (string, error) {
	data, attrs, err := r.getBytes(ctx, key, maxSmallObject)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return "", fmt.Errorf("s3store: decode %s: %w", key, err)
	}
	return attrs.ETag, nil
}

// put writes data to key with opts and returns the new ETag.
func (r remote) put(ctx context.Context, key string, data []byte, opts s3client.PutOptions) (string, error) {
	return r.c.PutObject(ctx, r.bucket, key, bytes.NewReader(data), int64(len(data)), opts)
}

// remoteErr annotates a client error; failures of the service (rather than
// rejections of the request) also wrap storage.ErrBackingUnavailable.
func remoteErr(what string, err error) error {
	if err == nil {
		return nil
	}
	if s3client.IsUnavailable(err) {
		return fmt.Errorf("s3store: %s: %w: %w", what, storage.ErrBackingUnavailable, err)
	}
	return fmt.Errorf("s3store: %s: %w", what, err)
}

// randomID returns n random bytes in hex.
func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// openMarker reads (or, when the create mode allows it, creates) the
// marker, runs the conditional-write probe and reconciles the marker: the
// KEK check value and BlobTags. It returns the marker and the KEK state.
func (e *Engine) openMarker(ctx context.Context) (Marker, KEKState, error) {
	r, k := e.remote(), e.keys
	want := e.opts.ExpectBackingID
	if want == "" {
		// A reload must find the backing loaded before, never another one
		// (or a new one) at the same location.
		want = e.Marker().BackingID
	}
	var m Marker
	etag, err := r.getJSON(ctx, k.marker(), &m)
	missing := s3client.IsNotFound(err)
	if err != nil && !missing {
		return Marker{}, 0, err
	}
	if missing {
		if err := e.checkCreate(ctx, want); err != nil {
			return Marker{}, 0, err
		}
	} else if err := e.checkOwned(m); err != nil {
		return Marker{}, 0, err
	}
	// The probe runs before a marker is ever created, so a service that
	// ignores conditional writes never gets one.
	if err := e.probe(ctx); err != nil {
		return Marker{}, 0, err
	}
	if missing {
		if e.opts.Create == CreateIfEmpty {
			// Keys may have appeared since the first check.
			if err := e.checkEmpty(ctx); err != nil {
				return Marker{}, 0, err
			}
		}
		if m, etag, err = e.createMarker(ctx); err != nil {
			return Marker{}, 0, err
		}
	}
	kek := e.currentKEK()
	m, err = rewriteMarker(ctx, e.client(), e.opts.Bucket, e.opts.Prefix, m, etag,
		func(m Marker) error {
			if err := m.validate(want); err != nil {
				return err
			}
			if err := e.checkOwned(m); err != nil {
				// createMarker lost a race, or the marker was replaced.
				return err
			}
			_, err := m.CheckKEK(kek)
			return err
		},
		func(m *Marker) bool {
			changed := false
			if len(kek) > 0 && m.KEKCheck == "" {
				// The first time a KEK is configured: record its check
				// value.
				m.KEKCheck, changed = crypt.KeyCheck(kek), true
			}
			if !m.BlobTags {
				// A backing of an older build: its blobs are tagged from
				// now on (the older ones stay untagged).
				m.BlobTags, changed = true, true
			}
			return changed
		})
	if err != nil {
		if s3client.IsNotFound(err) {
			// Deleted while being reconciled.
			return Marker{}, 0, fmt.Errorf("%w (%s)", ErrMarkerMissing, e.location())
		}
		return Marker{}, 0, err
	}
	state, _ := m.CheckKEK(kek)
	return m, state, nil
}

// checkCreate decides whether a backing may be created at the location,
// which has no marker; want is the backing ID expected there ("" = any).
func (e *Engine) checkCreate(ctx context.Context, want string) error {
	switch {
	case want != "":
		return fmt.Errorf("%w (%s, expected backing %s)", ErrMarkerMissing, e.location(), want)
	case e.opts.Create == MustExist:
		return fmt.Errorf("%w (%s)", ErrMarkerMissing, e.location())
	case e.opts.Create == CreateIfEmpty:
		return e.checkEmpty(ctx)
	}
	return e.checkUnmanaged(ctx)
}

// checkOwned refuses, in CreateIfEmpty mode, a marker this engine did not
// try to create (createMarker) unless it is the expected backing: a store
// someone else created at the location (even while this engine was
// creating its own) makes the location non-empty.
func (e *Engine) checkOwned(m Marker) error {
	if e.opts.Create != CreateIfEmpty || m.BackingID == "" {
		return nil
	}
	if e.opts.ExpectBackingID != "" && m.BackingID == e.opts.ExpectBackingID {
		return nil
	}
	e.mu.Lock()
	ours := e.attempted[m.BackingID]
	e.mu.Unlock()
	if ours {
		return nil
	}
	return fmt.Errorf("%w: %s holds %s of another backing (%s)", ErrPrefixNotEmpty, e.location(), markerKey, m.BackingID)
}

// checkUnmanaged refuses a prefix without a marker that holds backing data.
func (e *Engine) checkUnmanaged(ctx context.Context) error {
	for _, dir := range []string{metaDir, dataDir, partsDir, keysDir, resticDir, resticAuxDir, gatewayDir} {
		out, err := e.client().ListObjectsV2(ctx, e.opts.Bucket, s3client.ListInput{Prefix: e.keys.prefix + dir, MaxKeys: 100})
		if err != nil {
			return remoteErr("list "+dir, err)
		}
		for _, o := range out.Objects {
			// Probes are written before the marker; a crash in between
			// leaves one behind.
			if !strings.HasPrefix(o.Key, e.keys.prefix+probePrefix) {
				return fmt.Errorf("%w (found %s)", ErrUnmanagedPrefix, o.Key)
			}
		}
	}
	return nil
}

// errStopWalk ends a listing early.
var errStopWalk = errors.New("s3store: stop walk")

// checkEmpty refuses (ErrPrefixNotEmpty) a location holding any key but a
// zero-byte folder placeholder named like the prefix and leftovers of the
// conditional-write probe (written before the marker, deleted best effort).
func (e *Engine) checkEmpty(ctx context.Context) error {
	prefix := e.keys.prefix
	var found string
	err := e.client().Walk(ctx, e.opts.Bucket, prefix, "", func(o s3client.ObjectAttrs) error {
		if prefix != "" && o.Key == prefix && o.Size == 0 {
			return nil
		}
		if e.keys.isProbe(o.Key) {
			return nil
		}
		found = o.Key
		return errStopWalk
	})
	if found != "" {
		return fmt.Errorf("%w: %s holds %s", ErrPrefixNotEmpty, e.location(), found)
	}
	if err != nil {
		return remoteErr("list "+e.location(), err)
	}
	return nil
}

// probe verifies that the service honours If-None-Match on PUT: a second
// conditional PUT of the same key must fail with 412.
func (e *Engine) probe(ctx context.Context) error {
	r := e.remote()
	key := e.keys.probe(randomID(8))
	data := []byte("gos3 conditional-write probe\n")
	opts := s3client.PutOptions{IfNoneMatch: true, NoRetry: true}
	if _, err := r.put(ctx, key, data, opts); err != nil {
		return remoteErr("conditional-write probe", err)
	}
	defer func() { _ = e.client().DeleteObject(context.WithoutCancel(ctx), e.opts.Bucket, key) }()
	_, err := r.put(ctx, key, data, opts)
	switch {
	case err == nil:
		return ErrNoConditionalWrites
	case s3client.IsPreconditionFailed(err):
		return nil
	}
	return remoteErr("conditional-write probe", err)
}

// createMarker creates the marker of a new backing. Losing a race (or an
// ambiguous outcome) re-reads whatever marker is there; the ID tried is
// remembered first (Engine.attempted), so that marker is recognized as
// this engine's even when the answer to the PUT was lost.
func (e *Engine) createMarker(ctx context.Context) (Marker, string, error) {
	m := Marker{
		Format:    markerFormat,
		Kind:      markerKind,
		BackingID: randomID(16),
		Role:      e.opts.Role,
		BlobTags:  true,
		Created:   e.now().UTC().Truncate(time.Second),
	}
	if len(e.currentKEK()) > 0 {
		m.KEKCheck = crypt.KeyCheck(e.currentKEK())
	}
	data, _ := json.Marshal(m)
	e.mu.Lock()
	if e.attempted == nil {
		e.attempted = map[string]bool{}
	}
	e.attempted[m.BackingID] = true
	e.mu.Unlock()
	r := e.remote()
	etag, err := r.put(ctx, e.keys.marker(), data, s3client.PutOptions{IfNoneMatch: true, NoRetry: true})
	if err == nil {
		e.logger().Info("s3store: created backing marker", "bucket", e.opts.Bucket, "prefix", e.opts.Prefix,
			"backingId", m.BackingID, "role", m.Role)
		return m, etag, nil
	}
	if !s3client.IsPreconditionFailed(err) && !s3client.IsUnavailable(err) {
		return Marker{}, "", remoteErr("create marker", err)
	}
	var got Marker
	etag, rerr := r.getJSON(ctx, e.keys.marker(), &got)
	if rerr != nil {
		if s3client.IsNotFound(rerr) {
			return Marker{}, "", remoteErr("create marker", err)
		}
		return Marker{}, "", errors.Join(remoteErr("create marker", err), rerr)
	}
	return got, etag, nil
}

// clearLegacyGateway clears the legacy gateway flag of the marker of the
// backing loaded by the engine (see Backend.ClearLegacyGateway).
func (e *Engine) clearLegacyGateway(ctx context.Context) error {
	id := e.Marker().BackingID
	if id == "" {
		return fmt.Errorf("s3store: the backing at %s is not loaded yet: %w", e.location(), storage.ErrBackingUnavailable)
	}
	m, err := rewriteMarker(ctx, e.client(), e.opts.Bucket, e.opts.Prefix, Marker{}, "",
		func(m Marker) error { return m.validate(id) },
		func(m *Marker) bool {
			if !m.Gateway {
				return false
			}
			m.Gateway = false
			return true
		})
	if err != nil {
		if s3client.IsNotFound(err) {
			return fmt.Errorf("%w (%s)", ErrMarkerMissing, e.location())
		}
		return err
	}
	e.noteMarker(m)
	return nil
}
