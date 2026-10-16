package admin

import (
	"strings"
	"testing"
)

// Grants on a bucket alone do not reach a backing: disclosing a
// repository's backing S3 keys, and putting a bucket's data (snapshots, a
// replica, a move target) on a backing the caller names, also need the
// route's action on arn:gos3:admin:::backing/<name>. Before, a user scoped
// to bucket/<name> could point the bucket's snapshots at any backing and
// then read that backing's S3 keys from the credentials endpoint.

func TestResticCredentialsNeedBackingGrant(t *testing.T) {
	e, _, logs := newResticEnv(t)
	e.putPolicy("bucket-only", map[string]any{"Effect": "Allow", "Action": "admin:GetResticCredentials",
		"Resource": "arn:gos3:admin:::bucket/photos"})
	e.putPolicy("other-backing", map[string]any{"Effect": "Allow", "Action": "admin:GetResticCredentials",
		"Resource": []string{"arn:gos3:admin:::bucket/photos", "arn:gos3:admin:::backing/fast"}})
	e.putPolicy("with-backing", map[string]any{"Effect": "Allow", "Action": "admin:GetResticCredentials",
		"Resource": []string{"arn:gos3:admin:::bucket/photos", "arn:gos3:admin:::backing/default"}})
	path := Prefix + "/buckets/photos/restic/credentials"

	for _, name := range []string{"bucket-only", "other-backing"} {
		c := e.addUser(name, name)
		r := e.do(&c, "POST", path, map[string]any{})
		if msg := r.expectError(t, 403, codeAccessDenied); !strings.Contains(msg, "backing/default") {
			t.Errorf("%s: message %q", name, msg)
		}
		assertNoSecrets(t, name, r.body)
	}
	if strings.Contains(logs.String(), "disclosed") {
		t.Fatalf("refused request logged as a disclosure: %s", logs.String())
	}
	c := e.addUser("with-backing", "with-backing")
	var cr resticCredentials
	e.do(&c, "POST", path, map[string]any{}).json(t, &cr)
	if cr.Env["AWS_SECRET_ACCESS_KEY"] != testResticSecret {
		t.Fatalf("credentials = %+v", cr)
	}
}

func TestSnapshotCredentialsNeedTargetBackingGrant(t *testing.T) {
	snaps := newFakeSnapshots()
	e, _, _ := newResticEnv(t, func(d *Deps) { d.Snapshots = snaps })
	snaps.setConfig("photos", `{"enabled":true,"target":{"kind":"backing","backing":"fast","repo":"snap-1"}}`)
	e.putPolicy("bucket-only", map[string]any{"Effect": "Allow", "Action": "admin:GetResticCredentials",
		"Resource": "arn:gos3:admin:::bucket/photos"})
	e.putPolicy("with-backing", map[string]any{"Effect": "Allow", "Action": "admin:GetResticCredentials",
		"Resource": []string{"arn:gos3:admin:::bucket/photos", "arn:gos3:admin:::backing/fast"}})
	body := map[string]any{"purpose": "snapshots"}
	path := Prefix + "/buckets/photos/restic/credentials"

	c := e.addUser("bucket-only", "bucket-only")
	e.do(&c, "POST", path, body).expectError(t, 403, codeAccessDenied)
	c = e.addUser("with-backing", "with-backing")
	e.do(&c, "POST", path, body).expectStatus(t, 200)
}

