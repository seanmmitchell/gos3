package s3store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client"
)

// TestCloseReleasesLeaseWithStaleETag checks that Close releases the lease
// even when the session's idea of the lease ETag is stale (a renewal that
// landed after Close cancelled it, or whose answer was lost), so the next
// open takes over at once instead of waiting for the lease TTL.
func TestCloseReleasesLeaseWithStaleETag(t *testing.T) {
	v := newBenv(t)
	opts := v.options("stale/")
	opts.LeaseTTL = time.Minute // a missed release would make the reopen wait this long
	b := v.ready(opts)
	pauseLease(b.eng)

	// Rewrite the lease as a renewal of ours that the session never saw.
	r := b.eng.remote()
	ctx := context.Background()
	var doc leaseDoc
	etag, err := r.getJSON(ctx, b.eng.keys.lease(), &doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.Renewals++
	body, _ := json.Marshal(doc)
	if _, err := r.put(ctx, b.eng.keys.lease(), body, s3client.PutOptions{IfMatch: etag}); err != nil {
		t.Fatal(err)
	}

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	var after leaseDoc
	if _, err := r.getJSON(ctx, b.eng.keys.lease(), &after); err != nil {
		t.Fatal(err)
	}
	if !after.Released {
		t.Fatalf("lease not released after Close: %+v", after)
	}
	start := time.Now()
	v.ready(opts)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("reopen took %v (waited for the lease TTL)", d)
	}
}
