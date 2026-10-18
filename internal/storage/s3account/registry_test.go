package s3account

import (
	"context"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
)

// TestRegistrySurvivesReload creates buckets of every kind and location,
// reopens the backing and checks that everything is still there, with a
// home and with local records.
func TestRegistrySurvivesReload(t *testing.T) {
	for _, withHome := range []bool{true, false} {
		name := "local"
		if withHome {
			name = "home"
		}
		t.Run(name, func(t *testing.T) {
			r := s3clienttest.Start(t)
			r.CreateBucket(t, "home")
			r.CreateBucket(t, "pre-existing")
			remotePut(t, r.Client(t), "pre-existing", "old.txt", "written by another tool")
			dir := t.TempDir()
			claims := NewClaims()
			opts := func() Options {
				if withHome {
					return homeOptions(t, r, claims, "home", "gos3/")
				}
				return localOptions(t, r, claims, dir)
			}
			ctx := context.Background()
			b := openReady(t, opts())
			ci, err := b.AddContainer(ctx, ContainerSpec{Bucket: "dedicated", CreateBucket: true, Label: "Team A"})
			noErr(t, "AddContainer", err)
			noErr(t, "SetDefaultContainer", b.SetDefaultContainer(ctx, ci.ID))
			if withHome {
				noErr(t, "create wh", b.CreateBucket(ctx, "wh", storage.CreateBucketOptions{Target: &storage.BucketTarget{Container: homeID}}))
				put(t, b, "wh", "a", "in the home")
			}
			noErr(t, "create wc", b.CreateBucket(ctx, "wc", storage.CreateBucketOptions{Target: &storage.BucketTarget{Kind: storage.BucketKindWrapped}, Owner: "alice"}))
			put(t, b, "wc", "b", "in the container")
			noErr(t, "create nat-created", b.CreateBucket(ctx, "nat-created", storage.CreateBucketOptions{Target: &storage.BucketTarget{Kind: storage.BucketKindNative}}))
			noErr(t, "SetBucketVersioning", b.SetBucketVersioning(ctx, "nat-created", storage.VersioningEnabled))
			noErr(t, "adopt", b.CreateBucket(ctx, "alias", storage.CreateBucketOptions{Target: &storage.BucketTarget{
				Kind: storage.BucketKindNative, RemoteBucket: "pre-existing", Adopt: true}}))
			noErr(t, "PutBucketConfig", b.PutBucketConfig(ctx, "alias", storage.ConfigPolicy, []byte(`{"policy":1}`)))
			noErr(t, "PutBucketConfig", b.PutBucketConfig(ctx, "alias", storage.ConfigEncryption, []byte(`<enc/>`)))
			noErr(t, "PutBucketConfig", b.PutBucketConfig(ctx, "alias", storage.ConfigEncryptionProvider, []byte(`{"provider":"AES256"}`)))
			noErr(t, "DeleteBucketConfig", b.DeleteBucketConfig(ctx, "alias", storage.ConfigEncryption))
			put(t, b, "nat-created", "c", "native")
			noErr(t, "Close", b.Close())

			b = openReady(t, opts())
			waitContainers(t, b)
			want := map[string]storage.BucketKind{
				"wc": storage.BucketKindWrapped, "nat-created": storage.BucketKindNative, "alias": storage.BucketKindNative,
			}
			if withHome {
				want["wh"] = storage.BucketKindWrapped
			}
			got := bucketKinds(t, b)
			if len(got) != len(want) {
				t.Fatalf("buckets after reload: %v, want %v", got, want)
			}
			for n, k := range want {
				if got[n] != k {
					t.Fatalf("bucket %s: kind %q, want %q", n, got[n], k)
				}
			}
			if withHome {
				if got := read(t, b, "wh", "a"); got != "in the home" {
					t.Fatalf("wh/a = %q", got)
				}
			}
			if got := read(t, b, "wc", "b"); got != "in the container" {
				t.Fatalf("wc/b = %q", got)
			}
			if got := read(t, b, "nat-created", "c"); got != "native" {
				t.Fatalf("nat-created/c = %q", got)
			}
			if got := read(t, b, "alias", "old.txt"); got != "written by another tool" {
				t.Fatalf("alias/old.txt = %q", got)
			}
			info, err := b.GetBucket(ctx, "nat-created")
			noErr(t, "GetBucket", err)
			if info.Versioning != storage.VersioningEnabled || info.Location != "s3://nat-created" || info.Kind != storage.BucketKindNative {
				t.Fatalf("nat-created info %+v", info)
			}
			info, err = b.GetBucket(ctx, "wc")
			noErr(t, "GetBucket", err)
			if info.Owner != "alice" || info.Location != "s3://dedicated/" {
				t.Fatalf("wc info %+v", info)
			}
			doc, err := b.GetBucketConfig(ctx, "alias", storage.ConfigPolicy)
			noErr(t, "GetBucketConfig", err)
			if string(doc) != `{"policy":1}` {
				t.Fatalf("policy %q", doc)
			}
			if _, err := b.GetBucketConfig(ctx, "alias", storage.ConfigEncryption); !strings.Contains(errString(err), "config not found") {
				t.Fatalf("deleted config: %v", err)
			}
			doc, err = b.GetBucketConfig(ctx, "alias", storage.ConfigEncryptionProvider)
			noErr(t, "GetBucketConfig(encryption-provider)", err)
			if string(doc) != `{"provider":"AES256"}` {
				t.Fatalf("encryption-provider %q", doc)
			}
			cs := b.Containers(ctx)
			wantContainers := 1
			if withHome {
				wantContainers = 2
				if cs[0].ID != homeID || cs[0].Role != containerRoleHome {
					t.Fatalf("containers %+v: the home is not first", cs)
				}
			}
			if len(cs) != wantContainers {
				t.Fatalf("containers %+v", cs)
			}
			last := cs[len(cs)-1]
			if last.ID != ci.ID || last.Label != "Team A" || !last.Default || last.Buckets != 1 || last.Role != containerRoleDedicated {
				t.Fatalf("container %+v", last)
			}
			ai := b.Info()
			if ai.NativeBuckets != 2 || ai.DefaultContainer != ci.ID {
				t.Fatalf("Info %+v", ai)
			}
		})
	}
}