// The backing that authorizes snapshot credentials is the one the snapshot
// service resolved them on, not a second reading of the configuration or
// placement, which a concurrent forced promote or configuration change can
// make differ (H-07).
func TestSnapshotCredentialsAuthorizedOnResolvedBacking(t *testing.T) {
	snaps := newFakeSnapshots()
	e, _, logs := newResticEnv(t, func(d *Deps) { d.Snapshots = snaps })
	// The configuration names fast; the credentials came from default.
	snaps.setConfig("photos", `{"enabled":true,"target":{"kind":"backing","backing":"fast","repo":"snap-1"}}`)
	snaps.mu.Lock()
	snaps.resolvedOn = "default"
	snaps.mu.Unlock()
	e.putPolicy("fast-only", map[string]any{"Effect": "Allow", "Action": "admin:GetResticCredentials",
		"Resource": []string{"arn:gos3:admin:::bucket/photos", "arn:gos3:admin:::backing/fast"}})
	path := Prefix + "/buckets/photos/restic/credentials"
	body := map[string]any{"purpose": "snapshots"}

	c := e.addUser("fast-only", "fast-only")
	r := e.do(&c, "POST", path, body)
	if msg := r.expectError(t, 403, codeAccessDenied); !strings.Contains(msg, "backing/default") {
		t.Fatalf("message %q", msg)
	}
	assertNoSecrets(t, "fast-only", r.body)
	if strings.Contains(logs.String(), "disclosed") {
		t.Fatalf("refused request logged as a disclosure: %s", logs.String())
	}

	// The audit log and the answer name the backing actually used.
	var cr resticCredentials
	e.root("POST", "/buckets/photos/restic/credentials", body).json(t, &cr)
	if cr.Backing != "default" || !strings.Contains(logs.String(), "backing=default") {
		t.Fatalf("credentials name backing %q; log: %s", cr.Backing, logs.String())
	}
}

func TestSnapshotTargetNeedsBackingGrant(t *testing.T) {
	e, snaps := newSnapshotsEnv(t)
	e.putPolicy("bucket-only", map[string]any{"Effect": "Allow", "Action": "admin:PutBucketSnapshots",
		"Resource": "arn:gos3:admin:::bucket/photos"})
	e.putPolicy("with-backing", map[string]any{"Effect": "Allow", "Action": "admin:PutBucketSnapshots",
		"Resource": []string{"arn:gos3:admin:::bucket/photos", "arn:gos3:admin:::backing/cold"}})
	path := Prefix + "/buckets/photos/restic/snapshots/config"
	onCold := map[string]any{"interval": "24h", "target": map[string]any{"kind": "backing", "backing": "cold"}}

	c := e.addUser("bucket-only", "bucket-only")
	if msg := e.do(&c, "PUT", path, onCold).expectError(t, 403, codeAccessDenied); !strings.Contains(msg, "backing/cold") {
		t.Fatalf("message %q", msg)
	}
	if _, err := snaps.Config(t.Context(), "photos"); err == nil {
		t.Fatal("a refused configuration was stored")
	}
	// The default target needs nothing beyond the bucket.
	e.do(&c, "PUT", path, map[string]any{"interval": "24h"}).expectStatus(t, 200)
	e.do(&c, "PUT", path, map[string]any{"target": "cold"}).expectError(t, 400, codeInvalidRequest)

	c = e.addUser("with-backing", "with-backing")
	e.do(&c, "PUT", path, onCold).expectStatus(t, 200)
}

func TestPlacementNeedsBackingGrant(t *testing.T) {
	e, _ := newRouterEnv(t)
	e.putPolicy("bucket-only", map[string]any{"Effect": "Allow", "Action": "admin:UpdateBucketPlacement",
		"Resource": "arn:gos3:admin:::bucket/photos"})
	e.putPolicy("with-backing", map[string]any{"Effect": "Allow", "Action": "admin:UpdateBucketPlacement",
		"Resource": []string{"arn:gos3:admin:::bucket/photos", "arn:gos3:admin:::backing/fast"}})
	addFast := map[string]any{"mode": "active-passive", "replicas": []string{"fast"}}

	c := e.addUser("bucket-only", "bucket-only")
	if msg := e.do(&c, "PUT", Prefix+"/buckets/photos/placement", addFast).expectError(t, 403, codeAccessDenied); !strings.Contains(msg, "backing/fast") {
		t.Fatalf("message %q", msg)
	}
	e.do(&c, "POST", Prefix+"/buckets/photos/placement/move", map[string]any{"to": "fast"}).expectError(t, 403, codeAccessDenied)

	w := e.addUser("with-backing", "with-backing")
	e.do(&w, "PUT", Prefix+"/buckets/photos/placement", addFast).expectStatus(t, 200)
	// A replica already in place needs nothing beyond the bucket.
	e.do(&c, "PUT", Prefix+"/buckets/photos/placement", addFast).expectStatus(t, 200)
}
