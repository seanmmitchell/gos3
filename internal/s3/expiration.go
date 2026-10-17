package s3

import (
	"bytes"
	"container/list"
	"context"
	"hash/maphash"
	"net/http"
	"sync"

	"github.com/seanmmitchell/gos3/internal/lifecycle"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// Lifecycle configurations feed informational response headers of
// GetObject, HeadObject, PutObject and other object operations, so they are
// looked up on every such request. A configuration may be up to
// lifecycle.MaxConfigSize (2 MiB), and anonymous readers of a public
// bucket choose how often it is looked up. So the parsed configurations
// are kept in their own cache (lifecycleConfigs) that
//
//   - recognises an unchanged document by a seeded hash and its length,
//     computed over the stored bytes without copying them when the backend
//     is a storage.ConfigViewer (both built-in backends are), instead of
//     keeping and comparing a copy of the raw document: a request costs
//     one pass over the document and no allocation;
//   - is bounded by an estimate of the memory the parsed configurations
//     hold (lifecycleCacheBudget), not just by their number.

// Lifecycle cache bounds.
const (
	// lifecycleCacheBudget bounds the estimated memory of the cached parsed
	// configurations (lifecycleCost of each).
	lifecycleCacheBudget = 64 << 20
	// lifecycleCacheEntries bounds the number of cached configurations.
	lifecycleCacheEntries = defaultConfigCacheSize
)

// lifecycleSeed seeds the hashes that identify cached documents. The hash
// is only compared within this process; the seed is random, so documents
// cannot be crafted to collide.
var lifecycleSeed = maphash.MakeSeed()

// lifecycleConfigs is the cache of parsed lifecycle configurations of all
// servers of the process (each entry is keyed by its server as well as its
// bucket, so servers never see each other's entries; those of a server
// that is gone age out).
var lifecycleConfigs = newLifecycleCache(lifecycleCacheBudget, lifecycleCacheEntries)

// lifecycleKey identifies a bucket of a server in lifecycleConfigs.
type lifecycleKey struct {
	s      *Server
	bucket string
}

// lifecycleEntry is a cached parsed configuration (nil for a document that
// does not parse) with the hash and length of its document.
type lifecycleEntry struct {
	key  lifecycleKey
	sum  uint64
	n    int
	cfg  *lifecycle.Configuration
	cost int64
}

// lifecycleCost estimates the memory a configuration parsed from n bytes
// holds: its strings (at most the document's text) plus the rule structs.
func lifecycleCost(n int) int64 { return 2*int64(n) + 1024 }

// lifecycleCache is a bounded LRU of lifecycleEntry values.
type lifecycleCache struct {
	mu         sync.Mutex
	maxBytes   int64
	maxEntries int
	bytes      int64
	lru        *list.List // of *lifecycleEntry, most recent first
	entries    map[lifecycleKey]*list.Element
}

func newLifecycleCache(maxBytes int64, maxEntries int) *lifecycleCache {
	return &lifecycleCache{maxBytes: maxBytes, maxEntries: maxEntries, lru: list.New(), entries: map[lifecycleKey]*list.Element{}}
}

// get returns the configuration cached for k when it was parsed from a
// document with hash sum and length n.
func (c *lifecycleCache) get(k lifecycleKey, sum uint64, n int) (*lifecycle.Configuration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[k]
	if !ok {
		return nil, false
	}
	e := el.Value.(*lifecycleEntry)
	if e.sum != sum || e.n != n {
		return nil, false
	}
	c.lru.MoveToFront(el)
	return e.cfg, true
}

// put caches cfg for k, evicting the least recently used entries while the
// cache is over its bounds. A configuration costing more than the whole
// budget is not cached.
func (c *lifecycleCache) put(k lifecycleKey, sum uint64, n int, cfg *lifecycle.Configuration) {
	e := &lifecycleEntry{key: k, sum: sum, n: n, cfg: cfg, cost: lifecycleCost(n)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		c.bytes -= el.Value.(*lifecycleEntry).cost
		c.lru.Remove(el)
		delete(c.entries, k)
	}
	if e.cost > c.maxBytes {
		return
	}
	c.entries[k] = c.lru.PushFront(e)
	c.bytes += e.cost
	for c.bytes > c.maxBytes || c.lru.Len() > c.maxEntries {
		old := c.lru.Back()
		oe := old.Value.(*lifecycleEntry)
		c.lru.Remove(old)
		delete(c.entries, oe.key)
		c.bytes -= oe.cost
	}
}

// size returns the number of entries and their estimated memory.
func (c *lifecycleCache) size() (int, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes
}

// bucketLifecycle returns the bucket's parsed lifecycle configuration, or
// nil when there is none or it cannot be read or parsed: the headers it
// feeds are informational and must never fail a request.
func (s *Server) bucketLifecycle(ctx context.Context, bucket string) *lifecycle.Configuration {
	key := lifecycleKey{s, bucket}
	var (
		sum uint64
		n   int
		cfg *lifecycle.Configuration
		hit bool
		raw []byte
	)
	view := func(data []byte) error {
		sum, n = maphash.Bytes(lifecycleSeed, data), len(data)
		if cfg, hit = lifecycleConfigs.get(key, sum, n); !hit {
			raw = bytes.Clone(data)
		}
		return nil
	}
	var err error
	if cv, ok := s.backend.(storage.ConfigViewer); ok {
		err = cv.ViewBucketConfig(ctx, bucket, storage.ConfigLifecycle, view)
	} else {
		var data []byte
		if data, err = s.backend.GetBucketConfig(ctx, bucket, storage.ConfigLifecycle); err == nil {
			err = view(data)
		}
	}
	if err != nil {
		return nil
	}
	if hit {
		return cfg
	}
	cfg, err = lifecycle.Parse(raw)
	if err != nil {
		// Remembered as nil, so an invalid document is not parsed again
		// on every request.
		cfg = nil
	}
	lifecycleConfigs.put(key, sum, n, cfg)
	return cfg
}

// setExpirationHeader sets x-amz-expiration (expiry-date and rule-id) when
// an enabled lifecycle Expiration rule (Days or Date) of the bucket applies
// to info, as S3 does on PutObject, CopyObject, CompleteMultipartUpload,
// GetObject and HeadObject. Only current versions get the header
// (noncurrent versions and delete markers expire by other rules).
func (s *Server) setExpirationHeader(ctx context.Context, h http.Header, info storage.ObjectInfo) {
	if info.DeleteMarker || !info.IsLatest {
		return
	}
	cfg := s.bucketLifecycle(ctx, info.Bucket)
	if t, id, ok := cfg.CurrentExpiration(info.Key, info.Tags, info.Size, info.LastModified); ok {
		h.Set("X-Amz-Expiration", lifecycle.ExpirationHeader(t, id))
	}
}

// setAbortHeaders sets x-amz-abort-date and x-amz-abort-rule-id when an
// enabled AbortIncompleteMultipartUpload rule applies to the upload, as S3
// does on CreateMultipartUpload and ListParts.
func (s *Server) setAbortHeaders(ctx context.Context, h http.Header, up storage.UploadInfo) {
	cfg := s.bucketLifecycle(ctx, up.Bucket)
	if t, id, ok := cfg.AbortDate(up.Key, up.Initiated); ok {
		h.Set("X-Amz-Abort-Date", t.UTC().Format(http.TimeFormat))
		h.Set("X-Amz-Abort-Rule-Id", id)
	}
}
