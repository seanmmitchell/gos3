package resticsnap

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seanmmitchell/gos3/internal/jobs"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// ACL guards of a source bucket: ownership controls disabling ACLs and a
// public access block ignoring public ACLs.
var (
	testOwnershipEnforced = []byte(`<OwnershipControls xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<Rule><ObjectOwnership>BucketOwnerEnforced</ObjectOwnership></Rule></OwnershipControls>`)
	testPABIgnore = []byte(`<PublicAccessBlockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
		`<IgnorePublicAcls>true</IgnorePublicAcls></PublicAccessBlockConfiguration>`)
)

// A restore carries the snapshot bucket's ACL guards to the target
// whatever "configs" says, as a copy job does: the restored objects keep
// their ACLs, which the guards kept from granting access. Before, a
// restore with configs "none" made a guarded bucket's legacy public-read
// objects public.
func TestRestoreKeepsACLGuards(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("hr", "", storage.VersioningEnabled)
	for key, data := range map[string][]byte{storage.ConfigOwnership: testOwnershipEnforced, storage.ConfigPublicAccessBlock: testPABIgnore} {
		if err := e.router.PutBucketConfig(ctx, "hr", key, data); err != nil {
			t.Fatal(err)
		}
	}
	e.put("hr", "salaries.csv", "secret", storage.ObjectMeta{ACL: "public-read"})
	e.configure("hr", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("hr")

	// An existing, empty target without guards gets them too.
	e.createBucket("plain", "", storage.VersioningEnabled)
	for i, req := range []string{
		`{"target":{"bucket":"dst-none","create":{"backing":"mem"}},"configs":"none"}`,
		`{"target":{"bucket":"dst-safe","create":{"backing":"mem"}},"configs":"safe"}`,
		`{"target":{"bucket":"plain"},"configs":"none"}`,
	} {
		j := e.restore("hr", st.Snapshot, req)
		if j.State != jobs.StateSucceeded {
			t.Fatalf("%d: restore: %s: %s", i, j.State, j.LastError)
		}
		dst := strings.Split(strings.Split(req, `"bucket":"`)[1], `"`)[0]
		for _, key := range storage.ACLGuardConfigs {
			data, err := e.router.GetBucketConfig(ctx, dst, key)
			if err != nil || !storage.ACLGuard(key, data) {
				t.Errorf("%s: %s = %q, %v; want the source's guard", dst, key, data, err)
			}
		}
		// The object keeps its ACL; the guards neutralise it.
		if _, oi := e.read(dst, "salaries.csv"); oi.ACL != "public-read" {
			t.Errorf("%s: ACL %q", dst, oi.ACL)
		}
	}
}

// A guard the target already has is left alone (it may be stricter).
func TestRestoreKeepsTargetGuard(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("hr", "", storage.VersioningEnabled)
	if err := e.router.PutBucketConfig(ctx, "hr", storage.ConfigPublicAccessBlock, testPABIgnore); err != nil {
		t.Fatal(err)
	}
	e.put("hr", "a", "x", storage.ObjectMeta{ACL: "public-read"})
	e.configure("hr", `{"enabled":false,"target":{"kind":"backing","backing":"disk"}}`)
	st := e.snapshot("hr")

	e.createBucket("guarded", "", storage.VersioningEnabled)
	strict := []byte(`<PublicAccessBlockConfiguration><BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls>` +
		`<BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	if err := e.router.PutBucketConfig(ctx, "guarded", storage.ConfigPublicAccessBlock, strict); err != nil {
		t.Fatal(err)
	}
	if j := e.restore("hr", st.Snapshot, `{"target":{"bucket":"guarded"},"configs":"none"}`); j.State != jobs.StateSucceeded {
		t.Fatalf("restore: %s: %s", j.State, j.LastError)
	}
	if data, err := e.router.GetBucketConfig(ctx, "guarded", storage.ConfigPublicAccessBlock); err != nil || string(data) != string(strict) {
		t.Fatalf("target guard replaced: %q, %v", data, err)
	}
}

// Without a readable bucket description the snapshot's guards are
// unknown, so the restore fails closed: versions are written without
// their ACLs.
func TestRestoreWithoutGuardsIsPrivate(t *testing.T) {
	e := newEnv(t, "mem", Options{Incremental: true})
	ctx := context.Background()
	e.createBucket("dst", "", storage.VersioningEnabled)
	r := &restoreRun{s: e.svc, spec: restoreSpec{Bucket: "hr", Target: restoreTarget{Bucket: "dst"}}}
	lm := time.Now().UTC().Truncate(time.Second)
	public := func(key string) *entry {
		return &entry{Key: key, VersionID: "v-" + key, LastModified: lm, Size: 1, Meta: &meta{ACL: "public-read"}}
	}

	for _, tc := range []struct {
		name string
		doc  *bucketDoc
	}{
		{"no-description", nil},
		{"unreadable-guard", &bucketDoc{Configs: map[string]configDoc{storage.ConfigOwnership: {Data: "!", Encoding: "base64"}}}},
	} {
		if err := r.applyACLGuards(ctx, tc.doc); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !r.privateACLs || len(r.status.Warnings) == 0 {
			t.Fatalf("%s: privateACLs %v, warnings %v", tc.name, r.privateACLs, r.status.Warnings)
		}
		if err := r.write(ctx, public(tc.name), strings.NewReader("x")); err != nil {
			t.Fatalf("%s: write: %v", tc.name, err)
		}
		if _, oi := e.read("dst", tc.name); oi.ACL != "" {
			t.Fatalf("%s: restored ACL %q, want private", tc.name, oi.ACL)
		}
	}

	// A readable description restores ACLs.
	if err := r.applyACLGuards(ctx, &bucketDoc{}); err != nil || r.privateACLs {
		t.Fatalf("with a description: privateACLs %v, %v", r.privateACLs, err)
	}
	if err := r.write(ctx, public("described"), strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, oi := e.read("dst", "described"); oi.ACL != "public-read" {
		t.Fatalf("restored ACL %q", oi.ACL)
	}
}