// TestRegistryOrphanedConfigs: configuration records of a bucket that is
// not a native bucket are removed when the records load.
func TestRegistryOrphanedConfigs(t *testing.T) {
	r := s3clienttest.Start(t)
	dir := t.TempDir()
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), dir))
	noErr(t, "PutRecord", b.records.PutRecord(ctx, configKey("ghost", storage.ConfigPolicy), []byte("{}")))
	noErr(t, "PutRecord", b.records.PutRecord(ctx, configKey("ghost2", storage.ConfigTagging), []byte("<t/>")))
	noErr(t, "Close", b.Close())
	b = openReady(t, localOptions(t, r, NewClaims(), dir))
	recs, err := b.records.Records(ctx)
	noErr(t, "Records", err)
	for k := range recs {
		if strings.HasPrefix(k, prefixConfigs) {
			t.Fatalf("orphaned record %s survived", k)
		}
	}
	if _, ok := recs[keySchema]; !ok {
		t.Fatalf("no schema record: %v", recs)
	}
}

// TestRegistryNewerSchema: records of a newer format are refused.
func TestRegistryNewerSchema(t *testing.T) {
	r := s3clienttest.Start(t)
	dir := t.TempDir()
	ctx := context.Background()
	b := openReady(t, localOptions(t, r, NewClaims(), dir))
	noErr(t, "PutRecord", b.records.PutRecord(ctx, keySchema, []byte(`{"schema":99}`)))
	noErr(t, "Close", b.Close())
	if _, err := open(localOptions(t, r, NewClaims(), dir), testHooks()); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("open with newer records: %v", err)
	}
}

// TestDecodeRegistry checks exact record keys: a bucket named like the
// prefix of another, and configuration keys that are prefixes of others.
func TestDecodeRegistry(t *testing.T) {
	recs := map[string][]byte{
		bucketKey("photos"):                                   recordJSON(bucketRecord{Kind: storage.BucketKindNative, Remote: "p"}),
		bucketKey("photos2"):                                  recordJSON(bucketRecord{Kind: storage.BucketKindWrapped, Container: homeID}),
		configKey("photos", storage.ConfigEncryption):         []byte("a"),
		configKey("photos", storage.ConfigEncryptionProvider): []byte("b"),
		configKey("photos2", storage.ConfigPolicy):            []byte("orphan"),
		containerKey("c1"):                                    recordJSON(containerRecord{Bucket: "x", BackingID: "id"}),
		keyDefaultContainer:                                   []byte(`{"id":"c1"}`),
	}
	reg, orphans, err := decodeRegistry(recs)
	noErr(t, "decodeRegistry", err)
	if len(reg.buckets) != 2 || reg.buckets["photos"].Info.Name != "photos" || reg.buckets["photos2"].Kind != storage.BucketKindWrapped {
		t.Fatalf("buckets %+v", reg.buckets)
	}
	if len(reg.configs["photos"]) != 2 || string(reg.configs["photos"][storage.ConfigEncryption]) != "a" {
		t.Fatalf("configs %+v", reg.configs)
	}
	if len(orphans) != 1 || orphans[0] != configPrefix("photos2") {
		t.Fatalf("orphans %v", orphans)
	}
	if reg.containers["c1"].ID != "c1" || reg.defaultContainer != "c1" {
		t.Fatalf("containers %+v default %q", reg.containers, reg.defaultContainer)
	}
}
