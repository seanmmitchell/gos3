package copier

import (
	"cmp"
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
// A key with more than MaxKeyVersions entries on either side does not stop
// the walk: its pair comes with that side's entries left out and Err set
// (see KeyPair), and the walk goes on with the next key.
//
// The two ranges must not overlap (the same bucket of the same backend with
// one prefix starting with the other): copying into the walked source
// would never end. Memory is bounded by two walk batches and
// MaxKeyVersions entries per key. fn runs outside of both walks, so it may
// write to either backend; it may keep the pairs. An error from fn stops
// the walk and is returned as is; ZipWalk wraps walk errors with the side
// that failed.
func ZipWalk(ctx context.Context, src, dst Endpoint, after string, versions bool, fn func([]KeyPair) error) error {
	if fn == nil {
		return errors.New("copier: nil ZipWalk callback")
	}
	if src.Backend == nil || dst.Backend == nil {
		return errors.New("copier: endpoint without a backend")
	}
	sc := &cursor{ep: src, side: "source", versions: versions}
	dc := &cursor{ep: dst, side: "target", versions: versions}
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
				s := sc.next()
				p = KeyPair{SrcKey: src.Prefix + sk, DstKey: dst.Prefix + sk, Src: s.entries, Err: s.err}
			case sc.empty() || dk < sk:
				d := dc.next()
				p = KeyPair{SrcKey: src.Prefix + dk, DstKey: dst.Prefix + dk, Dst: d.entries, Err: d.err}
			default:
				s, d := sc.next(), dc.next()
				p = KeyPair{SrcKey: src.Prefix + sk, DstKey: dst.Prefix + dk, Src: s.entries, Dst: d.entries, Err: cmp.Or(s.err, d.err)}
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
	side     string // "source" or "target", for errors
	versions bool
	after    string // the walk resumes strictly after this key ("" = start)
	groups   []group
	head     int
	done     bool // the walk has delivered every key
}

// group is one key's entries as a cursor read them. A key with more than
// MaxKeyVersions entries keeps none, and err (wrapping ErrTooManyVersions)
// says so.
type group struct {
	key     string
	entries []storage.ObjectInfo
	err     error
}

func (c *cursor) empty() bool { return c.head >= len(c.groups) }

// rel returns the next key relative to the endpoint's prefix ("" when
// empty).
func (c *cursor) rel() string {
	if c.empty() {
		return ""
	}
	return c.groups[c.head].key[len(c.ep.Prefix):]
}

// next returns the next key's group.
func (c *cursor) next() group {
	g := c.groups[c.head]
	c.groups[c.head] = group{}
	c.head++
	return g
}

// fill reads the next batch of keys once the buffer is used up. A batch
// ends at the first key boundary after batchVersions entries. A key found
// to have more than MaxKeyVersions entries is drained: the entries held
// are dropped, the rest are skipped as the walk passes them, and the group
// is flagged. So one key never holds more than MaxKeyVersions entries in
// memory, and the walk goes on with the next key.
func (c *cursor) fill(ctx context.Context) error {
	if !c.empty() || c.done {
		return nil
	}
	var (
		groups []group
		total  int // entries held
		full   bool
	)
	collect := func(oi storage.ObjectInfo) error {
		if n := len(groups); n == 0 || groups[n-1].key != oi.Key {
			if total >= batchVersions {
				full = true
				return storage.ErrStopWalk
			}
			groups = append(groups, group{key: oi.Key})
		}
		g := &groups[len(groups)-1]
		switch {
		case g.err != nil:
			// Draining a key with too many versions.
		case len(g.entries) >= MaxKeyVersions:
			g.err = fmt.Errorf("%w: %s/%s on the %s exceeds %d", ErrTooManyVersions, c.ep.Bucket, oi.Key, c.side, MaxKeyVersions)
			total -= len(g.entries)
			g.entries = nil
		default:
			g.entries = append(g.entries, oi)
			total++
		}
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
		c.after = groups[n-1].key
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
