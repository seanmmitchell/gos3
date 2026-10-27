package s3account

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmmitchell/gos3/internal/s3client/s3clienttest"
	"github.com/seanmmitchell/gos3/internal/storage"
	"github.com/seanmmitchell/gos3/internal/storage/s3store"
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

// TestRegistryRejectsPathContainerID: a container record whose ID is a
// path (planted in the records, which a remote home does not
// authenticate) is skipped when the records load, so it never names a
// temporary directory outside Options.TempDir (N-11).
func TestRegistryRejectsPathContainerID(t *testing.T) {
	for _, withHome := range []bool{true, false} {
		name := "local"
		if withHome {
			name = "home"
		}
		t.Run(name, func(t *testing.T) {
			r := s3clienttest.Start(t)
			r.CreateBucket(t, "home")
			dir := t.TempDir()
			base := t.TempDir()
			opts := func(temp string) Options {
				var o Options
				if withHome {
					o = homeOptions(t, r, NewClaims(), "home", "gos3/")
				} else {
					o = localOptions(t, r, NewClaims(), dir)
				}
				o.TempDir = temp
				return o
			}
			ctx := context.Background()
			b := openReady(t, opts(base))
			noErr(t, "PutRecord", b.records.PutRecord(ctx, "k/x/../../ESCAPED/",
				[]byte(`{"bucket":"nonexistent-remote","role":"existing"}`)))
			noErr(t, "Close", b.Close())

			b = openReady(t, opts(filepath.Join(base, "inner")))
			escaped := filepath.Join(base, "ESCAPED")
			if _, err := os.Stat(escaped); err == nil {
				t.Fatalf("%s was created from a planted container ID", escaped)
			}
			for _, ci := range b.Containers(ctx) {
				if ci.ID != homeID {
					t.Fatalf("planted container loaded: %+v", ci)
				}
			}
			noErr(t, "Close", b.Close())
			if _, err := os.Stat(escaped); err == nil {
				t.Fatalf("%s was created from a planted container ID", escaped)
			}
		})
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

// Real-shaped IDs of the decode tests.
const (
	testContainerID = "c0123abcd"
	testBackingID   = "0123456789abcdef0123456789abcdef"
)

// TestDecodeRegistry checks exact record keys: a bucket named like the
// prefix of another, and configuration keys that are prefixes of others.
func TestDecodeRegistry(t *testing.T) {
	recs := map[string][]byte{
		bucketKey("photos"):                                   recordJSON(bucketRecord{Kind: storage.BucketKindNative, Remote: "p"}),
		bucketKey("photos2"):                                  recordJSON(bucketRecord{Kind: storage.BucketKindWrapped, Container: homeID}),
		configKey("photos", storage.ConfigEncryption):         []byte("a"),
		configKey("photos", storage.ConfigEncryptionProvider): []byte("b"),
		configKey("photos2", storage.ConfigPolicy):            []byte("orphan"),
		containerKey(testContainerID):                         recordJSON(containerRecord{Bucket: "x", BackingID: testBackingID}),
		keyDefaultContainer:                                   []byte(`{"id":"` + testContainerID + `"}`),
	}
	reg, orphans, invalid, err := decodeRegistry(recs)
	noErr(t, "decodeRegistry", err)
	if len(invalid) != 0 {
		t.Fatalf("invalid records %v", invalid)
	}
	if len(reg.buckets) != 2 || reg.buckets["photos"].Info.Name != "photos" || reg.buckets["photos2"].Kind != storage.BucketKindWrapped {
		t.Fatalf("buckets %+v", reg.buckets)
	}
	if len(reg.configs["photos"]) != 2 || string(reg.configs["photos"][storage.ConfigEncryption]) != "a" {
		t.Fatalf("configs %+v", reg.configs)
	}
	if len(orphans) != 1 || orphans[0] != configPrefix("photos2") {
		t.Fatalf("orphans %v", orphans)
	}
	if reg.containers[testContainerID].ID != testContainerID || reg.defaultContainer != testContainerID {
		t.Fatalf("containers %+v default %q", reg.containers, reg.defaultContainer)
	}
}

// TestDecodeRegistrySkipsInvalidIDs: records with a container ID, backing
// ID, bucket name or configuration key this package never writes are
// skipped and reported (N-11); the other records still load.
func TestDecodeRegistrySkipsInvalidIDs(t *testing.T) {
	good := recordJSON(containerRecord{Bucket: "x", Role: containerRoleExisting, BackingID: testBackingID})
	badIDs := map[string]string{
		"dot-dot":     "..",
		"traversal":   "x/../../ESCAPED",
		"slash":       "c0123/abcd",
		"backslash":   `c0123\abcd`,
		"backslash-2": `..\..\ESCAPED`,
		"absolute":    "/etc/passwd",
		"drive":       `C:\Windows`,
		"uppercase":   "c0123ABCD",
		"empty":       "",
		"too-long":    "c0123abcd0",
		"too-short":   "c0123abc",
		"no-c":        "d0123abcd",
		"non-hex":     "c0123abcg",
		"home-like":   "home2",
	}
	for name, id := range badIDs {
		t.Run("container/"+name, func(t *testing.T) {
			recs := map[string][]byte{
				prefixContainers + id + "/":   good,
				containerKey(testContainerID): good,
			}
			reg, _, invalid, err := decodeRegistry(recs)
			noErr(t, "decodeRegistry", err)
			if _, ok := reg.containers[id]; ok || len(invalid) != 1 {
				t.Fatalf("container ID %q: containers %v, invalid %v", id, reg.containers, invalid)
			}
			if _, ok := reg.containers[testContainerID]; !ok || len(reg.containers) != 1 {
				t.Fatalf("valid container not loaded: %v", reg.containers)
			}
		})
		if id == "" {
			continue // an empty default container is the home
		}
		t.Run("default/"+name, func(t *testing.T) {
			recs := map[string][]byte{keyDefaultContainer: recordJSON(map[string]string{"id": id})}
			reg, _, invalid, err := decodeRegistry(recs)
			noErr(t, "decodeRegistry", err)
			if reg.defaultContainer != "" || len(invalid) != 1 {
				t.Fatalf("default container %q: got %q, invalid %v", id, reg.defaultContainer, invalid)
			}
		})
	}
	for name, bid := range map[string]string{
		"empty":     "",
		"short":     "0123456789abcdef",
		"uppercase": "0123456789ABCDEF0123456789ABCDEF",
		"traversal": "../../../../../../../../ESCAPED/",
		"too-long":  testBackingID + "0",
	} {
		t.Run("backing/"+name, func(t *testing.T) {
			recs := map[string][]byte{
				containerKey(testContainerID): recordJSON(containerRecord{Bucket: "x", BackingID: bid}),
			}
			reg, _, invalid, err := decodeRegistry(recs)
			noErr(t, "decodeRegistry", err)
			if len(reg.containers) != 0 || len(invalid) != 1 {
				t.Fatalf("backing ID %q: containers %v, invalid %v", bid, reg.containers, invalid)
			}
		})
	}
	t.Run("valid", func(t *testing.T) {
		recs := map[string][]byte{
			containerKey(homeID):                  recordJSON(containerRecord{Bucket: "x"}),
			keyDefaultContainer:                   []byte(`{"id":"home"}`),
			containerKey("cdeadbeef"):             good,
			bucketKey("ok"):                       recordJSON(bucketRecord{Kind: storage.BucketKindNative, Remote: "ok"}),
			configKey("ok", storage.ConfigPolicy): []byte("{}"),
		}
		reg, orphans, invalid, err := decodeRegistry(recs)
		noErr(t, "decodeRegistry", err)
		if len(invalid) != 0 || len(orphans) != 0 || len(reg.containers) != 2 || reg.defaultContainer != homeID ||
			len(reg.buckets) != 1 || len(reg.configs["ok"]) != 1 {
			t.Fatalf("registry %+v, orphans %v, invalid %v", reg, orphans, invalid)
		}
	})
	t.Run("buckets-and-configs", func(t *testing.T) {
		recs := map[string][]byte{
			prefixBuckets + "a/b/":                                recordJSON(bucketRecord{Kind: storage.BucketKindNative, Remote: "a"}),
			prefixBuckets + "/":                                   recordJSON(bucketRecord{Kind: storage.BucketKindNative, Remote: "e"}),
			prefixBuckets + "bad\xff/":                            recordJSON(bucketRecord{Kind: storage.BucketKindNative, Remote: "f"}),
			bucketKey("nat"):                                      recordJSON(bucketRecord{Kind: storage.BucketKindNative, Remote: "nat"}),
			prefixConfigs + "nat/a/b/":                            []byte("nested key"),
			prefixConfigs + "nat/b\xff/":                          []byte("invalid key"),
			prefixConfigs + "b\xff/" + storage.ConfigPolicy + "/": []byte("invalid bucket"),
			configKey("nat", storage.ConfigPolicy):                []byte("{}"),
		}
		reg, orphans, invalid, err := decodeRegistry(recs)
		noErr(t, "decodeRegistry", err)
		if len(reg.buckets) != 1 || reg.buckets["nat"] == nil {
			t.Fatalf("buckets %v", reg.buckets)
		}
		if len(reg.configs) != 1 || len(reg.configs["nat"]) != 1 || len(orphans) != 0 {
			t.Fatalf("configs %v, orphans %v", reg.configs, orphans)
		}
		if len(invalid) != 6 {
			t.Fatalf("invalid %v, want 6", invalid)
		}
	})
}

// TestStoreOptionsRejectsPathTempName: a temporary directory name that is
// not a single local path element is refused before any directory is
// created (N-11, defense in depth).
func TestStoreOptionsRejectsPathTempName(t *testing.T) {
	r := s3clienttest.Start(t)
	base := t.TempDir()
	o := localOptions(t, r, NewClaims(), t.TempDir())
	o.TempDir = filepath.Join(base, "inner")
	b := openReady(t, o)
	conn := b.conn.Load()
	for _, name := range []string{"c-x/../../ESCAPED", `c-x\..\..\ESCAPED`, "..", "../ESCAPED", "", "/abs", `C:\abs`, "a/b"} {
		if _, err := b.storeOptions(conn, "x", "", "", s3store.RoleContainer, s3store.MustExist, testBackingID, name); err == nil {
			t.Fatalf("storeOptions accepted temporary directory name %q", name)
		}
	}
	so, err := b.storeOptions(conn, "x", "", "", s3store.RoleContainer, s3store.MustExist, testBackingID, "c-"+testContainerID)
	noErr(t, "storeOptions", err)
	if want := filepath.Join(o.TempDir, "c-"+testContainerID); so.TempDir != want {
		t.Fatalf("TempDir %q, want %q", so.TempDir, want)
	}
}
