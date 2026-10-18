package s3account

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

func TestClaimsRules(t *testing.T) {
	home := func(backing, bucket, prefix string) Claim {
		return Claim{Backing: backing, Location: Location{Bucket: bucket, Prefix: prefix}, Role: RoleHome}
	}
	cont := func(backing, bucket, prefix string) Claim {
		return Claim{Backing: backing, Location: Location{Bucket: bucket, Prefix: prefix}, Role: RoleContainer}
	}
	nat := func(backing, bucket, gos string) Claim {
		return Claim{Backing: backing, Location: Location{Bucket: bucket}, Role: RoleNative, GosBucket: gos}
	}
	for _, tc := range []struct {
		name     string
		first    Claim
		second   Claim
		conflict bool
	}{
		{"native vs native", nat("a", "b1", "x"), nat("b", "B1", "y"), true},
		{"native vs native same backing other bucket", nat("a", "b1", "x"), nat("a", "b1", "y"), true},
		{"native vs home", home("a", "b1", "p/"), nat("b", "b1", "x"), true},
		{"container vs native", nat("a", "b1", "x"), cont("b", "b1", "q/"), true},
		{"home vs container disjoint", home("a", "b1", "p/"), cont("a", "b1", "q/"), false},
		{"home vs container nested below", home("a", "b1", "p/"), cont("b", "b1", "p/q/"), true},
		{"container vs home nested above", cont("a", "b1", "p/q/"), home("b", "b1", "p/"), true},
		{"root nests everything", cont("a", "b1", ""), cont("b", "b1", "z/"), true},
		{"equal prefixes", cont("a", "b1", "p/"), cont("b", "b1", "p/"), true},
		{"other bucket", nat("a", "b1", "x"), nat("b", "b2", "y"), false},
		{"prefix p/ vs p2/", cont("a", "b1", "p/"), cont("a", "b1", "p2/"), false},
		{"identical claim", nat("a", "b1", "x"), nat("a", "B1", "x"), false},
		{"identical home claim", home("a", "b1", "p/"), home("a", "b1", "p/"), false},
		{"identical container claim", cont("a", "b1", "p/"), cont("a", "b1", "p/"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClaims()
			rel1, err := c.Claim(tc.first)
			noErr(t, "first claim", err)
			rel2, err := c.Claim(tc.second)
			if tc.conflict {
				if !errors.Is(err, ErrClaimed) || !errors.Is(err, storage.ErrInvalidTarget) {
					t.Fatalf("second claim: got %v, want ErrClaimed (wrapping ErrInvalidTarget)", err)
				}
				rel1()
				rel1() // idempotent
				rel2, err = c.Claim(tc.second)
				noErr(t, "claim after release", err)
				rel2()
				if hs := c.Holders("", tc.second.Bucket); len(hs) != 0 {
					t.Fatalf("holders after release: %v", hs)
				}
				return
			}
			noErr(t, "second claim", err)
			rel1()
			if hs := c.Holders("", tc.second.Bucket); len(hs) != 1 {
				t.Fatalf("holders after one release: %v", hs)
			}
			rel2()
			if hs := c.Holders("", tc.first.Bucket); len(hs) != 0 {
				t.Fatalf("holders after both releases: %v", hs)
			}
		})
	}
	if _, err := NewClaims().Claim(Claim{Role: RoleNative}); err == nil {
		t.Fatal("claim without a bucket succeeded")
	}
}

// TestClaimsPerService: claims conflict only on the same service;
// endpoints are compared as lower-case host:port with default ports.
func TestClaimsPerService(t *testing.T) {
	home := func(endpoint, backing string) Claim {
		return Claim{Backing: backing, Endpoint: endpoint, Location: Location{Bucket: "backups", Prefix: "gos3/"}, Role: RoleHome}
	}
	nat := func(endpoint, backing string) Claim {
		return Claim{Backing: backing, Endpoint: endpoint, Location: Location{Bucket: "photos"}, Role: RoleNative, GosBucket: "p"}
	}
	for _, tc := range []struct {
		name     string
		a, b     Claim
		conflict bool
	}{
		{"homes on two services", home("https://s3.amazonaws.com", "aws"), home("http://minio.lan:9000", "minio"), false},
		{"natives on two services", nat("https://s3.amazonaws.com", "aws"), nat("http://minio.lan:9000", "minio"), false},
		{"default https port", home("https://S3.amazonaws.com", "a"), home("https://s3.amazonaws.com:443/", "b"), true},
		{"default http port", nat("http://minio.lan", "a"), nat("http://MINIO.lan:80", "b"), true},
		{"other port", nat("http://minio.lan:9000", "a"), nat("http://minio.lan:9001", "b"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClaims()
			_, err := c.Claim(tc.a)
			noErr(t, "first claim", err)
			_, err = c.Claim(tc.b)
			if tc.conflict != (err != nil) {
				t.Fatalf("second claim: %v, want conflict %v", err, tc.conflict)
			}
			if tc.conflict && !errors.Is(err, ErrClaimed) {
				t.Fatalf("conflict %v does not wrap ErrClaimed", err)
			}
			if hs := c.Holders(tc.a.Endpoint, tc.a.Bucket); len(hs) != 1 || hs[0].Backing != tc.a.Backing {
				t.Fatalf("holders on the first service: %+v", hs)
			}
		})
	}
}

// TestHomesOnTwoServices: two backings whose homes have the same bucket
// and prefix on different services both open, and their native buckets
// of equal names do not conflict; on one service they still do.
func TestHomesOnTwoServices(t *testing.T) {
	r1, r2 := s3clienttest.Start(t), s3clienttest.Start(t)
	r1.CreateBucket(t, "backups")
	r2.CreateBucket(t, "backups")
	ctx := context.Background()
	claims := NewClaims()
	o1 := homeOptions(t, r1, claims, "backups", "gos3/")
	o1.Name = "one"
	o2 := homeOptions(t, r2, claims, "backups", "gos3/")
	o2.Name = "two"
	b1, b2 := openReady(t, o1), openReady(t, o2)
	noErr(t, "native on one", b1.CreateBucket(ctx, "photos", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
	noErr(t, "native on two", b2.CreateBucket(ctx, "photos", storage.CreateBucketOptions{Target: nativeTarget("", false)}))
	if hs := claims.Holders(r1.URL, "photos"); len(hs) != 1 || hs[0].Backing != "one" {
		t.Fatalf("holders on the first service: %+v", hs)
	}
	// The same service again: still claimed.
	o3 := homeOptions(t, r1, claims, "backups", "gos3/inner/")
	o3.Name = "three"
	if _, err := open(o3, testHooks()); !errors.Is(err, ErrClaimed) {
		t.Fatalf("nested home on the same service: %v", err)
	}
	_, err := b1.CheckAdoptable(ctx, "photos")
	wantErr(t, "CheckAdoptable on the same service", err, ErrClaimed)
}
