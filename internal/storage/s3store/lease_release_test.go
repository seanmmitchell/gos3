package s3store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
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

// A renewal can reach the remote after cancellation without the session
// learning its ETag. Reopening may reclaim that session, but must not
// reclaim a different owner or epoch.
func TestReopenAfterUnobservedLeaseRenewal(t *testing.T) {
	for _, name := range []string{"same-session", "different-owner", "different-epoch"} {
		t.Run(name, func(t *testing.T) {
			v := newEnv(t)
			e, m := v.mustOpen(func(o *EngineOptions) {
				o.LeaseInterval = time.Minute
				o.LeaseTTL = time.Hour
			})
			mustSet(t, m, "a", "1")
			e.failCurrent(errJournalStalled)
			// Stop the old session before simulating a late remote write.
			e.sess.abandon(false)
			r := e.remote()
			ctx := context.Background()
			var doc leaseDoc
			etag, err := r.getJSON(ctx, e.keys.lease(), &doc)
			if err != nil {
				t.Fatal(err)
			}
			doc.Renewals++
			switch name {
			case "different-owner":
				doc.Owner = "another-engine"
			case "different-epoch":
				doc.Epoch++
			}
			body, _ := json.Marshal(doc)
			if _, err := r.put(ctx, e.keys.lease(), body, s3client.PutOptions{IfMatch: etag}); err != nil {
				t.Fatal(err)
			}
			// Skip a real hour-long lease wait, without measuring elapsed
			// wall time on a shared CI runner.
			timeout := time.Second
			if name == "same-session" {
				timeout = 10 * time.Second
			}
			openCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			err = e.Open(openCtx)
			if name == "same-session" {
				if err != nil {
					t.Fatalf("reopen our own renewed lease: %v", err)
				}
				if st := e.State(); st.State != StateServing || st.Epoch != 2 {
					t.Fatalf("reopened state = %+v", st)
				}
				if got := m.state(); !maps.Equal(got, map[string]string{"a": "1"}) {
					t.Fatalf("reloaded state = %v", got)
				}
			} else {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("reopen another session's lease = %v, want deadline exceeded", err)
				}
				var after leaseDoc
				if _, err := r.getJSON(ctx, e.keys.lease(), &after); err != nil {
					t.Fatal(err)
				}
				if after != doc {
					t.Fatalf("another session's lease changed: got %+v, want %+v", after, doc)
				}
			}
		})
	}
}
