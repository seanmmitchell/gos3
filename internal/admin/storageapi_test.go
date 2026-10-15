package admin

import (
	"context"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/config"
	"github.com/seanmmitchell/gos3/internal/iam"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/memory"
	"github.com/seanmmitchell/gos3/internal/storage/multi"
)

// newRouterEnv returns a test environment whose backend is a router over
// two memory backings, "default" and "fast", with one bucket.
func newRouterEnv(t *testing.T) (*testEnv, *multi.Router) {
	t.Helper()
	r, err := multi.Open(context.Background(), multi.Options{
		Backings: []multi.Backing{
			{Name: "default", Kind: "memory", Backend: memory.New()},
			{Name: "fast", Kind: "memory", Backend: memory.New()},
		},
		HealthInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	e := newEnv(t, func(d *Deps) { d.Backend = r })
	if err := r.CreateBucket(context.Background(), "photos", storage.CreateBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	return e, r
}

func TestStorageEndpoints(t *testing.T) {
	e, r := newRouterEnv(t)

	var bl struct {
		DefaultBacking string        `json:"defaultBacking"`
		Backings       []backingJSON `json:"backings"`
	}
	e.root("GET", "/backings", nil).json(t, &bl)
	if bl.DefaultBacking != "default" || len(bl.Backings) != 2 || bl.Backings[0].Buckets != 1 || !bl.Backings[1].Healthy {
		t.Fatalf("GET /backings = %+v", bl)
	}

	var st multi.PlacementStatus
	e.root("GET", "/buckets/photos/placement", nil).json(t, &st)
	if st.Mode != multi.ModeSingle || st.Primary != "default" || len(st.Members) != 1 {
		t.Fatalf("placement = %+v", st)
	}
	e.root("GET", "/buckets/nope/placement", nil).expectError(t, 404, codeNotFound)

	// Replication.
	e.root("PUT", "/buckets/photos/placement", map[string]any{"mode": "active-active", "replicas": []string{"fast"}}).json(t, &st)
	if st.Mode != multi.ModeActiveActive || len(st.Members) != 2 || st.Members[1].Backing != "fast" {
		t.Fatalf("after PUT: %+v", st)
	}
	e.root("PUT", "/buckets/photos/placement", map[string]any{"mode": "active-active", "replicas": []string{"nope"}}).
		expectError(t, 400, codeInvalidRequest)
	e.root("PUT", "/buckets/photos/placement", map[string]any{"mode": "single", "replicas": []string{"fast"}}).
		expectError(t, 400, codeInvalidRequest)
	e.root("PUT", "/buckets/photos/placement", map[string]any{"mode": "single", "bogus": 1}).
		expectError(t, 400, codeInvalidRequest)

	// The summary reports the placement.
	var sum bucketSummary
	e.root("GET", "/buckets/photos", nil).json(t, &sum)
	if sum.Placement == nil || sum.Placement.Mode != multi.ModeActiveActive || len(sum.Placement.Replicas) != 1 {
		t.Fatalf("summary placement = %+v", sum.Placement)
	}

	// Promote, then back to single.
	waitInSync(t, r, "photos")
	e.root("POST", "/buckets/photos/placement/promote", map[string]any{"backing": "fast"}).json(t, &st)
	if st.Primary != "fast" {
		t.Fatalf("after promote: %+v", st)
	}
	e.root("POST", "/buckets/photos/placement/promote", map[string]any{}).expectError(t, 400, codeInvalidRequest)
	e.root("POST", "/buckets/photos/placement/resync", nil).expectStatus(t, 202)
	e.root("PUT", "/buckets/photos/placement", map[string]any{"mode": "single", "replicas": []string{}}).json(t, &st)

	// Move and cancel. The copy on "default" left by the switch to single
	// is deleted in the background; until then a move back there is
	// refused with a 409. (Probing with a move and a cancel instead races:
	// a move of the empty bucket can finish before the cancel.)
	waitFor(t, func() bool {
		res := e.root("POST", "/buckets/photos/placement/move", map[string]any{"to": "default"})
		if res.status != 202 && res.status != 409 {
			t.Fatalf("POST move: status %d: %s", res.status, res.body)
		}
		return res.status == 202
	})
	e.root("POST", "/buckets/photos/placement/move", map[string]any{"to": "default"}).expectError(t, 409, codeConflict)
	e.root("POST", "/buckets/photos/placement/move", map[string]any{}).expectError(t, 400, codeInvalidRequest)
	waitFor(t, func() bool {
		st, _ := r.Placement("photos")
		return st.Move == nil && st.Primary == "default"
	})
	e.root("DELETE", "/buckets/photos/placement/move", nil).expectError(t, 409, codeConflict)
}

func TestStorageEndpointsAuthorization(t *testing.T) {
	e, _ := newRouterEnv(t)
	diag := e.addUser("diag", iam.PolicyDiagnostics)
	e.do(&diag, "GET", Prefix+"/backings", nil).expectStatus(t, 200)
	e.do(&diag, "GET", Prefix+"/buckets/photos/placement", nil).expectStatus(t, 200)
	e.do(&diag, "PUT", Prefix+"/buckets/photos/placement", map[string]any{"mode": "single"}).expectError(t, 403, codeAccessDenied)
	e.do(&diag, "POST", Prefix+"/buckets/photos/placement/move", map[string]any{"to": "fast"}).expectError(t, 403, codeAccessDenied)
	rw := e.addUser("rw", iam.PolicyReadWrite)
	e.do(&rw, "GET", Prefix+"/backings", nil).expectError(t, 403, codeAccessDenied)

	// Without the router the endpoints are absent.
	plain := newEnv(t)
	plain.root("GET", "/backings", nil).expectError(t, 404, codeNotFound)
}

func TestBackingConfigurationPermissionsAndValidation(t *testing.T) {
	e, r := newRouterEnv(t)
	r.EnableBackingManagement(config.StorageConfig{Backend: "memory", Backings: []config.BackingConfig{{Name: "fast", Backend: "memory"}}}, "", multi.BackingOpener{Open: func(d config.BackingDefinition) (multi.Backing, error) {
		return multi.Backing{Name: d.Name, Kind: d.Backend, Backend: memory.New()}, nil
	}})
	var in multi.BackingConfiguration
	e.root("GET", "/backings/config", nil).json(t, &in)
	diag := e.addUser("storage-diag", iam.PolicyDiagnostics)
	e.do(&diag, "GET", Prefix+"/backings/config", nil).expectError(t, 403, codeAccessDenied)
	e.do(&diag, "PUT", Prefix+"/backings/config", in).expectError(t, 403, codeAccessDenied)
	in.Backings = append(in.Backings, config.BackingDefinition{Name: "default", Backend: "memory"})
	e.root("PUT", "/backings/config", in).expectError(t, 400, codeInvalidRequest)
	in.Backings = in.Backings[:1]
	in.DefaultBacking = "fast"
	e.root("PUT", "/backings/config", in).expectStatus(t, 200)
	e.root("PUT", "/backings/config", in).expectError(t, 409, codeConflict)
	e.root("PUT", "/backings/config", map[string]any{"bogus": true}).expectError(t, 400, codeInvalidRequest)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitInSync(t *testing.T, r *multi.Router, bucket string) {
	t.Helper()
	waitFor(t, func() bool {
		st, err := r.Placement(bucket)
		if err != nil {
			return false
		}
		for _, m := range st.Members {
			if m.State != multi.StateInSync {
				return false
			}
		}
		return true
	})
}
