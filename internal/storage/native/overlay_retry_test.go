package native_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/native"
)

func TestBucketOverlayDeletionRetriesAfterFailure(t *testing.T) {
	h, state := newFaulty(t)
	h.mustPut("bkt", "key", []byte("secret"), storage.ObjectMeta{ACL: "public-read"})
	state.set(&state.failDelete, true)
	if err := h.st.DetachBucket(t.Context(), "bkt", false); !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	if native.OverlayCount(h.st, "bkt", "") != 0 {
		t.Fatal("failed deletion left overlay visible")
	}
	state.set(&state.failDelete, false)
	if err := h.st.DetachBucket(t.Context(), "bkt", false); err != nil {
		t.Fatal(err)
	}
	records, err := state.Records(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for key := range records {
		if strings.HasPrefix(key, "m/") {
			t.Errorf("durable overlay survived retry: %s", key)
		}
	}
	h.open()
	if native.OverlayCount(h.st, "bkt", "") != 0 {
		t.Fatal("overlay returned after restart")
	}
}

func TestOverlaySweepRetriesHiddenDeletions(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprint("replace=", replace), func(t *testing.T) {
			h, state := newFaulty(t)
			h.mustPut("bkt", "key", []byte("old"), storage.ObjectMeta{ACL: "public-read"})
			state.set(&state.failDelete, true)
			if _, err := h.DeleteObject(t.Context(), "bkt", "key", ""); err != nil {
				t.Fatal(err)
			}
			state.set(&state.failDelete, false)
			if native.OverlayCount(h.st, "bkt", "") != 0 {
				t.Fatal("failed deletion left overlay visible")
			}
			if replace {
				h.mustPut("bkt", "key", []byte("new"), storage.ObjectMeta{ACL: "authenticated-read"})
			}
			native.SweepOverlays(h.st, t.Context())
			h.open()
			if replace {
				if got := h.info("bkt", "key", "").ACL; got != "authenticated-read" {
					t.Fatalf("retry removed replacement overlay: %q", got)
				}
			} else if native.OverlayCount(h.st, "bkt", "") != 0 {
				t.Fatal("deleted overlay returned after restart")
			}
		})
	}
}
