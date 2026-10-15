package s3account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/localstate"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
)

// TestWaitReady: WaitReady gives up with the reason when the home cannot
// load, and operations answer storage.ErrBackingUnavailable meanwhile.
func TestWaitReady(t *testing.T) {
	r := s3clienttest.Start(t)
	r.CreateBucket(t, "home")
	ctx := context.Background()
	first := openReady(t, homeOptions(t, r, NewClaims(), "home", ""))
	if loc := first.Location(); loc != "s3://home/" {
		t.Fatalf("Location %q", loc)
	}

	// A second server on the same home: in use.
	b, err := open(homeOptions(t, r, NewClaims(), "home", ""), testHooks())
	noErr(t, "open", err)
	defer b.Close()
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err = b.WaitReady(wctx)
	wantErr(t, "WaitReady (in use)", err, storage.ErrBackingUnavailable)
	wantErr(t, "WaitReady (in use)", err, s3store.ErrInUse)
	_, err = b.ListBuckets(ctx)
	wantErr(t, "ListBuckets before ready", err, storage.ErrBackingUnavailable)
	wantErr(t, "ProbeHealth before ready", b.ProbeHealth(ctx), storage.ErrBackingUnavailable)
	if st := b.Status(); st.Ready || st.Err == nil {
		t.Fatalf("status %+v", st)
	}

	// A home in a missing bucket.
	m, err := open(homeOptions(t, r, NewClaims(), "missing", ""), testHooks())
	noErr(t, "open", err)
	defer m.Close()
	wantErr(t, "WaitReady (missing)", m.WaitReady(wctx), storage.ErrBackingUnavailable)
}

// TestOpenOptions checks Open's validation and the local records' lock.
func TestOpenOptions(t *testing.T) {
	r := s3clienttest.Start(t)
	claims := NewClaims()
	for _, tc := range []struct {
		name string
		mod  func(*Options)
	}{
		{"no claims", func(o *Options) { o.Claims = nil }},
		{"no home, no local dir", func(o *Options) { o.LocalDir = "" }},
		{"bad prefix", func(o *Options) { o.Home = &Location{Bucket: "h", Prefix: "x"} }},
		{"bad kind", func(o *Options) { o.DefaultKind = "other" }},
		{"bad KEK", func(o *Options) { o.Conn.KEK = []byte("short") }},
	} {
		o := localOptions(t, r, claims, t.TempDir())
		tc.mod(&o)
		if b, err := open(o, testHooks()); err == nil {
			_ = b.Close()
			t.Errorf("%s: Open succeeded", tc.name)
		}
	}
	dir := t.TempDir()
	b := openReady(t, localOptions(t, r, claims, dir))
	if loc := b.Location(); loc != "local:" {
		t.Fatalf("Location %q", loc)
	}
	if _, err := open(localOptions(t, r, NewClaims(), dir), testHooks()); !errors.Is(err, localstate.ErrLocked) {
		t.Fatalf("second open of the local records: %v", err)
	}
	// A home claimed by another backing of the server.
	r.CreateBucket(t, "home")
	openReady(t, homeOptions(t, r, claims, "home", "a/"))
	o := homeOptions(t, r, claims, "home", "a/b/")
	o.Name = "nested"
	if _, err := open(o, testHooks()); !errors.Is(err, ErrClaimed) {
		t.Fatalf("nested home: %v", err)
	}
	if err := b.Close(); err != nil || b.Close() != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := b.ListBuckets(context.Background()); !errors.Is(err, storage.ErrBackingUnavailable) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("ListBuckets after Close: %v", err)
	}
}
