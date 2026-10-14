package admin

import (
	"testing"

	"github.com/seanmmitchell/gos3/internal/storage"
)

func withBackings(def string, b ...Backing) envOption {
	return func(d *Deps) { d.Backings, d.DefaultBacking = b, def }
}

func TestBackings(t *testing.T) {
	gw := Backing{Name: "aws", Mode: "gateway", Endpoint: "https://s3.example.com", Region: "eu-west-1"}
	blob := Backing{Name: "cold", Mode: "blob", Endpoint: "http://minio:9000", Bucket: "gos3-data", Prefix: "p/"}
	e := newEnv(t, withBackings("cold", gw, blob))

	var list struct {
		Default  string    `json:"default"`
		Backings []Backing `json:"backings"`
	}
	e.root("GET", "/backings", nil).json(t, &list)
	if list.Default != "cold" || len(list.Backings) != 3 || list.Backings[0].Name != "local" ||
		list.Backings[1] != gw || list.Backings[2] != blob {
		t.Errorf("GET /backings = %+v", list)
	}

	// Every authenticated caller sees the choices (names and modes) in
	// /whoami, but only admin:ListBackings grants GET /backings.
	ro := e.addUser("reader", "readonly")
	e.do(&ro, "GET", Prefix+"/backings", nil).expectError(t, 403, codeAccessDenied)
	var w whoAmI
	e.do(&ro, "GET", Prefix+"/whoami", nil).json(t, &w)
	want := []backingChoice{{"local", "local"}, {"aws", "gateway"}, {"cold", "blob"}}
	if w.Backings.Default != "cold" || len(w.Backings.Available) != 3 {
		t.Fatalf("whoami backings = %+v", w.Backings)
	}
	for i := range want {
		if w.Backings.Available[i] != want[i] {
			t.Errorf("whoami backings[%d] = %+v", i, w.Backings.Available[i])
		}
	}
	if w.Capabilities.ListBackings {
		t.Error("readonly user has listBackings")
	}

	// Bucket summaries name the backing and its mode.
	e.createBucket("on-local", storage.CreateBucketOptions{})
	e.createBucket("on-aws", storage.CreateBucketOptions{Backing: "aws"})
	e.createBucket("on-gone", storage.CreateBucketOptions{Backing: "removed"})
	for name, wantB := range map[string][2]string{
		"on-local": {"local", "local"}, "on-aws": {"aws", "gateway"}, "on-gone": {"removed", ""},
	} {
		var s bucketSummary
		e.root("GET", "/buckets/"+name, nil).json(t, &s)
		if s.Backing != wantB[0] || s.BackingMode != wantB[1] {
			t.Errorf("%s: backing %q mode %q, want %q", name, s.Backing, s.BackingMode, wantB)
		}
	}
}

func TestBackingsDefaultLocal(t *testing.T) {
	e := newEnv(t)
	var w whoAmI
	e.root("GET", "/whoami", nil).json(t, &w)
	if w.Backings.Default != "local" || len(w.Backings.Available) != 1 || w.Backings.Available[0].Name != "local" {
		t.Errorf("whoami backings = %+v", w.Backings)
	}
}
