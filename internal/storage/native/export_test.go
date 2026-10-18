package native

import "context"

// Test hooks (compiled into tests only).

// SetListPage sets the page size of the store's listings (0 = the
// service's default), so tests can make walks span pages.
func SetListPage(s *Store, n int) {
	s.mu.Lock()
	s.listPage = n
	s.mu.Unlock()
}

// SweepOverlays runs the background sweep of overlays now, for every
// bucket.
func SweepOverlays(s *Store, ctx context.Context) { s.sweepOverlays(ctx, true) }

// OverlayCount returns the number of overlays of bucket's keys with the
// given prefix.
func OverlayCount(s *Store, bucket, keyPrefix string) int { return s.overlayCount(bucket, keyPrefix) }
