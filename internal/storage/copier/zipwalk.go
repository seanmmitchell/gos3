package copier

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/seanmmitchell/gos3/internal/storage"
)

// batchVersions is the number of listing entries a walk collects before it
// is suspended and its keys are paired, and roughly the number of entries
// ZipWalk passes to its callback at once. Batches end at key boundaries,
// so a key with more versions than this is still delivered whole, up to
// MaxKeyVersions. A variable for tests.
var batchVersions = 1000

// ZipWalk pairs the keys under src with those under dst, in ascending key
// order, and calls fn with them in batches of about a thousand listing
// entries: a merge join of two walks, WalkVersions when versions is set
// (every version and delete marker of each key) and WalkObjects otherwise
// (each key's latest version, keys whose latest version is a delete marker
// left out). See KeyPair.
//
// Source key src.Prefix+k pairs with target key dst.Prefix+k; a key found
// on one side only gets a pair with nothing on the other. The walk starts
// after source key after ("" = at the beginning), so a caller resumes an
// interrupted walk with the SrcKey of the last pair it finished (target-only
// keys have the source key they would map from).
//
// The two ranges must not overlap (the same bucket of the same backend with
// one prefix starting with the other): copying into the walked source
// would never end. Memory is bounded by two walk batches and
// MaxKeyVersions entries per key. fn runs outside of both walks, so it may write to either backend;
// it may keep the pairs. An error from fn stops the walk and is returned
// as is; ZipWalk wraps walk errors with the side that failed.
func ZipWalk(ctx context.Context, src, dst Endpoint, after string, versions bool, fn func([]KeyPair) error) error {
	if fn == nil {
		return errors.New("copier: nil ZipWalk callback")
	}
	if src.Backend == nil || dst.Backend == nil {
		return errors.New("copier: endpoint without a backend")
	}
	sc := &cursor{ep: src, versions: versions}
	dc := &cursor{ep: dst, versions: versions}
	switch {
	case after == "" || after < src.Prefix:
		// From the beginning.
	case strings.HasPrefix(after, src.Prefix):
		sc.after = after
		dc.after = dst.Prefix + after[len(src.Prefix):]
	default:
		// after sorts after every key with the source prefix.
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sc.fill(ctx); err != nil {
			return fmt.Errorf("listing source %s: %w", src, err)
		}
		if err := dc.fill(ctx); err != nil {
			return fmt.Errorf("listing target %s: %w", dst, err)
		}
		if sc.empty() && dc.empty() {
			return nil // fill leaves a cursor empty only once its walk is done
		}
		var (
			batch   []KeyPair
			entries int
		)
		// Pair keys while both sides are known: a cursor whose buffer is
		// used up must read further before a key of the other side can be
		// told to be missing from it.
		for (!sc.empty() || sc.done) && (!dc.empty() || dc.done) && !(sc.empty() && dc.empty()) {
			var p KeyPair
			sk, dk := sc.rel(), dc.rel()
			switch {
			case dc.empty() || !sc.empty() && sk < dk:
				p = KeyPair{SrcKey: src.Prefix + sk, DstKey: dst.Prefix + sk, Src: sc.next()}
			case sc.empty() || dk < sk:
				p = KeyPair{SrcKey: src.Prefix + dk, DstKey: dst.Prefix + dk, Dst: dc.next()}
			default:
				p = KeyPair{SrcKey: src.Prefix + sk, DstKey: dst.Prefix + dk, Src: sc.next(), Dst: dc.next()}
			}
			batch = append(batch, p)
			entries += len(p.Src) + len(p.Dst)
			if entries >= batchVersions {
				if err := fn(batch); err != nil {
					return err
				}
				batch, entries = nil, 0
			}
		}
		if len(batch) > 0 {
			if err := fn(batch); err != nil {
				return err
			}
		}
	}
}

// cursor reads one side of a ZipWalk in batches of whole keys.
type cursor struct {
	ep       Endpoint
	versions bool
	after    string // the walk resumes strictly after this key ("" = start)
	groups   [][]storage.ObjectInfo
	head     int
	done     bool // the walk has delivered every key
}

func (c *cursor) empty() bool { return c.head >= len(c.groups) }

// rel returns the next key relative to the endpoint's prefix ("" when
// empty).
func (c *cursor) rel() string {
	if c.empty() {
		return ""
	}
	return c.groups[c.head][0].Key[len(c.ep.Prefix):]
}

// next returns the next key's entries.
func (c *cursor) next() []storage.ObjectInfo {
	g := c.groups[c.head]
	c.groups[c.head] = nil
	c.head++
	return g
}

// fill reads the next batch of keys once the buffer is used up. A batch
// ends at the first key boundary after batchVersions entries.
func (c *cursor) fill(ctx context.Context) error {
	if !c.empty() || c.done {
		return nil
	}
	var (
		groups [][]storage.ObjectInfo
		total  int
		full   bool
	)
	collect := func(oi storage.ObjectInfo) error {
		if n := len(groups); n == 0 || groups[n-1][0].Key != oi.Key {
			if total >= batchVersions {
				full = true
				return storage.ErrStopWalk
			}
			groups = append(groups, nil)
		}
		if len(groups[len(groups)-1]) >= MaxKeyVersions {
			return fmt.Errorf("%w: %s/%s exceeds %d", ErrTooManyVersions, c.ep.Bucket, oi.Key, MaxKeyVersions)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], oi)
		total++
		return nil
	}
	var err error
	if c.versions {
		err = c.ep.Backend.WalkVersions(ctx, c.ep.Bucket, c.ep.Prefix, c.after, "", collect)
	} else {
		err = c.ep.Backend.WalkObjects(ctx, c.ep.Bucket, c.ep.Prefix, c.after, collect)
	}
	if err != nil {
		return err
	}
	c.groups, c.head, c.done = groups, 0, !full
	if n := len(groups); n > 0 {
		c.after = groups[n-1][0].Key
	}
	return nil
}

// LookupPair lists one source key and its target key the way ZipWalk
// would pair them (see KeyPair), for callers that learn of changed keys
// otherwise than by walking (change events of a mirror's source).
func LookupPair(ctx context.Context, src, dst Endpoint, srcKey string, versions bool) (KeyPair, error) {
	dstKey, ok := MapKey(src, dst, srcKey)
	if !ok {
		return KeyPair{}, fmt.Errorf("copier: key %q is not under the source prefix %q", srcKey, src.Prefix)
	}
	p := KeyPair{SrcKey: srcKey, DstKey: dstKey}
	var err error
	if p.Src, err = listKey(ctx, src.Backend, src.Bucket, srcKey, versions); err != nil {
		return KeyPair{}, fmt.Errorf("listing %s on the source: %w", srcKey, err)
	}
	if dstKey == "" {
		return p, nil
	}
	if p.Dst, err = listKey(ctx, dst.Backend, dst.Bucket, dstKey, versions); err != nil {
		return KeyPair{}, fmt.Errorf("listing %s on the target: %w", dstKey, err)
	}
	return p, nil
}
