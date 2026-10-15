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
// ever changed to add the KEK check value.
type Marker struct {
	Format    int    `json:"format"`
	Kind      string `json:"kind"`
	BackingID string `json:"backingId"` // random 128-bit hex
	Gateway   bool   `json:"gateway"`
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

// validate checks that the marker describes a backing opened with the
// given gateway setting.
func (m Marker) validate(gateway bool) error {
	if m.Format != markerFormat || m.Kind != markerKind || m.BackingID == "" {
		return fmt.Errorf("%w: format %d, kind %q", ErrMarkerMismatch, m.Format, m.Kind)
	}
	if m.Gateway != gateway {
		if m.Gateway {
			return fmt.Errorf("%w: the prefix holds a gateway backing", ErrMarkerMismatch)
		}
		return fmt.Errorf("%w: the prefix holds a regular (non-gateway) backing", ErrMarkerMismatch)
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

// recordKEKCheck stores the check value of kek in the marker of the
// backing at bucket/prefix if it has none (If-Match on the marker's ETag,
// so a concurrent writer of another value wins or loses atomically) and
// returns the marker. A different stored value is ErrWrongKEK.
func recordKEKCheck(ctx context.Context, c *s3client.Client, bucket, prefix string, gateway bool, kek []byte) (Marker, error) {
	r, k := remote{c: c, bucket: bucket}, keys{prefix}
	for range 5 {
		var m Marker
		etag, err := r.getJSON(ctx, k.marker(), &m)
		if err != nil {
			return Marker{}, err
		}
		if err := m.validate(gateway); err != nil {
			return Marker{}, err
		}
		if _, err := m.CheckKEK(kek); err != nil {
			return Marker{}, err
		}
		if m.KEKCheck != "" {
			return m, nil
		}
		next := m
		next.KEKCheck = crypt.KeyCheck(kek)
		data, _ := json.Marshal(next)
		_, err = r.put(ctx, k.marker(), data, s3client.PutOptions{IfMatch: etag, NoRetry: true})
		switch {
		case err == nil:
			return next, nil
		case s3client.IsPreconditionFailed(err) || s3client.IsUnavailable(err):
			continue // read it again
		default:
			return Marker{}, remoteErr("write marker", err)
		}
	}
	return Marker{}, fmt.Errorf("s3store: marker keeps changing: %w", storage.ErrBackingUnavailable)
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

// openMarker reads (or, on an empty prefix, creates) the marker, runs the
// conditional-write probe and reconciles the KEK check value. It returns
// the marker and the KEK state.
func (e *Engine) openMarker(ctx context.Context) (Marker, KEKState, error) {
	r, k := e.remote(), e.keys
	var m Marker
	etag, err := r.getJSON(ctx, k.marker(), &m)
	missing := s3client.IsNotFound(err)
	if err != nil && !missing {
		return Marker{}, 0, err
	}
	if missing {
		if err := e.checkUnmanaged(ctx); err != nil {
			return Marker{}, 0, err
		}
	}
	// The probe runs before a marker is ever created, so a service that
	// ignores conditional writes never gets one.
	if err := e.probe(ctx); err != nil {
		return Marker{}, 0, err
	}
	if missing {
		if m, etag, err = e.createMarker(ctx); err != nil {
			return Marker{}, 0, err
		}
	}
	if err := m.validate(e.opts.Gateway); err != nil {
		return Marker{}, 0, err
	}
	// Record the KEK check value the first time a KEK is configured. A
	// concurrent writer of a different value wins or loses atomically.
	for range 5 {
		state, err := m.CheckKEK(e.currentKEK())
		if err != nil || state != KEKVerified || m.KEKCheck != "" {
			return m, state, err
		}
		next := m
		next.KEKCheck = crypt.KeyCheck(e.currentKEK())
		data, _ := json.Marshal(next)
		if _, err := r.put(ctx, k.marker(), data, s3client.PutOptions{IfMatch: etag, NoRetry: true}); err == nil {
			return next, KEKVerified, nil
		} else if !s3client.IsPreconditionFailed(err) && !s3client.IsUnavailable(err) {
			return Marker{}, 0, remoteErr("write marker", err)
		}
		// Changed concurrently or unknown outcome: read it again.
		m = Marker{}
		if etag, err = r.getJSON(ctx, k.marker(), &m); err != nil {
			return Marker{}, 0, err
		}
		if err := m.validate(e.opts.Gateway); err != nil {
			return Marker{}, 0, err
		}
	}
	return Marker{}, 0, fmt.Errorf("s3store: marker keeps changing: %w", storage.ErrBackingUnavailable)
}

// checkUnmanaged refuses a prefix without a marker that holds backing data.
func (e *Engine) checkUnmanaged(ctx context.Context) error {
	for _, dir := range []string{metaDir, dataDir, keysDir} {
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
// ambiguous outcome) re-reads whatever marker is there.
func (e *Engine) createMarker(ctx context.Context) (Marker, string, error) {
	m := Marker{
		Format:    markerFormat,
		Kind:      markerKind,
		BackingID: randomID(16),
		Gateway:   e.opts.Gateway,
		Created:   e.now().UTC().Truncate(time.Second),
	}
	if len(e.currentKEK()) > 0 {
		m.KEKCheck = crypt.KeyCheck(e.currentKEK())
	}
	data, _ := json.Marshal(m)
	r := e.remote()
	etag, err := r.put(ctx, e.keys.marker(), data, s3client.PutOptions{IfNoneMatch: true, NoRetry: true})
	if err == nil {
		e.logger().Info("s3store: created backing marker", "bucket", e.opts.Bucket, "prefix", e.opts.Prefix, "backingId", m.BackingID)
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
